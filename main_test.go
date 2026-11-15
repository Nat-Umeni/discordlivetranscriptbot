package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// These tests run the real command handlers without Discord. disgo builds a
// command from the same JSON Discord sends, so the tests do too; replies,
// follow-ups and the voice connection are fakes that record what happened.

// IDs for the fake server. Any numbers work; they just have to be consistent.
const (
	serverID         snowflake.ID = 1000
	botID            snowflake.ID = 2000
	callerID         snowflake.ID = 3000
	otherMemberID    snowflake.ID = 3001
	noteTakerRoleID  snowflake.ID = 4000
	meetingChannelID snowflake.ID = 5000 // the bot can join this one
	lockedChannelID  snowflake.ID = 5001 // @everyone is denied Connect here
)

func TestMain(m *testing.M) {
	// The handlers log failures on purpose in many tests; keep the output readable.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// ---------- fakes ----------

// fakeConn stands in for a voice connection. Embedding voice.Conn satisfies the
// interface; only the methods below are used by the code under test, and any
// other method would panic, which would show up as a failing test.
type fakeConn struct {
	voice.Conn
	manager   *fakeVoiceManager
	guildID   snowflake.ID
	channelID *snowflake.ID
	openErr   error

	opened             bool
	openedChannel      snowflake.ID
	selfMute, selfDeaf bool
	closed             bool
	receiver           voice.OpusFrameReceiver // what the handler set up to listen with
}

func (c *fakeConn) SetOpusFrameReceiver(receiver voice.OpusFrameReceiver) {
	c.receiver = receiver
}

func (c *fakeConn) Open(_ context.Context, channelID snowflake.ID, selfMute bool, selfDeaf bool) error {
	c.opened = true
	c.openedChannel, c.selfMute, c.selfDeaf = channelID, selfMute, selfDeaf
	if c.openErr != nil {
		return c.openErr
	}
	c.channelID = &channelID
	return nil
}

// Close mirrors disgo: it leaves voice and removes the connection from the manager.
func (c *fakeConn) Close(context.Context) {
	c.closed = true
	c.channelID = nil
	delete(c.manager.conns, c.guildID)
}

func (c *fakeConn) ChannelID() *snowflake.ID { return c.channelID }

type fakeVoiceManager struct {
	voice.Manager
	conns   map[snowflake.ID]*fakeConn
	openErr error // given to every new connection
	created []*fakeConn
}

func (m *fakeVoiceManager) CreateConn(guildID snowflake.ID) voice.Conn {
	if existing, ok := m.conns[guildID]; ok {
		return existing
	}
	conn := &fakeConn{manager: m, guildID: guildID, openErr: m.openErr}
	m.conns[guildID] = conn
	m.created = append(m.created, conn)
	return conn
}

func (m *fakeVoiceManager) GetConn(guildID snowflake.ID) voice.Conn {
	conn, ok := m.conns[guildID]
	if !ok {
		// return a real nil interface: a nil *fakeConn inside the interface
		// would not compare equal to nil and would fool the code under test
		return nil
	}
	return conn
}

// connectBot puts the bot in a channel as if a previous start had joined it.
func (m *fakeVoiceManager) connectBot(guildID, channelID snowflake.ID) *fakeConn {
	conn := m.CreateConn(guildID).(*fakeConn)
	conn.channelID = &channelID
	return conn
}

// fakeRest records follow-up messages instead of sending them to Discord.
type fakeRest struct {
	rest.Rest
	followups   []discord.MessageCreate
	followupErr error
}

func (r *fakeRest) CreateFollowupMessage(_ snowflake.ID, _ string, messageCreate discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	r.followups = append(r.followups, messageCreate)
	if r.followupErr != nil {
		return nil, r.followupErr
	}
	return &discord.Message{}, nil
}

// ---------- the fake server ----------

type testEnv struct {
	client *bot.Client
	voice  *fakeVoiceManager
	rest   *fakeRest
}

// newTestEnv builds a server where:
//   - @everyone can View Channel and Connect, so the bot can join meetingChannelID
//   - lockedChannelID denies @everyone Connect
//   - the caller is sitting in meetingChannelID
//
// It also swaps in empty settings and sessions, restoring the real ones after.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	caches := cache.New(cache.WithCaches(cache.FlagsAll))
	caches.SetSelfUser(discord.OAuth2User{User: discord.User{ID: botID}})
	caches.AddRole(discord.Role{
		ID:          serverID, // @everyone's role ID is the server's ID
		GuildID:     serverID,
		Permissions: discord.PermissionViewChannel | discord.PermissionConnect,
	})
	caches.AddMember(discord.Member{GuildID: serverID, User: discord.User{ID: botID}})
	caches.AddChannel(voiceChannel(t, meetingChannelID, `[]`))
	caches.AddChannel(voiceChannel(t, lockedChannelID,
		fmt.Sprintf(`[{"id":"%d","type":0,"allow":"0","deny":"%d"}]`, serverID, discord.PermissionConnect)))
	caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: callerID, ChannelID: idPtr(meetingChannelID)})

	env := &testEnv{
		voice: &fakeVoiceManager{conns: make(map[snowflake.ID]*fakeConn)},
		rest:  &fakeRest{},
	}
	env.client = &bot.Client{Caches: caches, VoiceManager: env.voice, Rest: env.rest}

	oldSettings, oldSessions := settings, activeSessions
	t.Cleanup(func() { settings, activeSessions = oldSettings, oldSessions })

	var err error
	settings, err = loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	activeSessions = newSessionStore()

	return env
}

