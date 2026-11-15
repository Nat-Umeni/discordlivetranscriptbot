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
	"sync"
	"testing"
	"time"

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
	serverID          snowflake.ID = 1000
	botID             snowflake.ID = 2000
	callerID          snowflake.ID = 3000
	otherMemberID     snowflake.ID = 3001
	noteTakerRoleID   snowflake.ID = 4000
	noCaptionsRoleID  snowflake.ID = 4001 // members with it are never captioned
	meetingChannelID  snowflake.ID = 5000 // the bot can join this one
	lockedChannelID   snowflake.ID = 5001 // @everyone is denied Connect here
	noStatusChannelID snowflake.ID = 5002 // @everyone is denied Set Voice Channel Status here

	captionsChannelID       snowflake.ID = 6000 // text channel the bot can post captions in
	lockedCaptionsChannelID snowflake.ID = 6001 // @everyone is denied Create Public Threads here

	introMessageID  snowflake.ID = 7000 // the opening post the fake REST client "creates" in the captions thread
	threadID        snowflake.ID = 7001 // the captions thread it "creates"
	noticeMessageID snowflake.ID = 7002 // the notice it "creates" in the call's chat
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
	onClose            func()                  // if set, runs when the connection closes
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
	if c.onClose != nil {
		c.onClose()
	}
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

// Close mirrors disgo: every connection is closed and forgotten.
func (m *fakeVoiceManager) Close(ctx context.Context) {
	for _, conn := range m.conns {
		conn.Close(ctx)
	}
}

// connectBot puts the bot in a channel as if a previous start had joined it.
func (m *fakeVoiceManager) connectBot(guildID, channelID snowflake.ID) *fakeConn {
	conn := m.CreateConn(guildID).(*fakeConn)
	conn.channelID = &channelID
	return conn
}

// fakeRest records follow-ups, posts and threads instead of sending them to
// Discord. Each *Err field makes that call fail.
type fakeRest struct {
	rest.Rest
	followups   []discord.MessageCreate
	followupErr error

	posted    []postedMessage
	postErrIn map[snowflake.ID]error // posting in that channel fails

	threads   []startedThread
	threadErr error

	deleted        []postedMessage // only channelID and messageID are filled in
	deletedThreads []snowflake.ID
	deleteErr      error // deleting a message or a thread fails

	statuses  []postedMessage // voice channel statuses set; message.Content holds the status
	statusErr error

	edits   []postedMessage // message edits; message.Content holds the new text
	editErr error

	threadMembers []postedMessage // people added to threads: channelID is the thread, messageID the user
	addMemberErr  error

	messages  map[snowflake.ID]discord.Message // what GetMessage finds
	getErr    error
	responses []string // edits to a deferred interaction response (the hidden "thinking…" reply)
}

type postedMessage struct {
	channelID snowflake.ID
	messageID snowflake.ID
	message   discord.MessageCreate
}

type startedThread struct {
	channelID snowflake.ID
	create    discord.ThreadCreate
}

func (r *fakeRest) CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	messageID := noticeMessageID
	if channelID == threadID {
		messageID = introMessageID
	}
	r.posted = append(r.posted, postedMessage{channelID, messageID, messageCreate})
	if err := r.postErrIn[channelID]; err != nil {
		return nil, err
	}
	return &discord.Message{ID: messageID, ChannelID: channelID}, nil
}

func (r *fakeRest) SetVoiceChannelStatus(channelID snowflake.ID, update discord.VoiceChannelStatusUpdate, _ ...rest.RequestOpt) error {
	r.statuses = append(r.statuses, postedMessage{channelID: channelID, message: discord.MessageCreate{Content: update.Status}})
	return r.statusErr
}

