package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

const (
	// How often one caption message is edited while someone's still talking.
	// Discord allows about 5 edits every 5 seconds in a channel, shared by
	// everyone talking, so editing on every new word would hit that straight
	// away and captions would fall behind.
	captionEditEvery = time.Second

	// How often the poster looks for edits that have become due.
	posterCheckEvery = 250 * time.Millisecond

	// Discord allows 2000 characters per message; this leaves room for the
	// name and the "speaking…" line.
	maxCaptionTextLength = 1900

	// A caption that fails this many times is given up on, so one broken
	// message can't hold up everyone else's.
	maxCaptionFailures = 3

	// Shown under a caption while the person is still talking. "-#" is
	// Discord's small grey text.
	speakingLine = "\n-# speaking…"
)

// openPosters counts posters that are still sending, so shutdown can wait for
// the last captions to reach Discord before it closes the connection.
var openPosters sync.WaitGroup

// liveCaption is one message in the captions thread: a sentence someone is
// saying or has said, or a one-off "isn't being captioned" notice.
type liveCaption struct {
	userID snowflake.ID
	text   string // the newest text
	final  bool   // the sentence has ended
	notice bool   // a one-off notice rather than a caption

	messageID snowflake.ID // 0 until it's been posted
	sentText  string       // what Discord is showing now
	sentFinal bool
	lastSent  time.Time // when it was last posted or edited, or last failed
	failures  int
}

// threadPoster posts captions in the captions thread and edits them as they
// grow. The transcriber hands it text while holding its lock, so caption and
// notCaptioned only note the newest text and return; a worker goroutine does
// the slow part, talking to Discord, in the background.
//
// While Discord is slow or making the bot wait (rate limits), newer text just
// replaces what's waiting, so only the latest version is ever sent and the
// captions catch up instead of falling further behind. Finished sentences are
// never skipped.
type threadPoster struct {
	rest      rest.Rest
	threadID  snowflake.ID
	nameFor   nameFunc
	editEvery time.Duration // captionEditEvery; tests shorten it

	mu    sync.Mutex
	queue []*liveCaption                // oldest first: the order people started talking
	open  map[snowflake.ID]*liveCaption // each speaker's unfinished sentence

	wake      chan struct{} // nudges the worker when there's new text
	done      chan struct{} // closed when the transcriber has finished
	closeOnce sync.Once
	finished  chan struct{} // closed once the worker has sent everything and stopped
}

