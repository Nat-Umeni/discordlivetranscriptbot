package captionbot

import (
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// startCaptions runs /transcribe start as a moderator sitting in
// meetingChannelID, and returns the call it starts.
func (env *testEnv) startCaptions(t *testing.T) *liveCall {
	t.Helper()
	event, _ := env.command(t, moderator, startCommand("Weekly Meetup"))
	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	env.bot.callsMu.Lock()
	defer env.bot.callsMu.Unlock()
	call, found := env.bot.calls[serverID]
	if !found {
		t.Fatal("starting captions should remember the call")
	}
	return call
}

// waitForPosters waits until every poster has sent everything and every
// transcript is posted, so the fake Discord can be read safely.
func (env *testEnv) waitForPosters(t *testing.T) {
	t.Helper()
	if !captions.WaitForPosters(&env.bot.posters, 5*time.Second) {
		t.Fatal("the posters never finished")
	}
	if !captions.WaitForPosters(&env.bot.transcripts, 5*time.Second) {
		t.Fatal("the transcripts never finished")
	}
}

// threadMessages is everything posted in the captions thread, oldest first.
func (env *testEnv) threadMessages() []string {
	var texts []string
	for _, posted := range env.rest.posted {
		if posted.channelID == threadID {
			texts = append(texts, posted.message.Content)
		}
	}
	return texts
}

// sessionEnded waits until the server's session is gone: endings from the
// empty-call timer happen on the timer's own goroutine.
func (env *testEnv) sessionEnded(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, running := env.bot.sessions.Get(serverID); !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantEndedWith checks a call ended tidily: the closing line comes after the
// last caption, then the transcript; the call's notice says captions have
// ended, the status is cleared, the bot left and the session is gone.
func wantEndedWith(t *testing.T, env *testEnv, closingLine string) {
	t.Helper()
	env.sessionEnded(t)
	env.waitForPosters(t)

	messages := env.threadMessages()
	if len(messages) < 2 || messages[len(messages)-2] != closingLine || messages[len(messages)-1] != transcriptHeading {
		t.Errorf("the thread should end with %q then the transcript, got %q", closingLine, messages)
	}

	ended := false
	for _, edit := range env.rest.edits {
		if edit.channelID == meetingChannelID && edit.messageID == noticeMessageID && edit.message.Content == endedNotice(threadID) {
			ended = true
		}
	}
	if !ended {
		t.Errorf("the call notice should be edited to say captions have ended, edits %+v", env.rest.edits)
	}

	last := env.rest.statuses[len(env.rest.statuses)-1]
	if last.channelID != meetingChannelID || last.message.Content != "" {
		t.Errorf("the call's status should be cleared last, got %+v", env.rest.statuses)
	}
	if !env.voice.created[0].closed {
		t.Error("the bot should leave the call")
	}
	env.noSession(t)
}

func TestEndedNotice(t *testing.T) {
	if got, want := endedNotice(threadID), "⏹️ **Captions for this call have ended.** They're in "+discord.ChannelMention(threadID)+"."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := stoppedByLine("xX_Sam_Xx"), `⏹️ **Captions stopped** by xX\_Sam\_Xx.`; got != want {
		t.Errorf("names should show as typed: got %q, want %q", got, want)
	}
}

// ---------- each way captions end ----------

func TestStopEndsWithAClosingLineAfterTheLastCaption(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.AddMember(discord.Member{GuildID: serverID,
		User: discord.User{ID: callerID, Username: "sam123", GlobalName: strPtr("Sam")}})
	call := env.startCaptions(t)
	call.poster.Caption(callerID, "Last thing.", true) // still being posted as the call ends

	event, _ := env.command(t, moderator, stopCommand())
	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}

	wantEndedWith(t, env, "⏹️ **Captions stopped** by caller.")
	messages := env.threadMessages()
	if len(messages) < 3 || messages[len(messages)-3] != "**Sam:** Last thing." {
		t.Errorf("the last caption should come just before the closing line, thread %q", messages)
	}
}

func TestDisconnectEndsWithAClosingLine(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.bot.OnVoiceLeave(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	wantEndedWith(t, env, disconnectedLine)
}

func TestMoveEndsWithAClosingLine(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.bot.OnVoiceMove(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(botID)})

	wantEndedWith(t, env, movedLine)
}

func TestShutDownEndsWithAClosingLine(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.bot.ShutDown(env.client)

	wantEndedWith(t, env, shutdownLine)
}

func TestEndingStillLeavesWhenMarkingTheNoticeFails(t *testing.T) {
	logs := captureLog(t)
	env := newTestEnv(t)
	env.startCaptions(t)
	env.rest.mu.Lock()
	env.rest.editErr = errorString("discord unreachable")
	env.rest.mu.Unlock()

	event, _ := env.command(t, moderator, stopCommand())
	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}

	env.waitForPosters(t)
	if !strings.Contains(logs.String(), "couldn't mark the call notice as ended") {
		t.Errorf("the failed edit should be logged, logged:\n%s", logs)
	}
	if !env.voice.created[0].closed {
		t.Error("a notice that won't update mustn't keep the bot in the call")
	}
	env.noSession(t)
}