func (r *fakeRest) CreateThread(channelID snowflake.ID, create discord.ThreadCreate, _ ...rest.RequestOpt) (*discord.GuildThread, error) {
	r.threads = append(r.threads, startedThread{channelID, create})
	if r.threadErr != nil {
		return nil, r.threadErr
	}
	// GuildThread's ID field is private, so build it from JSON like disgo does.
	var thread discord.GuildThread
	raw := fmt.Sprintf(`{"id":"%d","type":11,"guild_id":"%d","parent_id":"%d"}`, threadID, serverID, channelID)
	if err := json.Unmarshal([]byte(raw), &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

// Close does nothing: there's no real connection to close.
func (r *fakeRest) Close(context.Context) {}

func (r *fakeRest) UpdateMessage(channelID, messageID snowflake.ID, update discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	r.edits = append(r.edits, postedMessage{channelID: channelID, messageID: messageID, message: discord.MessageCreate{Content: *update.Content}})
	if r.editErr != nil {
		return nil, r.editErr
	}
	return &discord.Message{ID: messageID, ChannelID: channelID}, nil
}

func (r *fakeRest) GetMessage(_ snowflake.ID, messageID snowflake.ID, _ ...rest.RequestOpt) (*discord.Message, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	message, ok := r.messages[messageID]
	if !ok {
		return nil, errors.New("unknown message")
	}
	return &message, nil
}

func (r *fakeRest) UpdateInteractionResponse(_ snowflake.ID, _ string, update discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	r.responses = append(r.responses, *update.Content)
	return &discord.Message{}, nil
}

func (r *fakeRest) AddThreadMember(threadID, userID snowflake.ID, _ ...rest.RequestOpt) error {
	r.threadMembers = append(r.threadMembers, postedMessage{channelID: threadID, messageID: userID})
	return r.addMemberErr
}

func (r *fakeRest) DeleteMessage(channelID, messageID snowflake.ID, _ ...rest.RequestOpt) error {
	r.deleted = append(r.deleted, postedMessage{channelID: channelID, messageID: messageID})
	return r.deleteErr
}

func (r *fakeRest) DeleteChannel(channelID snowflake.ID, _ ...rest.RequestOpt) error {
	r.deletedThreads = append(r.deletedThreads, channelID)
	return r.deleteErr
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
//   - @everyone can also post, make threads and set voice channel statuses, so
//     the bot can announce in the call and post captions in captionsChannelID,
//     which is picked as the captions channel
//   - noStatusChannelID denies @everyone Set Voice Channel Status
//   - lockedCaptionsChannelID denies @everyone Create Public Threads
//   - noCaptionsRoleID is picked as the no-captions role
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
		Permissions: voicePermissions | captionsPermissions,
	})
	caches.AddMember(discord.Member{GuildID: serverID, User: discord.User{ID: botID}})
	caches.AddChannel(voiceChannel(t, meetingChannelID, `[]`))
	caches.AddChannel(voiceChannel(t, lockedChannelID,
		fmt.Sprintf(`[{"id":"%d","type":0,"allow":"0","deny":"%d"}]`, serverID, discord.PermissionConnect)))
	caches.AddChannel(voiceChannel(t, noStatusChannelID,
		fmt.Sprintf(`[{"id":"%d","type":0,"allow":"0","deny":"%d"}]`, serverID, discord.PermissionSetVoiceChannelStatus)))
	caches.AddChannel(textChannel(t, captionsChannelID, `[]`))
	caches.AddChannel(textChannel(t, lockedCaptionsChannelID,
		fmt.Sprintf(`[{"id":"%d","type":0,"allow":"0","deny":"%d"}]`, serverID, discord.PermissionCreatePublicThreads)))
	caches.AddVoiceState(discord.VoiceState{GuildID: serverID, UserID: callerID, ChannelID: idPtr(meetingChannelID)})

	env := &testEnv{
		voice: &fakeVoiceManager{conns: make(map[snowflake.ID]*fakeConn)},
		rest:  &fakeRest{},
	}
	env.client = &bot.Client{Caches: caches, VoiceManager: env.voice, Rest: env.rest}

	// Stop any captions a test started, so their goroutines don't run on
	// into later tests.
	t.Cleanup(func() {
		for _, conn := range env.voice.created {
			if tr, ok := conn.receiver.(*transcriber); ok {
				tr.Close()
			}
		}
	})

	oldSettings, oldSessions := settings, activeSessions
	t.Cleanup(func() { settings, activeSessions = oldSettings, oldSessions })

	var err error
	settings, err = loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.SetCaptionsChannel(serverID, captionsChannelID); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetNoCaptionsRole(serverID, noCaptionsRoleID); err != nil {
		t.Fatal(err)
	}
	activeSessions = newSessionStore()

	return env
}

// clearSettings swaps in empty settings, as on a server nobody has set up yet.
func clearSettings(t *testing.T) {
	t.Helper()
	var err error
	settings, err = loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
}

