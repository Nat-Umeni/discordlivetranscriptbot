package captionbot

import (
	"strings"
	"testing"

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
	if testing.Short() {
		t.Skip("waits the full 5 s shutdown timeout")
	}
	logs := captureLog(t)
	env := newTestEnv(t)
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

	env.bot.clearStatusesOnShutdown(env.client)

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
