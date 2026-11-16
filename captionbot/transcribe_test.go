package captionbot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
	"livetranscribebot/sessions"
	"livetranscribebot/speech"
)

func TestStartJoinsTheCallersChannel(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, startCommand("Weekly Meetup"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	onlyReply(t, sent, "I'm joining "+discord.ChannelMention(meetingChannelID))
	if len(env.voice.created) != 1 {
		t.Fatalf("want one voice connection, got %d", len(env.voice.created))
	}
	conn := env.voice.created[0]
	if !conn.opened || conn.openedChannel != meetingChannelID {
		t.Fatalf("should open the caller's channel, opened=%v channel=%v", conn.opened, conn.openedChannel)
	}
	if !conn.selfMute || conn.selfDeaf {
		t.Errorf("should join muted but NOT deafened (deafened = no audio), got mute=%v deaf=%v", conn.selfMute, conn.selfDeaf)
	}
	// No model is loaded in handler tests (recogniser is nil). That's fine:
	// the transcriber only touches it once someone talks.
	tr, ok := conn.receiver.(*speech.Transcriber)
	if !ok {
		t.Fatalf("after joining it should start captioning with a transcriber, got %T", conn.receiver)
	}
	defer tr.Close() // stops its quiet timer; the real disgo does this when the call ends

	current, running := env.bot.sessions.Get(serverID)
	want := sessions.Session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "Weekly Meetup"}
	if !running || current != want {
		t.Fatalf("session: got (%+v, %v), want (%+v, true)", current, running, want)
	}

	// Opted-out speakers are skipped: the no-captions role is checked. (Sam,
	// the caller, is in the call, so they're cached like everyone in voice.)
	env.client.Caches.AddMember(discord.Member{GuildID: serverID,
		User: discord.User{ID: callerID, Username: "sam123", GlobalName: strPtr("Sam")}})
	env.client.Caches.AddMember(discord.Member{GuildID: serverID, RoleIDs: []snowflake.ID{noCaptionsRoleID},
		User: discord.User{ID: otherMemberID, Username: "jo"}})
	optedOut := env.bot.optedOutOfCaptions(env.client.Caches, serverID)
	if !optedOut(otherMemberID) || optedOut(callerID) {
		t.Error("only members with the no-captions role should be skipped")
	}

	// First, a notice in the call's own chat: captioned, where, how to opt
	// out. It pings nobody.
	if len(env.rest.posted) != 2 {
		t.Fatalf("want a notice in the call and an opening post in the captions channel, got %+v", env.rest.posted)
	}
	notice := env.rest.posted[0]
	if notice.channelID != meetingChannelID {
		t.Errorf("the notice should go in the call's chat first, went to %v", notice.channelID)
	}
	for _, want := range []string{"being captioned live", discord.ChannelMention(captionsChannelID), "never recorded", discord.RoleMention(noCaptionsRoleID)} {
		if !strings.Contains(notice.message.Content, want) {
			t.Errorf("notice %q should contain %q", notice.message.Content, want)
		}
	}
	noPingsIn(t, notice.message)

	// Then the captions thread, standing on its own in the captions channel
	// (not hanging off a message, which would show the post twice).
	if len(env.rest.threads) != 1 || env.rest.threads[0].channelID != captionsChannelID {
		t.Fatalf("want one thread in the captions channel, got %+v", env.rest.threads)
	}
	threadCreate, ok := env.rest.threads[0].create.(discord.GuildPublicThreadCreate)
	if !ok {
		t.Fatalf("want a public thread, got %T", env.rest.threads[0].create)
	}
	if !strings.HasPrefix(threadCreate.Name, "📝 Weekly Meetup - ") {
		t.Errorf("thread name %q", threadCreate.Name)
	}

	// The opening post goes inside the thread.
	intro := env.rest.posted[1]
	if intro.channelID != threadID {
		t.Errorf("the opening post should go inside the thread, went to %v", intro.channelID)
	}
	for _, want := range []string{"Weekly Meetup", discord.ChannelMention(meetingChannelID), discord.UserMention(callerID),
		"never recorded", discord.RoleMention(noCaptionsRoleID)} {
		if !strings.Contains(intro.message.Content, want) {
			t.Errorf("opening post %q should contain %q", intro.message.Content, want)
		}
	}
	noPingsIn(t, intro.message)

	// Once the thread exists, the notice is edited to link straight to it.
	if len(env.rest.edits) != 1 {
		t.Fatalf("want the notice edited once to link the thread, got %+v", env.rest.edits)
	}
	edit := env.rest.edits[0]
	if edit.channelID != meetingChannelID || edit.messageID != noticeMessageID ||
		!strings.Contains(edit.message.Content, "Captions are in "+discord.ChannelMention(threadID)) {
		t.Errorf("the notice should now say the captions are in the thread, got %+v", edit)
	}

	// Whoever started the captions is added to the thread, and only them.
	if len(env.rest.threadMembers) != 1 || env.rest.threadMembers[0].channelID != threadID || env.rest.threadMembers[0].messageID != callerID {
		t.Errorf("want only the starter added to the thread, got %+v", env.rest.threadMembers)
	}

	// The call's status shows captions are on.
	if len(env.rest.statuses) != 1 || env.rest.statuses[0].channelID != meetingChannelID ||
		env.rest.statuses[0].message.Content != "🔴 Live captions: Weekly Meetup" {
		t.Errorf("want the call's status set to live captions, got %+v", env.rest.statuses)
	}

	// The caller gets a hidden link to the thread.
	if len(env.rest.followups) != 1 || !strings.Contains(env.rest.followups[0].Content, "Captions are live in "+discord.ChannelMention(threadID)) {
		t.Fatalf("want a follow-up linking the thread, got %+v", env.rest.followups)
	}
	if !env.rest.followups[0].Flags.Has(discord.MessageFlagEphemeral) {
		t.Error("the link should be hidden too")
	}

	// Captions go to the terminal and the new thread, under the speaker's name
	// in this server. Checked last: the poster talks to the fake Discord from
	// its own goroutine, so env.rest is only read again once it has finished.
	tr.Close() // the call ends: its poster sends what's left and stops

	logs := captureLog(t)
	out, _ := env.bot.captionOutputs(env.client, serverID, threadID)
	out.Caption(callerID, "hello everyone", true)
	out.Closed()
	if !captions.WaitForPosters(&env.bot.posters, 5*time.Second) {
		t.Fatal("the posters never finished")
	}

	if !strings.Contains(logs.String(), "Sam: hello everyone") {
		t.Errorf("the terminal should log the finished caption under the name, logged:\n%s", logs)
	}
	last := env.rest.posted[len(env.rest.posted)-1]
	if last.channelID != threadID || last.message.Content != "**Sam:** hello everyone" {
		t.Errorf("want the caption posted in the thread as **Sam:** hello everyone, got %+v", last)
	}
}