func voiceChannel(t *testing.T, id snowflake.ID, overwrites string) discord.GuildChannel {
	t.Helper()
	var channel discord.GuildVoiceChannel
	raw := fmt.Sprintf(`{"id":"%d","type":2,"guild_id":"%d","name":"voice","permission_overwrites":%s}`, id, serverID, overwrites)
	if err := json.Unmarshal([]byte(raw), &channel); err != nil {
		t.Fatalf("building voice channel: %v", err)
	}
	return channel
}

func idPtr(id snowflake.ID) *snowflake.ID { return &id }

// ---------- building commands ----------

// caller describes who runs a command.
type caller struct {
	id          snowflake.ID
	roles       []snowflake.ID
	permissions discord.Permissions
	inDM        bool
}

var (
	plainMember = caller{id: callerID}
	moderator   = caller{id: callerID, permissions: discord.PermissionManageChannels}
	noteTaker   = caller{id: callerID, roles: []snowflake.ID{noteTakerRoleID}}
)

func startCommand(title string) string {
	return fmt.Sprintf(`{"id":"1","name":"transcribe","type":1,"options":[{"name":"start","type":1,"options":[{"name":"title","type":3,"value":%q}]}]}`, title)
}

func stopCommand() string {
	return `{"id":"1","name":"transcribe","type":1,"options":[{"name":"stop","type":1}]}`
}

func setupCommand(roleID snowflake.ID) string {
	return fmt.Sprintf(`{"id":"2","name":"captions-setup","type":1,`+
		`"options":[{"name":"notetaker-role","type":8,"value":"%d"}],`+
		`"resolved":{"roles":{"%d":{"id":"%d","name":"Note-taker","permissions":"0"}}}}`, roleID, roleID, roleID)
}

// replies records what a handler sent back as its first reply.
type replies struct {
	messages []discord.MessageCreate
	err      error // if set, sending a reply fails with this
}

