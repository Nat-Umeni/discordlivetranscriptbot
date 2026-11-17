package captionbot

import (
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
	"livetranscribebot/sessions"
)

func TestShutDownClearsStatusesLeavesCallsAndWaitsForCaptions(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	// A poster still sending when shutdown starts: it finishes once the call
	// is left (in the real bot, leaving closes the transcriber, which closes
	// its poster).
	poster := captions.NewThreadPoster(env.rest, threadID, func(snowflake.ID) string { return "Sam" }, &env.bot.posters)
	conn.onClose = poster.Closed

	env.bot.ShutDown(env.client)

	statusCleared(t, env)
	if !conn.closed {
		t.Error("shutting down should leave every call")
	}
	select {
	case <-poster.Done():
	default:
		t.Error("shutting down should wait for posters to finish before closing the client")
	}
}

func TestShutDownGivesUpWaitingOnAStuckPoster(t *testing.T) {
	logs := captureLog(t)
	env := newTestEnv(t)
	env.bot.shutdownWait = 20 * time.Millisecond
	poster := captions.NewThreadPoster(env.rest, threadID, func(snowflake.ID) string { return "Sam" }, &env.bot.posters)
	defer poster.Closed()

	env.bot.ShutDown(env.client) // the poster is never closed

	if !strings.Contains(logs.String(), "some captions were still being posted when I shut down") {
		t.Errorf("a stuck poster should be logged, not wait forever, logged:\n%s", logs)
	}
}

func TestShutdownClearsEveryCallsStatus(t *testing.T) {
	env := newTestEnv(t)
	const otherServer, otherCall snowflake.ID = 1001, 5100
	env.bot.sessions.Start(serverID, sessions.Session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "A"})
	env.bot.sessions.Start(otherServer, sessions.Session{StartedBy: callerID, ChannelID: otherCall, Title: "B"})

	env.bot.ShutDown(env.client)

	cleared := map[snowflake.ID]bool{}
	for _, status := range env.rest.statuses {
		if status.message.Content != "" {
			t.Errorf("shutting down should only clear statuses, got %q", status.message.Content)
		}
		cleared[status.channelID] = true
	}
	if len(cleared) != 2 || !cleared[meetingChannelID] || !cleared[otherCall] {
		t.Errorf("want both calls' statuses cleared, cleared %v", cleared)
	}
}

func TestShutDownGivesUpWaitingOnAStuckTranscript(t *testing.T) {
	logs := captureLog(t)
	env := newTestEnv(t)
	env.bot.shutdownWait = 20 * time.Millisecond
	env.bot.transcripts.Add(1) // a transcript that never finishes
	defer env.bot.transcripts.Done()

	env.bot.ShutDown(env.client)

	if !strings.Contains(logs.String(), "some transcripts were still being posted when I shut down") {
		t.Errorf("a stuck transcript should be logged, not wait forever, logged:\n%s", logs)
	}
}

func TestRunning(t *testing.T) {
	env := newTestEnv(t)
	if env.bot.Running() {
		t.Error("nothing should be running yet")
	}

	env.startCaptions(t)
	if !env.bot.Running() {
		t.Error("captions are running")
	}

	event, _ := env.command(t, moderator, stopCommand())
	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	if env.bot.Running() {
		t.Error("captions have stopped")
	}
}