// noPingsIn checks a message notifies nobody, even though it mentions people,
// roles and channels.
func noPingsIn(t *testing.T, message discord.MessageCreate) {
	t.Helper()
	mentions := message.AllowedMentions
	if mentions == nil || mentions.Parse == nil || len(mentions.Parse) != 0 {
		t.Errorf("%q must not ping anyone, got %+v", message.Content, mentions)
	}
}

func TestStartRefusesUntilSetupIsComplete(t *testing.T) {
	tests := []struct {
		name             string
		channel, noCapts bool
		want             string
	}{
		{"nothing set", false, false, "pick a captions channel (captions-channel) and a role for people who don't want to be captioned (no-captions-role) with /captions-setup."},
		{"no captions channel", false, true, "pick a captions channel (captions-channel) with /captions-setup."},
		{"no no-captions role", true, false, "pick a role for people who don't want to be captioned (no-captions-role) with /captions-setup."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.clearSettings(t)
			if test.channel {
				_ = env.bot.settings.SetCaptionsChannel(serverID, captionsChannelID)
			}
			if test.noCapts {
				_ = env.bot.settings.SetNoCaptionsRole(serverID, noCaptionsRoleID)
			}
			event, sent := env.command(t, moderator, startCommand("x"))

			if err := env.bot.onTranscribeStart(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, "Before I can caption here, an admin needs to "+test.want)
			env.noSession(t)
			if len(env.voice.created) != 0 {
				t.Error("should not join before setup is complete")
			}
		})
	}
}