func textChannel(t *testing.T, id snowflake.ID, overwrites string) discord.GuildChannel {
	t.Helper()
	var channel discord.GuildTextChannel
	raw := fmt.Sprintf(`{"id":"%d","type":0,"guild_id":"%d","name":"captions","permission_overwrites":%s}`, id, serverID, overwrites)
	if err := json.Unmarshal([]byte(raw), &channel); err != nil {
		t.Fatalf("building text channel: %v", err)
	}
	return channel
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

// setupCommand builds /captions-setup with any of a note-taker role, a captions
// channel and a no-captions role; 0 leaves that option out. Discord sends the
// chosen roles and channel alongside the options ("resolved"), so this does too.
func setupCommand(roleID, channelID, noCaptionsID snowflake.ID) string {
	var options, resolved, roles []string
	if roleID != 0 {
		options = append(options, fmt.Sprintf(`{"name":"notetaker-role","type":8,"value":"%d"}`, roleID))
		roles = append(roles, fmt.Sprintf(`"%d":{"id":"%d","name":"Note-taker","permissions":"0"}`, roleID, roleID))
	}
	if noCaptionsID != 0 {
		options = append(options, fmt.Sprintf(`{"name":"no-captions-role","type":8,"value":"%d"}`, noCaptionsID))
		roles = append(roles, fmt.Sprintf(`"%d":{"id":"%d","name":"No captions","permissions":"0"}`, noCaptionsID, noCaptionsID))
	}
	if len(roles) > 0 {
		resolved = append(resolved, `"roles":{`+strings.Join(roles, ",")+`}`)
	}
	if channelID != 0 {
		options = append(options, fmt.Sprintf(`{"name":"captions-channel","type":7,"value":"%d"}`, channelID))
		resolved = append(resolved, fmt.Sprintf(`"channels":{"%d":{"id":"%d","type":0,"name":"captions","permissions":"0"}}`, channelID, channelID))
	}
	return fmt.Sprintf(`{"id":"2","name":"captions-setup","type":1,"options":[%s],"resolved":{%s}}`,
		strings.Join(options, ","), strings.Join(resolved, ","))
}

// replies records what a handler sent back as its first reply.
type replies struct {
	messages []discord.MessageCreate
	popups   []discord.ModalCreate // popups opened instead of a message
	err      error                 // if set, sending a reply fails with this
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
				if popup, ok := data.(discord.ModalCreate); ok {
					sent.popups = append(sent.popups, popup)
					return nil
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

	current, running := activeSessions.get(serverID)
	want := session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "Weekly Meetup"}
	if !running || current != want {
		t.Fatalf("session: got (%+v, %v), want (%+v, true)", current, running, want)
	}

	// Opted-out speakers are skipped: the no-captions role is checked. (Sam,
	// the caller, is in the call, so they're cached like everyone in voice.)
	env.client.Caches.AddMember(discord.Member{GuildID: serverID,
		User: discord.User{ID: callerID, Username: "sam123", GlobalName: strPtr("Sam")}})
	env.client.Caches.AddMember(discord.Member{GuildID: serverID, RoleIDs: []snowflake.ID{noCaptionsRoleID},
		User: discord.User{ID: otherMemberID, Username: "jo"}})
	if !tr.optedOut(otherMemberID) || tr.optedOut(callerID) {
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
	out, ok := tr.out.(multiOutput)
	if !ok || len(out) != 2 {
		t.Fatalf("want captions going to the terminal and the thread, got %#v", tr.out)
	}
	if _, ok := out[0].(terminalOutput); !ok {
		t.Errorf("first output should be the terminal, got %T", out[0])
	}
	poster, ok := out[1].(*threadPoster)
	if !ok || poster.threadID != threadID {
		t.Fatalf("second output should post in the new thread, got %#v", out[1])
	}

	logs := captureLog(t)
	tr.out.caption(callerID, "hello everyone", true)
	tr.Close() // the call ends: the poster sends what's left and stops
	select {
	case <-poster.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the poster never finished")
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
			clearSettings(t)
			if test.channel {
				_ = settings.SetCaptionsChannel(serverID, captionsChannelID)
			}
			if test.noCapts {
				_ = settings.SetNoCaptionsRole(serverID, noCaptionsRoleID)
			}
			event, sent := env.command(t, moderator, startCommand("x"))

			if err := onTranscribeStart(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, "Before I can caption here, an admin needs to "+test.want)
			noSession(t)
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

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I don't have permission to join "+discord.ChannelMention(noStatusChannelID)+
		", ask an admin to give me View Channel, Connect, Send Messages and Set Voice Channel Status there")
	noSession(t)
	if len(env.voice.created) != 0 {
		t.Error("should not join a call it can't announce captions in")
	}
}

func TestStartWhenTheBotCantPostCaptions(t *testing.T) {
	env := newTestEnv(t)
	_ = settings.SetCaptionsChannel(serverID, lockedCaptionsChannelID) // permissions changed since setup
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't post captions in "+discord.ChannelMention(lockedCaptionsChannelID))
	noSession(t) // the claim is given back
	if len(env.voice.created) != 0 {
		t.Error("should not join when it can't post captions")
	}
}

func TestStartWhenTheCaptionsChannelIsGone(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.RemoveChannel(captionsChannelID) // deleted since setup
	event, sent := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I can post in the captions channel. If it's been deleted, pick another with /captions-setup captions-channel.")
	noSession(t)
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
	noSession(t)
}

func TestStartWhenTheNoticeFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.postErrIn = map[snowflake.ID]error{meetingChannelID: errors.New("discord unreachable")}
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
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

	if err := onTranscribeStart(event); err != nil {
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

	if err := onTranscribeStart(event); err != nil {
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

	if err := onTranscribeStart(event); err != nil {
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

	if err := onTranscribeStart(event); err != nil {
		t.Fatalf("the notice already told people, so a missing status isn't worth stopping for: %v", err)
	}
	stillCaptioning(t, env)
}

func TestStartStillCaptionsWhenTheLinkFollowupFails(t *testing.T) {
	env := newTestEnv(t)
	env.rest.followupErr = errors.New("discord unreachable")
	event, _ := env.command(t, moderator, startCommand("x"))

	if err := onTranscribeStart(event); err != nil {
		t.Fatalf("a missing link isn't worth stopping captions for: %v", err)
	}
	stillCaptioning(t, env)
}

func stillCaptioning(t *testing.T, env *testEnv) {
	t.Helper()
	conn := env.voice.created[0]
	tr, ok := conn.receiver.(*transcriber)
	if !ok {
		t.Fatalf("captions should still start, receiver is %T", conn.receiver)
	}
	tr.Close()
	if conn.closed {
		t.Error("should stay in voice")
	}
	if _, running := activeSessions.get(serverID); !running {
		t.Error("the session should still be running")
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
	statusCleared(t, env)
	noSession(t)
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

	if err := onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	if !conn.closed {
		t.Error("a status that won't clear mustn't keep the bot in the call")
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
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, 0, 0))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	for _, want := range []string{
		"Note-takers: " + discord.RoleMention(noteTakerRoleID),
		"Captions go in: " + discord.ChannelMention(captionsChannelID),        // set by newTestEnv, kept
		"Not captioned: anyone with " + discord.RoleMention(noCaptionsRoleID), // same
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}

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
	if got, _ := reloaded.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("setting the role must not wipe the captions channel, got %v", got)
	}
}

func TestCaptionsSetupSavesTheChannel(t *testing.T) {
	env := newTestEnv(t)
	clearSettings(t) // a server nobody has set up yet
	event, sent := env.command(t, moderator, setupCommand(0, captionsChannelID, 0))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	for _, want := range []string{
		"Captions go in: " + discord.ChannelMention(captionsChannelID),
		"Note-takers: not set (only moderators can start captions)",
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}

	reloaded, err := loadSettings(settings.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.CaptionsChannel(serverID); !ok || got != captionsChannelID {
		t.Fatalf("channel not written to settings.json: got (%v, %v)", got, ok)
	}
	if _, ok := reloaded.NoteTakerRole(serverID); ok {
		t.Error("saving only a channel shouldn't make a note-taker role appear")
	}
}

func TestCaptionsSetupSavesBoth(t *testing.T) {
	env := newTestEnv(t)
	clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, captionsChannelID, 0))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Got it.")
	if got, _ := settings.NoteTakerRole(serverID); got != noteTakerRoleID {
		t.Errorf("role not saved, got %v", got)
	}
	if got, _ := settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("channel not saved, got %v", got)
	}
}

func TestCaptionsSetupWithNothingShowsTheSettings(t *testing.T) {
	env := newTestEnv(t)
	clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(0, 0, 0))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Here's how captions are set up here:")
	if strings.HasPrefix(reply.Content, "Got it") {
		t.Error("nothing was changed, so it shouldn't say it saved anything")
	}
	for _, want := range []string{
		"Captions go in: not set (I need one before I can start captions)",
		"Not captioned: not set (I need one before I can start captions, so people can opt out)",
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}
}