// ---------- an empty call ----------

// leave takes someone out of the call (in disgo's cache, as Discord's update
// would) and sends the leave event.
func (env *testEnv) leave(userID snowflake.ID) {
	env.client.Caches.RemoveVoiceState(serverID, userID)
	env.bot.OnVoiceLeave(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(userID)})
}

// inCall puts someone in a voice channel in disgo's cache, without an event.
func (env *testEnv) inCall(userID, channelID snowflake.ID) {
	env.client.Caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: userID, ChannelID: idPtr(channelID)})
}

func (env *testEnv) stillRunning(t *testing.T, after time.Duration) {
	t.Helper()
	time.Sleep(after)
	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Fatal("captions should still be running")
	}
}

func TestEmptyCallEndsAfterTheWait(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 20 * time.Millisecond
	env.startCaptions(t)

	env.leave(callerID) // the last person goes

	wantEndedWith(t, env, emptyCallLine)
}

func TestSomeoneRejoiningCancelsTheCountdown(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 100 * time.Millisecond
	env.startCaptions(t)

	env.leave(callerID)
	env.inCall(callerID, meetingChannelID)
	env.bot.OnVoiceJoin(&events.GuildVoiceJoin{GenericGuildVoiceState: env.voiceEvent(callerID)})

	env.stillRunning(t, 300*time.Millisecond)
}

func TestMovingOutOfTheCallEmptiesIt(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 20 * time.Millisecond
	env.startCaptions(t)

	env.inCall(callerID, lockedChannelID)
	env.bot.OnVoiceMove(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(callerID)})

	wantEndedWith(t, env, emptyCallLine)
}

func TestBotsDontCountAsPeople(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 20 * time.Millisecond
	env.startCaptions(t)
	const musicBot snowflake.ID = 2100
	env.client.Caches.AddMember(discord.Member{GuildID: serverID, User: discord.User{ID: musicBot, Bot: true}})
	env.inCall(musicBot, meetingChannelID)
	env.inCall(botID, meetingChannelID)                                                  // this bot, in its own call
	env.inCall(otherMemberID, lockedChannelID)                                           // someone in a different call
	env.client.Caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: 3200}) // no channel at all

	env.leave(callerID)

	wantEndedWith(t, env, emptyCallLine)
}

// Someone whose member info isn't cached can't be checked for being a bot, so
// they count as a person: captions never stop on someone who's still there.
func TestSomeoneNotCachedCountsAsAPerson(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 20 * time.Millisecond
	env.startCaptions(t)
	env.inCall(3100, meetingChannelID)

	env.leave(callerID)

	env.stillRunning(t, 100*time.Millisecond)
}

// If a join was missed, the count when the countdown finishes finds them and
// captions carry on.
func TestCountdownCountsAgainBeforeEnding(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = 50 * time.Millisecond
	env.startCaptions(t)

	env.leave(callerID)
	env.inCall(callerID, meetingChannelID) // back, but no join event arrived

	env.stillRunning(t, 200*time.Millisecond)
}

func TestAnotherLeaveDoesntRestartTheCountdown(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = time.Hour
	call := env.startCaptions(t)

	env.leave(callerID)
	env.bot.callsMu.Lock()
	first := call.emptyCountdown
	env.bot.callsMu.Unlock()

	env.leave(otherMemberID) // someone else leaving another call in the server
	env.bot.callsMu.Lock()
	again := call.emptyCountdown
	env.bot.callsMu.Unlock()

	if first == nil || again != first {
		t.Error("an empty call should count down once, not restart on every event")
	}
}

func TestStoppingCancelsTheCountdown(t *testing.T) {
	env := newTestEnv(t)
	env.bot.emptyCallWait = time.Hour
	call := env.startCaptions(t)
	env.leave(callerID)

	event, _ := env.command(t, moderator, stopCommand())
	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}

	env.bot.callsMu.Lock()
	defer env.bot.callsMu.Unlock()
	if call.emptyCountdown != nil {
		t.Error("stopping should cancel the countdown")
	}
}

// A countdown that's been cancelled or replaced does nothing when it fires, and
// ending a session that's already gone does nothing either.
func TestStaleEndingsDoNothing(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.bot.endIfStillEmpty(env.client, serverID, &emptyCountdown{})
	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Fatal("an out-of-date countdown mustn't end captions")
	}

	env.bot.endSession(env.client, 9999, "⏹️ nothing running here")
	if len(env.voice.created) != 1 || env.voice.created[0].closed {
		t.Error("ending a server with no session shouldn't touch anything")
	}
}

// errorString is a plain error for tests.
type errorString string

func (e errorString) Error() string { return string(e) }
