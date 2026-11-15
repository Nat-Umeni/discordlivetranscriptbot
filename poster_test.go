package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// fakeThread stands in for Discord's side of the captions thread. It keeps
// each message's current text so tests can check what people would see, logs
// every call, and can be told to fail or to hang (like a rate-limited
// Discord) until let go.
type fakeThread struct {
	rest.Rest

	mu       sync.Mutex
	messages map[snowflake.ID]string
	order    []snowflake.ID // message IDs, oldest first
	calls    []threadCall
	nextID   snowflake.ID

	failCreates, failEdits int           // the next n creates / edits fail
	gate                   chan struct{} // if set, calls wait until it's closed
	waiting                int           // calls currently waiting at the gate
}

type threadCall struct {
	edit     bool
	content  string
	mentions *discord.AllowedMentions
}

func newFakeThread() *fakeThread {
	return &fakeThread{messages: map[snowflake.ID]string{}, nextID: 100}
}

func (f *fakeThread) waitAtGate() {
	f.mu.Lock()
	gate := f.gate
	if gate != nil {
		f.waiting++
	}
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (f *fakeThread) CreateMessage(channelID snowflake.ID, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.waitAtGate()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, threadCall{edit: false, content: m.Content, mentions: m.AllowedMentions})
	if f.failCreates > 0 {
		f.failCreates--
		return nil, errors.New("discord said no")
	}
	f.nextID++
	f.messages[f.nextID] = m.Content
	f.order = append(f.order, f.nextID)
	return &discord.Message{ID: f.nextID, ChannelID: channelID}, nil
}

func (f *fakeThread) UpdateMessage(channelID, messageID snowflake.ID, m discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.waitAtGate()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, threadCall{edit: true, content: *m.Content, mentions: m.AllowedMentions})
	if f.failEdits > 0 {
		f.failEdits--
		return nil, errors.New("discord said no")
	}
	f.messages[messageID] = *m.Content
	return &discord.Message{ID: messageID, ChannelID: channelID}, nil
}

// shown is what the thread shows, oldest message first.
func (f *fakeThread) shown() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, id := range f.order {
		texts = append(texts, f.messages[id])
	}
	return texts
}

func (f *fakeThread) callCount() (creates, edits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.edit {
			edits++
		} else {
			creates++
		}
	}
	return creates, edits
}

func (f *fakeThread) waitingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waiting
}

func testNames(userID snowflake.ID) string {
	return map[snowflake.ID]string{1: "Sam", 2: "Jo"}[userID]
}

func newTestPoster(t *testing.T, thread *fakeThread) *threadPoster {
	t.Helper()
	p := newThreadPoster(thread, threadID, testNames, nil)
	t.Cleanup(func() { finish(t, p) }) // safe if the test already finished it
	return p
}

// finish ends the captions and waits for the poster to send everything.
func finish(t *testing.T, p *threadPoster) {
	t.Helper()
	p.closed()
	select {
	case <-p.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the poster never finished")
	}
}

// waitUntil checks something the poster's goroutine does in its own time.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantShown(t *testing.T, thread *fakeThread, want ...string) {
	t.Helper()
	got := thread.shown()
	if len(got) != len(want) {
		t.Fatalf("thread shows %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: shows %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPosterGrowsACaptionThenFinishesIt(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)

	p.caption(1, "so I", false)
	waitUntil(t, "the caption is posted", func() bool { return len(thread.shown()) == 1 })
	wantShown(t, thread, "**Sam:** so I\n-# speaking…")

	p.caption(1, "so I rang", false)
	p.caption(1, "So I rang him.", true)
	finish(t, p)

	// Still one message: it grew, then lost its "speaking…" line.
	wantShown(t, thread, "**Sam:** So I rang him.")
}

func TestPosterCoalescesRapidPartials(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread) // edits paced at the real 1 s

	p.caption(1, "w0", false)
	waitUntil(t, "the caption is posted", func() bool { return len(thread.shown()) == 1 })
	text := "w0"
	for i := 1; i <= 50; i++ {
		text += fmt.Sprintf(" w%d", i)
		p.caption(1, text, false)
	}
	p.caption(1, text+".", true)
	finish(t, p)

	wantShown(t, thread, "**Sam:** "+text+".")
	if creates, edits := thread.callCount(); creates != 1 || edits > 2 {
		t.Errorf("50 quick updates should be squashed into a couple of edits, got %d creates and %d edits", creates, edits)
	}
}