// command builds the event disgo would hand to a handler, from Discord-style JSON.
func (env *testEnv) command(t *testing.T, who caller, data string) (*handler.CommandEvent, *replies) {
	t.Helper()

	var raw string
	if who.inDM {
		raw = fmt.Sprintf(`{"id":"9","type":2,"application_id":"8","token":"tok","version":1,`+
			`"user":{"id":"%d","username":"caller"},"data":%s}`, who.id, data)
	} else {
		roles, _ := json.Marshal(who.roles)
		if who.roles == nil {
			roles = []byte("[]")
		}
		raw = fmt.Sprintf(`{"id":"9","type":2,"application_id":"8","token":"tok","version":1,"guild_id":"%d",`+
			`"member":{"user":{"id":"%d","username":"caller"},"roles":%s,"permissions":"%d"},"data":%s}`,
			serverID, who.id, roles, who.permissions, data)
	}

	var interaction discord.ApplicationCommandInteraction
	if err := json.Unmarshal([]byte(raw), &interaction); err != nil {
		t.Fatalf("building command: %v", err)
	}

	sent := &replies{}
	event := &handler.CommandEvent{
		ApplicationCommandInteractionCreate: &events.ApplicationCommandInteractionCreate{
			GenericEvent:                  events.NewGenericEvent(env.client, 0, 0),
			ApplicationCommandInteraction: interaction,
			Respond: func(_ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
				if sent.err != nil {
					return sent.err
				}
				sent.messages = append(sent.messages, data.(discord.MessageCreate))
				return nil
			},
		},
		Ctx: context.Background(),
	}
	return event, sent
}

// onlyReply checks exactly one reply was sent, that only the caller can see it,
// and that it says what we expect.
func onlyReply(t *testing.T, sent *replies, wantText string) discord.MessageCreate {
	t.Helper()
	if len(sent.messages) != 1 {
		t.Fatalf("got %d replies, want 1: %+v", len(sent.messages), sent.messages)
	}
	reply := sent.messages[0]
	if !reply.Flags.Has(discord.MessageFlagEphemeral) {
		t.Errorf("reply should be hidden (ephemeral): %q", reply.Content)
	}
	if !strings.Contains(reply.Content, wantText) {
		t.Errorf("reply %q should contain %q", reply.Content, wantText)
	}
	return reply
}

func noSession(t *testing.T) {
	t.Helper()
	if current, running := activeSessions.get(serverID); running {
		t.Fatalf("expected no session, found %+v", current)
	}
}

// ---------- /transcribe start ----------

func TestStartJoinsTheCallersChannel(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, startCommand("Weekly Meetup"))

	if err := onTranscribeStart(event); err != nil {
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
	tr, ok := conn.receiver.(*transcriber)
	if !ok {
		t.Fatalf("after joining it should start captioning with a transcriber, got %T", conn.receiver)
	}
	defer tr.Close() // stops its quiet timer; the real disgo does this when the call ends

	// Captions go out under the speaker's name in this server.
	env.client.Caches.AddMember(discord.Member{GuildID: serverID,
		User: discord.User{ID: callerID, Username: "sam123", GlobalName: strPtr("Sam")}})
	logs := captureLog(t)
	tr.onText(callerID, "hello everyone", true)
	if !strings.Contains(logs.String(), "Sam: hello everyone") {
		t.Errorf("captions should show the speaker's name, logged:\n%s", logs)
	}

	current, running := activeSessions.get(serverID)
	want := session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "Weekly Meetup"}
	if !running || current != want {
		t.Fatalf("session: got (%+v, %v), want (%+v, true)", current, running, want)
	}
}