func TestCaptionsSetupSavesTheNoCaptionsRole(t *testing.T) {
	env := newTestEnv(t)
	clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(0, 0, noCaptionsRoleID))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	if want := "Not captioned: anyone with " + discord.RoleMention(noCaptionsRoleID); !strings.Contains(reply.Content, want) {
		t.Errorf("reply %q should contain %q", reply.Content, want)
	}

	reloaded, err := loadSettings(settings.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.NoCaptionsRole(serverID); !ok || got != noCaptionsRoleID {
		t.Fatalf("no-captions role not written to settings.json: got (%v, %v)", got, ok)
	}
	if _, ok := reloaded.NoteTakerRole(serverID); ok {
		t.Error("the no-captions role must not be saved as the note-taker role")
	}
}

func TestCaptionsSetupRefusesAChannelTheBotCantPostIn(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, lockedCaptionsChannelID, 0))

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't post captions in "+discord.ChannelMention(lockedCaptionsChannelID)+
		". Ask an admin to give me View Channel, Send Messages, Create Public Threads and Send Messages in Threads there.")
	if got, _ := settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("the refused channel must not be saved, got %v", got)
	}
	if _, ok := settings.NoteTakerRole(serverID); ok {
		t.Error("nothing should be saved when the command is refused, including the role")
	}
}

func TestCaptionsSetupWhenTheChannelIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(0, 6999, 0)) // a channel the cache doesn't know

	if err := onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I can post in that channel")
	if got, _ := settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("an unchecked channel must not be saved, got %v", got)
	}
}

func TestCaptionsSetupWhenSavingFails(t *testing.T) {
	for _, test := range []struct {
		name                        string
		roleID, channel, noCaptions snowflake.ID
	}{
		{"note-taker role", noteTakerRoleID, 0, 0},
		{"channel", 0, captionsChannelID, 0},
		{"no-captions role", 0, 0, noCaptionsRoleID},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			settings.path = filepath.Join(t.TempDir(), "missing-folder", "settings.json")
			event, sent := env.command(t, moderator, setupCommand(test.roleID, test.channel, test.noCaptions))

			if err := onCaptionsSetup(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, "I couldn't save that")
		})
	}
}

func TestCaptionsSetupInDM(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, setupCommand(noteTakerRoleID, 0, 0))

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

func TestShutDownClearsStatusesLeavesCallsAndWaitsForCaptions(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	// A poster still sending when shutdown starts: it finishes once the call
	// is left (in the real bot, leaving closes the transcriber, which closes
	// its poster).
	var posters sync.WaitGroup
	poster := newThreadPoster(newFakeThread(), threadID, testNames, &posters)
	conn.onClose = poster.closed

	shutDown(env.client, &posters)

	statusCleared(t, env)
	if !conn.closed {
		t.Error("shutting down should leave every call")
	}
	select {
	case <-poster.finished:
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
	var posters sync.WaitGroup
	poster := newThreadPoster(newFakeThread(), threadID, testNames, &posters)
	defer poster.closed()

	shutDown(env.client, &posters) // the poster is never closed

	if !strings.Contains(logs.String(), "some captions were still being posted when I shut down") {
		t.Errorf("a stuck poster should be logged, not wait forever, logged:\n%s", logs)
	}
}

func TestShutdownClearsEveryCallsStatus(t *testing.T) {
	env := newTestEnv(t)
	const otherServer, otherCall snowflake.ID = 1001, 5100
	activeSessions.start(serverID, session{StartedBy: callerID, ChannelID: meetingChannelID, Title: "A"})
	activeSessions.start(otherServer, session{StartedBy: callerID, ChannelID: otherCall, Title: "B"})

	clearStatusesOnShutdown(env.client)

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

func TestBotDisconnectedEndsTheSession(t *testing.T) {
	env := newTestEnv(t)
	conn := env.startedSession(callerID)

	onBotLeftVoice(&events.GuildVoiceLeave{GenericGuildVoiceState: env.voiceEvent(botID)})

	noSession(t)
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
	statusCleared(t, env) // the call it was captioning, not the one it was dragged into
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