// While Discord makes the bot wait, newer text replaces what's waiting, so only
// the newest version goes out once Discord is ready.
func TestPosterSendsOnlyTheNewestTextWhenDiscordIsSlow(t *testing.T) {
	thread := newFakeThread()
	thread.gate = make(chan struct{})
	p := newTestPoster(t, thread)

	p.caption(1, "a", false)
	waitUntil(t, "the post is stuck waiting on Discord", func() bool { return thread.waitingCount() == 1 })
	p.caption(1, "a b", false)
	p.caption(1, "a b c", false)
	p.caption(1, "A b c d.", true)

	close(thread.gate) // Discord catches up
	finish(t, p)

	wantShown(t, thread, "**Sam:** A b c d.")
	if creates, edits := thread.callCount(); creates != 1 || edits != 1 {
		t.Errorf("want the stuck post and then one edit with the newest text, got %d creates and %d edits", creates, edits)
	}
}

func TestPosterKeepsSpeakersApartInTheOrderTheyStarted(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)

	p.caption(1, "so", false)  // Sam starts first
	p.caption(2, "hey", false) // Jo talks over him
	p.caption(2, "Hey you.", true)
	p.caption(1, "So anyway.", true)
	finish(t, p)

	wantShown(t, thread, "**Sam:** So anyway.", "**Jo:** Hey you.")
}

func TestPosterStartsANewMessageForTheNextSentence(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)

	p.caption(1, "First thing.", true)
	p.caption(1, "second", false)
	p.caption(1, "Second thing.", true)
	finish(t, p)

	wantShown(t, thread, "**Sam:** First thing.", "**Sam:** Second thing.")
}

func TestPosterNotCaptionedNotice(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)

	p.notCaptioned(2)
	finish(t, p)

	wantShown(t, thread, "*[Jo isn't being captioned]*")
}

// When the call ends mid-pause, the newest text still goes out, without
// waiting for the edit pacing.
func TestPosterClosedSendsWhatsLeftAtOnce(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)
	p.editEvery = time.Hour // a normal edit would never be due in this test

	p.caption(1, "a", false)
	waitUntil(t, "the caption is posted", func() bool { return len(thread.shown()) == 1 })
	p.caption(1, "a b", false)
	finish(t, p)

	wantShown(t, thread, "**Sam:** a b\n-# speaking…")
}

func TestPosterPingsNobody(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)

	p.caption(1, "hey @everyone", false)
	waitUntil(t, "the caption is posted", func() bool { return len(thread.shown()) == 1 })
	p.caption(1, "Hey @everyone.", true)
	finish(t, p)

	thread.mu.Lock()
	defer thread.mu.Unlock()
	if len(thread.calls) != 2 {
		t.Fatalf("want a post and an edit, got %d calls", len(thread.calls))
	}
	for _, call := range thread.calls {
		if call.mentions == nil || call.mentions.Parse == nil || len(call.mentions.Parse) != 0 {
			t.Errorf("%q (edit=%v) must not ping anyone, got %+v", call.content, call.edit, call.mentions)
		}
	}
}

func TestPosterRetriesAFailedEdit(t *testing.T) {
	thread := newFakeThread()
	thread.failEdits = 1
	p := newTestPoster(t, thread)
	p.editEvery = 10 * time.Millisecond

	p.caption(1, "so", false)
	waitUntil(t, "the caption is posted", func() bool { return len(thread.shown()) == 1 })
	p.caption(1, "So there.", true)
	waitUntil(t, "the edit is retried", func() bool { return thread.shown()[0] == "**Sam:** So there." })
}

func TestPosterGivesUpOnABrokenCaptionWithoutBlockingOthers(t *testing.T) {
	logs := captureLog(t)
	thread := newFakeThread()
	thread.failCreates = maxCaptionFailures
	p := newTestPoster(t, thread)
	p.editEvery = 10 * time.Millisecond

	p.caption(1, "lost", false)
	waitUntil(t, "the caption is given up on", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.queue) == 0
	})
	if !strings.Contains(logs.String(), "giving up on a caption for 1") {
		t.Errorf("giving up should be logged, logged:\n%s", logs)
	}

	// Sam's next words start a fresh message rather than updating the lost one,
	// and Jo is unaffected.
	p.caption(1, "Back again.", true)
	p.caption(2, "Hi.", true)
	finish(t, p)

	wantShown(t, thread, "**Sam:** Back again.", "**Jo:** Hi.")
}