func TestStartAllowsNoteTakers(t *testing.T) {
	env := newTestEnv(t)
	_ = settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	event, sent := env.command(t, noteTaker, startCommand("Notes"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm joining")
}

func TestStartInDMIsRefused(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
	noSession(t)
}

func TestStartRefusesNonNoteTakersAndHintsAtSetup(t *testing.T) {
	env := newTestEnv(t) // no note-taker role set yet
	event, sent := env.command(t, plainMember, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Only note-takers and moderators can start captions here. An admin can choose the note-taker role with /captions-setup.")
	noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not touch voice when refusing")
	}
}

func TestStartRefusesMembersWithoutTheRoleOnceItIsSet(t *testing.T) {
	env := newTestEnv(t)
	_ = settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	someoneElse := caller{id: callerID, roles: []snowflake.ID{9999}}
	event, sent := env.command(t, someoneElse, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
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

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't see you in a chat")
	noSession(t)
}

func TestStartWhileAlreadyTranscribingLeavesTheBotWhereItIs(t *testing.T) {
	env := newTestEnv(t)
	running := session{StartedBy: otherMemberID, ChannelID: lockedChannelID, Title: "Earlier"}
	activeSessions.start(serverID, running)
	event, sent := env.command(t, moderator, startCommand("Later"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm already transcribing 'Earlier' in "+discord.ChannelMention(lockedChannelID))
	if len(env.voice.created) != 0 {
		t.Error("must not create or move a voice connection")
	}
	current, _ := activeSessions.get(serverID)
	if current != running {
		t.Errorf("the running session must be untouched, got %+v", current)
	}
}

func TestStartWhenBotMemberIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveMember(serverID, botID)
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I'm allowed to join")
	noSession(t) // the claim is given back
}

func TestStartWhenChannelIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveChannel(meetingChannelID)
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I'm allowed to join")
	noSession(t)
}

func TestStartWhenBotLacksConnect(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: callerID, ChannelID: idPtr(lockedChannelID)})
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I don't have permission to join "+discord.ChannelMention(lockedChannelID))
	noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not try to join a channel it can't connect to")
	}
}

func TestStartWhenTheReplyFails(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, startCommand("x"))
	sent.err = errors.New("discord unreachable")

	if err := onTranscribeStart(event); err == nil {
		t.Fatal("the reply's error should be returned so middleware.Go logs it")
	}
	noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not join after failing to reply")
	}
}

func TestStartWhenJoiningFails(t *testing.T) {
	env := newTestEnv(t)
	env.voice.openErr = context.DeadlineExceeded
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
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
	noSession(t) // the claim is given back so start can be tried again
}

func TestStartWhenJoiningFailsAndTheFollowupFails(t *testing.T) {
	env := newTestEnv(t)
	env.voice.openErr = context.DeadlineExceeded
	env.rest.followupErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err == nil {
		t.Fatal("the follow-up's error should be returned")
	}
	noSession(t)
}

// ---------- /transcribe stop ----------

// startedSession puts a session and a connected bot in place, as a successful start would.
func (env *testEnv) startedSession(startedBy snowflake.ID) *fakeConn {
	activeSessions.start(serverID, session{StartedBy: startedBy, ChannelID: meetingChannelID, Title: "Weekly"})
	return env.voice.connectBot(serverID, meetingChannelID)
}

func TestStopByTheStarterEvenWithoutPermissions(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)
	event, sent := env.command(t, plainMember, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving "+discord.ChannelMention(meetingChannelID)+" and stopping the captions for 'Weekly'")
	if !conn.closed {
		t.Error("should leave voice")
	}
	noSession(t)
}

