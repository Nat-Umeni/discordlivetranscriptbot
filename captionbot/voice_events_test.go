package captionbot

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/sessions"
)

func (env *testEnv) voiceEvent(userID snowflake.ID) *events.GenericGuildVoiceState {
	return &events.GenericGuildVoiceState{
		GenericEvent: events.NewGenericEvent(env.client, 0, 0),
		VoiceState:   discord.VoiceState{GuildID: serverID, UserID: userID},
	}
}

func TestBotDisconnectedEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	env.bot.OnBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	env.noSession(t)
	if !conn.closed {
		t.Error("the dropped connection should be closed, or the next start would get it back")
	}
	statusCleared(t, env) // tried, at least: Discord may refuse once the bot is out
	if env.voice.GetConn(serverID) != nil {
		t.Error("the connection should be removed from the manager")
	}
}

func TestBotDisconnectedWithNoConnectionLeftStillEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	env.bot.sessions.Start(serverID, sessions.Session{StartedBy: callerID})

	env.bot.OnBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	env.noSession(t)
}

func TestBotMovedEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	env.bot.OnBotMovedVoice(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(botID)})

	env.noSession(t)
	if !conn.closed {
		t.Error("the bot should leave the channel it was dragged into")
	}
	statusCleared(t, env) // the call it was captioning, not the one it was dragged into
}

func TestOtherPeopleLeavingOrMovingIsIgnored(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	env.bot.OnBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(otherMemberID)})
	env.bot.OnBotMovedVoice(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(otherMemberID)})

	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Error("someone else leaving must not end the session")
	}
	if conn.closed {
		t.Error("someone else leaving must not close the bot's connection")
	}
}

func TestBotLeavingWithNoSessionDoesNothing(t *testing.T) {
	// what happens right after /transcribe stop: the session is already gone
	env := newTestEnv(t)
	conn := env.voice.connectBot(serverID, meetingChannelID)

	env.bot.OnBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	if conn.closed {
		t.Error("with no session the leave was ours; nothing to clean up")
	}
}