func TestCaptionMessage(t *testing.T) {
	tests := []struct {
		why           string
		name, text    string
		final, notice bool
		want          string
	}{
		{"still talking", "Sam", "so I rang", false, false, "**Sam:** so I rang\n-# speaking…"},
		{"finished", "Sam", "So I rang him.", true, false, "**Sam:** So I rang him."},
		{"swearing as said", "Sam", "Fuck that.", true, false, "**Sam:** Fuck that."},
		{"formatting characters in a name show as typed", "xX_Sam_Xx", "hi", true, false, `**xX\_Sam\_Xx:** hi`},
		{"and in the text", "Sam", `a*b ~c~ |d| \e`, true, false, `**Sam:** a\*b \~c\~ \|d\| \\e`},
		{"opt-out notice", "Jo_", "", true, true, `*[Jo\_ isn't being captioned]*`},
	}
	for _, test := range tests {
		if got := captionMessage(test.name, test.text, test.final, test.notice); got != test.want {
			t.Errorf("%s: got %q, want %q", test.why, got, test.want)
		}
	}

	// Discord refuses messages over 2000 characters.
	long := captionMessage("Sam", strings.Repeat("word ", 1000), false, false)
	if n := utf8.RuneCountInString(long); n > 2000 {
		t.Errorf("a long caption is %d characters, over Discord's 2000", n)
	}
	if !strings.HasSuffix(long, "…"+speakingLine) {
		t.Errorf("a long caption should be cut with … and keep its speaking line, ends %q", long[len(long)-40:])
	}
}

func TestWaitForPosters(t *testing.T) {
	var running sync.WaitGroup
	p := newThreadPoster(newFakeThread(), threadID, testNames, &running)

	if waitForPosters(&running, 20*time.Millisecond) {
		t.Error("a poster that hasn't been closed hasn't finished")
	}
	p.closed()
	if !waitForPosters(&running, 5*time.Second) {
		t.Error("a closed poster should finish")
	}
}

// The transcriber calls in from disgo's goroutines while the worker sends. Run
// with -race: without the lock this fails.
func TestPosterConcurrentCallers(t *testing.T) {
	thread := newFakeThread()
	p := newTestPoster(t, thread)
	p.editEvery = time.Millisecond

	var wg sync.WaitGroup
	for user := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := snowflake.ID(100 + user)
			text := ""
			for i := range 30 {
				text += fmt.Sprintf(" w%d", i)
				p.caption(id, text, false)
			}
			p.caption(id, "Done.", true)
		}()
	}
	wg.Wait()
	finish(t, p)

	shown := thread.shown()
	if len(shown) != 8 {
		t.Fatalf("want one message per speaker, got %d", len(shown))
	}
	for _, message := range shown {
		if !strings.HasSuffix(message, ":** Done.") {
			t.Errorf("every caption should end finished, got %q", message)
		}
	}
}

// The whole path with real speech: the model's sample recording, as Discord
// packets, through the transcriber and the poster into the thread.
func TestSpeechEndsUpAsOneFinishedMessageInTheThread(t *testing.T) {
	recogniser := testRecogniser(t) // skips if the model isn't downloaded
	thread := newFakeThread()
	poster := newThreadPoster(thread, threadID, testNames, nil)
	tr := newTranscriber(recogniser, poster, nobodyOptedOut)

	for _, data := range speechPackets(t, sampleWAV("0.wav")) {
		if err := tr.ReceiveOpusFrame(1, &voice.Packet{SSRC: 7, Opus: data}); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close() // the call ends mid-pause: the last sentence is finished and sent
	select {
	case <-poster.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the poster never finished")
	}

	shown := thread.shown()
	if len(shown) != 1 {
		t.Fatalf("want the sentence as one message, got %q", shown)
	}
	t.Logf("thread shows: %s", shown[0])
	if !strings.HasPrefix(shown[0], "**Sam:** ") || strings.Contains(shown[0], "speaking…") {
		t.Errorf("want a finished caption under Sam's name, got %q", shown[0])
	}
	containsAll(t, shown[0], "AFTER EARLY NIGHTFALL", "YELLOW LAMPS", "BROTHELS")
}
