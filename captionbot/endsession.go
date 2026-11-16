package captionbot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// emptyCallTimeout is how long a call can have nobody in it (bots don't count)
// before its captions stop by themselves. Long enough to survive someone's
// connection dropping for a moment, short enough that the bot doesn't sit in
// an empty call with "Live captions" showing.
const emptyCallTimeout = time.Minute

// The closing lines posted in the thread after the last caption, so readers
// can tell "captions ended" from "everyone went quiet".
const (
	disconnectedLine = "⏹️ **Captions stopped:** I was disconnected from the call."
	movedLine        = "⏹️ **Captions stopped:** I was moved out of the call."
	emptyCallLine    = "⏹️ **Captions stopped:** everyone left the call."
	shutdownLine     = "⏹️ **Captions stopped:** I'm shutting down."
)

// stoppedByLine is the closing line when someone runs /transcribe stop.
func stoppedByLine(name string) string {
	return fmt.Sprintf("⏹️ **Captions stopped** by %s.", captions.EscapeMarkdown(name))
}

// liveCall is what the bot keeps about a call it's captioning, so it can end
// it tidily: the thread's poster (for the closing line), the call's notice
// (to mark it ended) and the countdown that runs while the call is empty.
type liveCall struct {
	poster         *captions.ThreadPoster
	voiceChannelID snowflake.ID
	threadID       snowflake.ID
	threadName     string // the transcript's title
	noticeID       snowflake.ID
	emptyCountdown *emptyCountdown // nil unless the call is empty
}

// emptyCountdown is one run of the "nobody's here" timer. Each run is its own
// value, so a timer that fires after being replaced or cancelled can tell it's
// out of date.
type emptyCountdown struct {
	timer *time.Timer
}

// addCall remembers a call once captions are running in it.
func (b *Bot) addCall(serverID snowflake.ID, call *liveCall) {
	b.callsMu.Lock()
	defer b.callsMu.Unlock()
	b.calls[serverID] = call
}

// takeCall forgets a server's call and stops its countdown, returning it (nil
// if there wasn't one).
func (b *Bot) takeCall(serverID snowflake.ID) *liveCall {
	b.callsMu.Lock()
	defer b.callsMu.Unlock()
	call, found := b.calls[serverID]
	if !found {
		return nil
	}
	delete(b.calls, serverID)
	if call.emptyCountdown != nil {
		call.emptyCountdown.timer.Stop()
		call.emptyCountdown = nil
	}
	return call
}

// endSession ends a server's captions, whatever the reason: /transcribe stop,
// the bot being disconnected or moved, an empty call, or shutting down.
//  1. hand the poster its closing line, posted after the last caption
//  2. mark the call's notice as ended, so nobody reading it later thinks
//     they're still being captioned
//  3. clear the call's status
//  4. leave the call: that closes the transcriber, which finishes everyone's
//     last sentence and closes the poster, which posts them then the line;
//     once that's done the transcript files are posted (in the background)
//  5. forget the session, last, so a new start can't slip in while leaving
//
// Steps 2 and 3 are best effort: if Discord refuses, it's logged and the bot
// still leaves.
func (b *Bot) endSession(client *bot.Client, serverID snowflake.ID, closingLine string) {
	current, running := b.sessions.Get(serverID)
	if !running {
		return
	}

	if call := b.takeCall(serverID); call != nil {
		call.poster.SignOff(closingLine)
		markNoticeEnded(client, call.voiceChannelID, call.noticeID, call.threadID)
		b.transcriptWhenPosted(client, call)
	}

	// While the bot is still in the call: setting a call's status needs it
	// connected.
	clearVoiceStatus(client, current.ChannelID)

	// GetConn is nil if the bot is already out of voice (someone disconnected
	// it). Otherwise Close leaves, and also removes disgo's connection, which
	// CreateConn would otherwise hand to the next /transcribe start.
	if voiceConnection := client.VoiceManager.GetConn(serverID); voiceConnection != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		voiceConnection.Close(ctx)
	}

	b.sessions.Remove(serverID)
}

// checkEmpty starts the countdown to ending a call's captions when nobody's
// left in it, and cancels it when someone's there again. It runs whenever
// anyone joins, leaves or moves.
func (b *Bot) checkEmpty(client *bot.Client, serverID snowflake.ID) {
	b.callsMu.Lock()
	defer b.callsMu.Unlock()

	call, found := b.calls[serverID]
	if !found {
		return
	}

	if peopleInCall(client.Caches, serverID, call.voiceChannelID, client.ID()) > 0 {
		if call.emptyCountdown != nil {
			call.emptyCountdown.timer.Stop()
			call.emptyCountdown = nil
		}
		return
	}

	if call.emptyCountdown != nil {
		return // already counting down
	}
	// The timer can't use countdown until this function unlocks callsMu, by
	// which time countdown.timer is set.
	countdown := &emptyCountdown{}
	countdown.timer = time.AfterFunc(b.emptyCallWait, func() { b.endIfStillEmpty(client, serverID, countdown) })
	call.emptyCountdown = countdown
}

// endIfStillEmpty runs when a countdown finishes. It counts again first: if
// this countdown was cancelled or replaced, or someone's in the call after all
// (a join the bot missed), captions carry on.
func (b *Bot) endIfStillEmpty(client *bot.Client, serverID snowflake.ID, countdown *emptyCountdown) {
	b.callsMu.Lock()
	call, found := b.calls[serverID]
	current := found && call.emptyCountdown == countdown
	if current {
		call.emptyCountdown = nil
	}
	empty := current && peopleInCall(client.Caches, serverID, call.voiceChannelID, client.ID()) == 0
	b.callsMu.Unlock()

	if empty {
		log.Printf("nobody's been in the call in server %s for %s, ending that session", serverID, b.emptyCallWait)
		b.endSession(client, serverID, emptyCallLine)
	}
}

// peopleInCall counts the people in a voice channel from disgo's voice-state
// cache. The bot itself and other bots don't count. Someone whose member info
// isn't cached counts as a person, so captions never stop on someone who's
// still there.
func peopleInCall(caches cache.Caches, serverID, channelID, selfID snowflake.ID) int {
	people := 0
	for state := range caches.VoiceStates(serverID) {
		if state.ChannelID == nil || *state.ChannelID != channelID || state.UserID == selfID {
			continue
		}
		if member, found := caches.Member(serverID, state.UserID); found && member.User.Bot {
			continue
		}
		people++
	}
	return people
}
