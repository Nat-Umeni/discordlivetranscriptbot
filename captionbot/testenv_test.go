package captionbot

import (
	"bytes"
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

	"livetranscribebot/settings"
	"livetranscribebot/speech"
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
	client       *bot.Client
	voice        *fakeVoiceManager
	rest         *fakeRest
	bot          *Bot   // the handlers under test, with this test's own settings
	settingsPath string // where bot's settings are saved
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
			if tr, ok := conn.receiver.(*speech.Transcriber); ok {
				tr.Close()
			}
		}
	})

	// No speech model in handler tests: the transcriber only needs it once
	// someone talks.
	env.bot = New(nil, nil)
	env.useSettingsFile(t, filepath.Join(t.TempDir(), "settings.json"))
	if err := env.bot.settings.SetCaptionsChannel(serverID, captionsChannelID); err != nil {
		t.Fatal(err)
	}
	if err := env.bot.settings.SetNoCaptionsRole(serverID, noCaptionsRoleID); err != nil {
		t.Fatal(err)
	}

	return env
}

// useSettingsFile gives the bot empty settings saved at path.
func (env *testEnv) useSettingsFile(t *testing.T, path string) {
	t.Helper()
	store, err := settings.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	env.bot.settings = store
	env.settingsPath = path
}

// clearSettings swaps in empty settings, as on a server nobody has set up yet.
func (env *testEnv) clearSettings(t *testing.T) {
	t.Helper()
	env.useSettingsFile(t, filepath.Join(t.TempDir(), "settings.json"))
}

// reloadSettings reads the bot's settings back from the file, to check what
// was saved.
func (env *testEnv) reloadSettings(t *testing.T) *settings.Store {
	t.Helper()
	reloaded, err := settings.Load(env.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

func strPtr(s string) *string { return &s }

// captureLog sends log output to a buffer for one test, so the test can check
// what was printed. TestMain discards logs otherwise.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
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

func (env *testEnv) noSession(t *testing.T) {
	t.Helper()
	if current, running := env.bot.sessions.Get(serverID); running {
		t.Fatalf("expected no session, found %+v", current)
	}
}