func TestStopByANoteTaker(t *testing.T) {
	env := newTestEnv(t)
	_ = settings.SetNoteTakerRole(serverID, noteTakerRoleID)
	conn := env.startedSession(otherMemberID)
	event, sent := env.command(t, noteTaker, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	if !conn.closed {
		t.Error("should leave voice")
	}
	noSession(t)
}

func TestStopByAModerator(t *testing.T) {
	env := newTestEnv(t)
	env.startedSession(otherMemberID)
	event, sent := env.command(t, moderator, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	noSession(t)
}

func TestStopRefusesSomeoneElse(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(otherMemberID)
	event, sent := env.command(t, plainMember, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Only the person who started these captions, note-takers or moderators can stop them.")
	if conn.closed {
		t.Error("must not leave voice")
	}
	if _, running := activeSessions.get(serverID); !running {
		t.Error("the session must keep running")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm not transcribing in this server right now.")
}

func TestStopInDM(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
}

func TestStopWhenTheBotIsAlreadyOutOfVoice(t *testing.T) {
	env := newTestEnv(t)
	activeSessions.start(serverID, session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "Weekly"})
	event, sent := env.command(t, moderator, stopCommand())

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I'm leaving")
	noSession(t) // nothing to close, but the session is still cleared
}

func TestStopWhenTheReplyFails(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)
	event, sent := env.command(t, moderator, stopCommand())
	sent.err = errors.New("discord unreachable")

	if err := onTranscribeStop(event); err == nil {
		t.Fatal("the reply's error should be returned")
	}
	if conn.closed {
		t.Error("should not leave without telling the caller")
	}
	if _, running := activeSessions.get(serverID); !running {
		t.Error("the session should keep running")
	}
}

// ---------- /captions-setup ----------

func TestCaptionsSetupSavesTheRole(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Got it, I'll treat "+discord.RoleMention(noteTakerRoleID)+" as note-takers here.")

	got, ok := settings.NoteTakerRole(serverID)
	if !ok || got != noteTakerRoleID {
		t.Fatalf("role not saved: got (%v, %v)", got, ok)
	}
	reloaded, err := loadSettings(settings.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.NoteTakerRole(serverID); !ok || got != noteTakerRoleID {
		t.Fatalf("role not written to settings.json: got (%v, %v)", got, ok)
	}
}

func TestCaptionsSetupWhenSavingFails(t *testing.T) {
	env := newTestEnv(t)
	settings.path = filepath.Join(t.TempDir(), "missing-folder", "settings.json")
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't save that")
}

func TestCaptionsSetupInDM(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, setupCommand(noteTakerRoleID))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
}

// ---------- canManageTranscription ----------

func TestCanManageTranscription(t *testing.T) {
	tests := []struct {
		name    string
		who     caller
		roleSet bool
		want    bool
	}{
		{"moderator", moderator, false, true},
		{"note-taker", noteTaker, true, true},
		{"has the role's ID but no role is set yet", noteTaker, false, false},
		{"plain member, no role set", plainMember, false, false},
		{"plain member, role set", plainMember, true, false},
		{"DM (no member)", caller{id: callerID, inDM: true}, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			if test.roleSet {
				_ = settings.SetNoteTakerRole(serverID, noteTakerRoleID)
			}
			event, _ := env.command(t, test.who, stopCommand())

			if got := canManageTranscription(event, serverID); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

// ---------- bot disconnected or moved by someone else ----------

func (env *testEnv) voiceEvent(userID snowflake.ID) *events.GenericGuildVoiceState {
	return &events.GenericGuildVoiceState{
		GenericEvent: events.NewGenericEvent(env.client, 0, 0),
		VoiceState:   discord.VoiceState{GuildID: serverID, UserID: userID},
	}
}

func TestBotDisconnectedEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	onBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	noSession(t)
	if !conn.closed {
		t.Error("the dropped connection should be closed, or the next start would get it back")
	}
	if env.voice.GetConn(serverID) != nil {
		t.Error("the connection should be removed from the manager")
	}
}

func TestBotDisconnectedWithNoConnectionLeftStillEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	activeSessions.start(serverID, session{StartedBy: callerID})

	onBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	noSession(t)
}

func TestBotMovedEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	onBotMovedVoice(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(botID)})

	noSession(t)
	if !conn.closed {
		t.Error("the bot should leave the channel it was dragged into")
	}
}

func TestOtherPeopleLeavingOrMovingIsIgnored(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	onBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(otherMemberID)})
	onBotMovedVoice(&events.GuildVoiceMove{GenericGuildVoiceState: env.voiceEvent(otherMemberID)})

	if _, running := activeSessions.get(serverID); !running {
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

	onBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	if conn.closed {
		t.Error("with no session the leave was ours; nothing to clean up")
	}
}

// ---------- replies ----------

func TestHiddenMessagesAreEphemeral(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, stopCommand())

	if err := createHiddenMessageForCommandCaller(event, "hello"); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "hello")
}