func TestStartWhenTheBotCantSetTheCallsStatus(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: callerID, ChannelID: idPtr(noStatusChannelID)})
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I don't have permission to join "+discord.ChannelMention(noStatusChannelID)+
		", ask an admin to give me View Channel, Connect, Send Messages and Set Voice Channel Status there")
	env.noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not join a call it can't announce captions in")
	}
}

func TestStartWhenTheBotCantPostCaptions(t *testing.T) {
	env := newTestEnv(t)
	_ = env.bot.settings.SetCaptionsChannel(serverID, lockedCaptionsChannelID) // permissions changed since setup
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't post captions in "+discord.ChannelMention(lockedCaptionsChannelID))
	env.noSession(t) // the claim is given back
	if len(env.voice.created) != 0 {
		t.Error("should not join when it can't post captions")
	}
}

func TestStartWhenTheCaptionsChannelIsGone(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveChannel(captionsChannelID) // deleted since setup
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I can post in the captions channel. If it's been deleted, pick another with /captions-setup captions-channel.")
	env.noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not join when it can't check the captions channel")
	}
}

// leftAgainAfterAnnouncingFailed checks a start that joined but couldn't tell
// people about the captions: it left voice, said so, never listened, set no
// status, and gave the claim back.
func leftAgainAfterAnnouncingFailed(t *testing.T, env *testEnv) {
	t.Helper()
	if len(env.rest.followups) != 1 || !strings.Contains(env.rest.followups[0].Content,
		"I couldn't post the captions notice in "+discord.ChannelMention(meetingChannelID)+"'s chat and the thread in "+
			discord.ChannelMention(captionsChannelID)+", so I've left again.") {
		t.Fatalf("want one 'couldn't post the captions notice' follow-up, got %+v", env.rest.followups)
	}
	if !env.rest.followups[0].Flags.Has(discord.MessageFlagEphemeral) {
		t.Error("the follow-up should be hidden")
	}
	conn := env.voice.created[0]
	if !conn.opened || !conn.closed {
		t.Errorf("should have joined then left again, opened=%v closed=%v", conn.opened, conn.closed)
	}
	if conn.receiver != nil {
		t.Error("must not listen when people couldn't be told")
	}
	if len(env.rest.statuses) != 0 {
		t.Errorf("no status should be set, got %+v", env.rest.statuses)
	}
	env.noSession(t)
}

func TestStartWhenTheNoticeFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.postErrIn = map[snowflake.ID]error{meetingChannelID: errors.New("discord unreachable")}
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("the failure is reported to the caller, not returned: %v", err)
	}
	leftAgainAfterAnnouncingFailed(t, env)
	if len(env.rest.threads) != 0 || len(env.rest.posted) != 1 {
		t.Error("nothing else should be posted when the notice fails")
	}
}

func TestStartWhenTheThreadFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.threadErr = errors.New("missing permissions")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("the failure is reported to the caller, not returned: %v", err)
	}
	leftAgainAfterAnnouncingFailed(t, env)
	if len(env.rest.posted) != 1 {
		t.Error("no opening post without a thread to put it in")
	}
	wantDeleted(t, env, postedMessage{channelID: meetingChannelID, messageID: noticeMessageID})
}

func TestStartWhenTheOpeningPostFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.postErrIn = map[snowflake.ID]error{threadID: errors.New("missing permissions")}
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("the failure is reported to the caller, not returned: %v", err)
	}
	leftAgainAfterAnnouncingFailed(t, env)
	if len(env.rest.deletedThreads) != 1 || env.rest.deletedThreads[0] != threadID {
		t.Errorf("the empty thread should be deleted, deleted %v", env.rest.deletedThreads)
	}
	wantDeleted(t, env, postedMessage{channelID: meetingChannelID, messageID: noticeMessageID})
}

// wantDeleted checks exactly these messages were deleted, in this order.
func wantDeleted(t *testing.T, env *testEnv, want ...postedMessage) {
	t.Helper()
	if len(env.rest.deleted) != len(want) {
		t.Fatalf("deleted %+v, want %+v", env.rest.deleted, want)
	}
	for i, got := range env.rest.deleted {
		if got.channelID != want[i].channelID || got.messageID != want[i].messageID {
			t.Errorf("deletion %d: got message %v in %v, want message %v in %v",
				i, got.messageID, got.channelID, want[i].messageID, want[i].channelID)
		}
	}
}