// newThreadPoster starts a poster for one call's captions thread. running, if
// not nil, counts it until it has sent everything after closed.
func newThreadPoster(restClient rest.Rest, threadID snowflake.ID, nameFor nameFunc, running *sync.WaitGroup) *threadPoster {
	p := &threadPoster{
		rest:      restClient,
		threadID:  threadID,
		nameFor:   nameFor,
		editEvery: captionEditEvery,
		open:      make(map[snowflake.ID]*liveCaption),
		// Room for one nudge: if one is already waiting, the worker will see
		// this text too, so a second isn't needed.
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	if running != nil {
		running.Add(1)
	}
	go p.run(running)
	return p
}

func (p *threadPoster) caption(userID snowflake.ID, text string, final bool) {
	p.mu.Lock()
	c, found := p.open[userID]
	if !found {
		// Their first words since their last sentence ended: a new message.
		c = &liveCaption{userID: userID}
		p.queue = append(p.queue, c)
		p.open[userID] = c
	}
	c.text = text
	if final {
		c.final = true
		delete(p.open, userID) // their next words start a new message
	}
	p.mu.Unlock()

	p.nudge()
}

func (p *threadPoster) notCaptioned(userID snowflake.ID) {
	p.mu.Lock()
	p.queue = append(p.queue, &liveCaption{userID: userID, notice: true, final: true})
	p.mu.Unlock()

	p.nudge()
}

// closed tells the worker to send whatever's left and stop.
func (p *threadPoster) closed() {
	p.closeOnce.Do(func() { close(p.done) })
}

// nudge wakes the worker without ever waiting: the caller may be holding the
// transcriber's lock.
func (p *threadPoster) nudge() {
	select {
	case p.wake <- struct{}{}:
	default: // a nudge is already waiting
	}
}

// run is the worker: it sends whatever's due whenever there's new text, and
// every posterCheckEvery for paced edits.
func (p *threadPoster) run(running *sync.WaitGroup) {
	defer func() {
		close(p.finished)
		if running != nil {
			running.Done()
		}
	}()

	ticker := time.NewTicker(posterCheckEvery)
	defer ticker.Stop()

	for {
		p.sendDue(false)
		select {
		case <-p.wake:
		case <-ticker.C:
		case <-p.done:
			// Captions are over: send everything left, without waiting for
			// the edit pacing, then stop.
			p.sendDue(true)
			return
		}
	}
}

// sendDue sends everything that's due, oldest caption first. force ignores the
// edit pacing.
func (p *threadPoster) sendDue(force bool) {
	for {
		c, snapshot, found := p.nextDue(force)
		if !found {
			return
		}
		p.send(c, snapshot)
	}
}

// captionSnapshot is what's about to be sent for a caption. Text that arrives
// while it's being sent isn't marked as sent: it goes out next time.
type captionSnapshot struct {
	text      string
	final     bool
	messageID snowflake.ID
}

// nextDue finds the oldest caption with something to send.
func (p *threadPoster) nextDue(force bool) (*liveCaption, captionSnapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.forgetFinished()
	now := time.Now()
	for _, c := range p.queue {
		if p.isDue(c, now, force) {
			return c, captionSnapshot{text: c.text, final: c.final, messageID: c.messageID}, true
		}
	}
	return nil, captionSnapshot{}, false
}

// isDue says whether a caption has something to send now. Called with p.mu
// held.
func (p *threadPoster) isDue(c *liveCaption, now time.Time, force bool) bool {
	paced := now.Sub(c.lastSent) >= p.editEvery

	// After a failure, wait a moment before trying again, unless it's the
	// final send-off.
	if c.failures > 0 && !force && !paced {
		return false
	}
	if c.messageID == 0 {
		return true // not posted yet
	}
	if c.text == c.sentText && c.final == c.sentFinal {
		return false // Discord already shows this
	}
	// A finished sentence goes out straight away; one still being spoken is
	// edited at most once every editEvery.
	return c.final || force || paced
}

// forgetFinished drops captions with nothing left to send: finished sentences
// Discord already shows in full, and ones given up on. Called with p.mu held.
func (p *threadPoster) forgetFinished() {
	kept := p.queue[:0]
	for _, c := range p.queue {
		fullySent := c.messageID != 0 && c.sentFinal && c.sentText == c.text
		givenUp := c.failures >= maxCaptionFailures
		if givenUp {
			log.Printf("giving up on a caption for %s after %d failed tries", c.userID, c.failures)
			// Their next words start a fresh message rather than updating
			// this lost one.
			if p.open[c.userID] == c {
				delete(p.open, c.userID)
			}
		}
		if !fullySent && !givenUp {
			kept = append(kept, c)
		}
	}
	// Clear the dropped tail so the old captions can be freed.
	clear(p.queue[len(kept):])
	p.queue = kept
}

// send posts or edits one caption. It talks to Discord without holding p.mu,
// so the transcriber is never kept waiting.
func (p *threadPoster) send(c *liveCaption, snapshot captionSnapshot) {
	content := captionMessage(p.nameFor(c.userID), snapshot.text, snapshot.final, c.notice)

	messageID := snapshot.messageID
	var err error
	if messageID == 0 {
		var message *discord.Message
		message, err = p.rest.CreateMessage(p.threadID, discord.MessageCreate{Content: content, AllowedMentions: noPings})
		if err == nil {
			messageID = message.ID
		}
	} else {
		_, err = p.rest.UpdateMessage(p.threadID, messageID, discord.MessageUpdate{Content: &content, AllowedMentions: noPings})
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	c.lastSent = time.Now()
	if err != nil {
		c.failures++
		log.Printf("couldn't post a caption in the thread (try %d of %d): %v", c.failures, maxCaptionFailures, err)
		return
	}
	c.failures = 0
	c.messageID = messageID
	c.sentText, c.sentFinal = snapshot.text, snapshot.final
}

// captionMessage is what a caption looks like in the thread:
//
//	**Sam:** so I rang him
//	-# speaking…
//
// with the "speaking…" line gone once the sentence has ended.
func captionMessage(name, text string, final, notice bool) string {
	if notice {
		return fmt.Sprintf("*[%s isn't being captioned]*", escapeMarkdown(name))
	}

	message := fmt.Sprintf("**%s:** %s", escapeMarkdown(name), cutToLength(escapeMarkdown(text), maxCaptionTextLength))
	if !final {
		message += speakingLine
	}
	return message
}

// markdownEscaper puts a backslash before the characters Discord treats as
// formatting, so they show as typed. Backslash itself goes first, or the
// backslashes added for the others would be escaped too.
var markdownEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, `|`, `\|`, "`", "\\`")

// escapeMarkdown stops a name like xX_Sam_Xx turning into italics, or a
// caption's symbols changing how the message looks.
func escapeMarkdown(text string) string {
	return markdownEscaper.Replace(text)
}

// waitForPosters waits for every poster counted in running to finish, or for
// the timeout, and says whether they all finished.
func waitForPosters(running *sync.WaitGroup, timeout time.Duration) bool {
	allDone := make(chan struct{})
	go func() {
		running.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		return true
	case <-time.After(timeout):
		return false
	}
}