// Linking the notice to the thread and adding the starter only make captions
// easier to find: if they fail, captions still start.
func TestStartStillCaptionsWhenFindingTheThreadIsHarder(t *testing.T) {
	logs := captureLog(t)
	env := newTestEnv(t)
	env.rest.editErr = errors.New("discord unreachable")
	env.rest.addMemberErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("these failures aren't worth stopping captions for: %v", err)
	}
	stillCaptioning(t, env)
	for _, want := range []string{"couldn't link the call notice to the captions thread", "couldn't add " + callerID.String() + " to the captions thread"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, logged:\n%s", want, logs)
		}
	}
}

func TestStartStillCaptionsWhenTheStatusFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.statusErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("the notice already told people, so a missing status isn't worth stopping for: %v", err)
	}
	stillCaptioning(t, env)
}

func TestStartStillCaptionsWhenTheLinkFollowupFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.followupErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("a missing link isn't worth stopping captions for: %v", err)
	}
	stillCaptioning(t, env)
}

func stillCaptioning(t *testing.T, env *testEnv) {
	t.Helper()
	conn := env.voice.created[0]
	tr, ok := conn.receiver.(*speech.Transcriber)
	if !ok {
		t.Fatalf("captions should still start, receiver is %T", conn.receiver)
	}
	tr.Close()
	if conn.closed {
		t.Error("should stay in voice")
	}
	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Error("the session should still be running")
	}
}

func TestStartAllowsNoteTakers(t *testing.T) {
	env := newTestEnv(t)
	_ = env.bot.settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	event, sent := env.command(t, noteTaker, startCommand("Notes"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm joining")
}

func TestStartInDMIsRefused(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
	env.noSession(t)
}

func TestStartRefusesNonNoteTakersAndHintsAtSetup(t *testing.T) {
	env := newTestEnv(t) // no note-taker role set yet
	event, sent := env.command(t, plainMember, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Only note-takers and moderators can start captions here. An admin can choose the note-taker role with /captions-setup.")
	env.noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not touch voice when refusing")
	}
}

func TestStartRefusesMembersWithoutTheRoleOnceItIsSet(t *testing.T) {
	env := newTestEnv(t)
	_ = env.bot.settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	someoneElse := caller{id: callerID, roles: []snowflake.ID{9999}}
	event, sent := env.command(t, someoneElse, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Only note-takers and moderators can start captions here.")
	if strings.Contains(reply.Content, "/captions-setup") {
		t.Error("the setup hint is only for when no role has been chosen yet")
	}
}

func TestStartWhenCallerIsNotInVoice(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveVoiceState(serverID, callerID)
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't see you in a chat")
	env.noSession(t)
}

func TestStartWhileAlreadyTranscribingLeavesTheBotWhereItIs(t *testing.T) {
	env := newTestEnv(t)
	running := sessions.Session{StartedBy: otherMemberID, ChannelID: lockedChannelID, Title: "Earlier"}
	env.bot.sessions.Start(serverID, running)
	event, sent := env.command(t, moderator, startCommand("Later"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm already transcribing 'Earlier' in "+discord.ChannelMention(lockedChannelID))
	if len(env.voice.created) != 0 {
		t.Error("must not create or move a voice connection")
	}
	current, _ := env.bot.sessions.Get(serverID)
	if current != running {
		t.Errorf("the running session must be untouched, got %+v", current)
	}
}

func TestStartWhenBotMemberIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveMember(serverID, botID)
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I'm allowed to join")
	env.noSession(t) // the claim is given back
}

func TestStartWhenChannelIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveChannel(meetingChannelID)
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I'm allowed to join")
	env.noSession(t)
}

func TestStartWhenBotLacksConnect(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: callerID, ChannelID: idPtr(lockedChannelID)})
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I don't have permission to join "+discord.ChannelMention(lockedChannelID))
	env.noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not try to join a channel it can't connect to")
	}
}

func TestStartWhenTheReplyFails(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, startCommand("x"))
	sent.err = errors.New("discord unreachable")

	if err := env.bot.onTranscribeStart(event); err == nil {
		t.Fatal("the reply's error should be returned so middleware.Go logs it")
	}
	env.noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not join after failing to reply")
	}
}

func TestStartWhenJoiningFails(t *testing.T) {
	env := newTestEnv(t)
	env.voice.openErr = context.DeadlineExceeded
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err != nil {
		t.Fatalf("a failed join is reported to the caller, not returned: %v", err)
	}
	onlyReply(t, sent, "I'm joining")
	if len(env.rest.followups) != 1 || !strings.Contains(env.rest.followups[0].Content, "I couldn't join") {
		t.Fatalf("want one 'I couldn't join' follow-up, got %+v", env.rest.followups)
	}
	if !env.rest.followups[0].Flags.Has(discord.MessageFlagEphemeral) {
		t.Error("the follow-up should be hidden too")
	}
	if !env.voice.created[0].closed {
		t.Error("the failed connection should be closed")
	}
	if env.voice.created[0].receiver != nil {
		t.Error("a failed join should not start listening")
	}
	env.noSession(t) // the claim is given back so start can be tried again
}

func TestStartWhenJoiningFailsAndTheFollowupFails(t *testing.T) {
	env := newTestEnv(t)
	env.voice.openErr = context.DeadlineExceeded
	env.rest.followupErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := env.bot.onTranscribeStart(event); err == nil {
		t.Fatal("the follow-up's error should be returned")
	}
	env.noSession(t)
}

// ---------- /transcribe stop ----------

// startedSession puts a session and a connected bot in place, as a successful start would.
func (env *testEnv) startedSession(startedBy snowflake.ID) *fakeConn {
	env.bot.sessions.Start(serverID, sessions.Session{StartedBy: startedBy, ChannelID: meetingChannelID, Title: "Weekly"})
	return env.voice.connectBot(serverID, meetingChannelID)
}

func TestStopByTheStarterEvenWithoutPermissions(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)
	event, sent := env.command(t, plainMember, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving "+discord.ChannelMention(meetingChannelID)+" and stopping the captions for 'Weekly'")
	if !conn.closed {
		t.Error("should leave voice")
	}
	statusCleared(t, env)
	env.noSession(t)
}

// statusCleared checks the "live captions" status was taken off the call.
func statusCleared(t *testing.T, env *testEnv) {
	t.Helper()
	if len(env.rest.statuses) != 1 || env.rest.statuses[0].channelID != meetingChannelID || env.rest.statuses[0].message.Content != "" {
		t.Errorf("want the call's status cleared, got %+v", env.rest.statuses)
	}
}

func TestStopStillLeavesWhenClearingTheStatusFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.statusErr = errors.New("discord unreachable")
	conn := env.startedSession(callerID)
	event, _ := env.command(t, plainMember, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	if !conn.closed {
		t.Error("a status that won't clear mustn't keep the bot in the call")
	}
	env.noSession(t)
}

func TestStopByANoteTaker(t *testing.T) {
	env := newTestEnv(t)
	_ = env.bot.settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	conn := env.startedSession(otherMemberID)
	event, sent := env.command(t, noteTaker, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	if !conn.closed {
		t.Error("should leave voice")
	}
	env.noSession(t)
}

func TestStopByAModerator(t *testing.T) {
	env := newTestEnv(t)
	env.startedSession(otherMemberID)
	event, sent := env.command(t, moderator, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	env.noSession(t)
}

func TestStopRefusesSomeoneElse(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(otherMemberID)
	event, sent := env.command(t, plainMember, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Only the person who started these captions, note-takers or moderators can stop them.")
	if conn.closed {
		t.Error("must not leave voice")
	}
	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Error("the session must keep running")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm not transcribing in this server right now.")
}

func TestStopInDM(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
}

func TestStopWhenTheBotIsAlreadyOutOfVoice(t *testing.T) {
	env := newTestEnv(t)
	env.bot.sessions.Start(serverID, sessions.Session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "Weekly"})
	event, sent := env.command(t, moderator, stopCommand())

	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	env.noSession(t) // nothing to close, but the session is still cleared
}

func TestStopWhenTheReplyFails(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)
	event, sent := env.command(t, moderator, stopCommand())
	sent.err = errors.New("discord unreachable")

	if err := env.bot.onTranscribeStop(event); err == nil {
		t.Fatal("the reply's error should be returned")
	}
	if conn.closed {
		t.Error("should not leave without telling the caller")
	}
	if _, running := env.bot.sessions.Get(serverID); !running {
		t.Error("the session should keep running")
	}
}
