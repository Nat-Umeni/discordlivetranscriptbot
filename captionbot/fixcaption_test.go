package captionbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// ---------- the text side: reading and writing captions ----------

func TestEditedMark(t *testing.T) {
	if got := editedMark(discord.Message{}); got != 0 {
		t.Errorf("never edited should be 0, got %d", got)
	}
	when := time.Date(2026, 11, 15, 21, 5, 47, 123456000, time.UTC)
	if got := editedMark(discord.Message{EditedTimestamp: &when}); got != when.UnixNano() {
		t.Errorf("got %d, want %d", got, when.UnixNano())
	}
}

// ---------- opening the popup ----------

const captionMessageID snowflake.ID = 8000

var captionEditedAt = time.Date(2026, 11, 15, 21, 5, 47, 0, time.UTC)

// fixCaptionCommand builds the Apps → Fix caption command for a message,
// with the message as Discord sends it alongside ("resolved").
func fixCaptionCommandJSON(authorID snowflake.ID, content string) string {
	message, _ := json.Marshal(map[string]any{
		"id": captionMessageID.String(), "channel_id": threadID.String(), "type": 0,
		"author":           map[string]any{"id": authorID.String(), "username": "someone"},
		"content":          content,
		"timestamp":        "2026-11-15T21:05:42Z",
		"edited_timestamp": captionEditedAt.Format(time.RFC3339Nano),
	})
	return fmt.Sprintf(`{"id":"5","name":"Fix caption","type":3,"target_id":"%d","resolved":{"messages":{"%d":%s}}}`,
		captionMessageID, captionMessageID, message)
}

func openFixCaption(t *testing.T, env *testEnv, who caller, authorID snowflake.ID, content string) *replies {
	t.Helper()
	event, sent := env.command(t, who, fixCaptionCommandJSON(authorID, content))
	if err := env.bot.onFixCaption(event.MessageCommandInteractionData(), event); err != nil {
		t.Fatal(err)
	}
	return sent
}

func TestFixCaptionOpensAPrefilledPopup(t *testing.T) {
	for _, who := range []caller{moderator, noteTaker} {
		env := newTestEnv(t)
		_ = env.bot.settings.SetNoteTakerRole(serverID, noteTakerRoleID)

		sent := openFixCaption(t, env, who, botID, `**xX\_Sam\_Xx:** so I rang him`+"\n-# ✏️ fixed by Jo")

		if len(sent.messages) != 0 || len(sent.popups) != 1 {
			t.Fatalf("want just the popup, got messages %+v popups %d", sent.messages, len(sent.popups))
		}
		popup := sent.popups[0]
		wantID := fmt.Sprintf("/fix-caption/%d/%d/%d", threadID, captionMessageID, captionEditedAt.UnixNano())
		if popup.CustomID != wantID || popup.Title != "Fix caption" {
			t.Errorf("popup %q %q, want custom ID %q", popup.Title, popup.CustomID, wantID)
		}
		if len(popup.Components) != 3 {
			t.Fatalf("want speaker, caption and tone tags, got %d fields", len(popup.Components))
		}

		speaker := popup.Components[0].(discord.LabelComponent).Component.(discord.TextInputComponent)
		caption := popup.Components[1].(discord.LabelComponent).Component.(discord.TextInputComponent)
		tags := popup.Components[2].(discord.LabelComponent).Component.(discord.StringSelectMenuComponent)
		if speaker.CustomID != speakerField || speaker.Value != "xX_Sam_Xx" || !speaker.Required {
			t.Errorf("speaker field %+v, want it pre-filled with the name as typed", speaker)
		}
		if caption.CustomID != captionField || caption.Value != "so I rang him" || !caption.Required {
			t.Errorf("caption field %+v, want it pre-filled with the text, without the fixed-by line", caption)
		}
		if tags.CustomID != tagsField || len(tags.Options) != len(toneTags) || tags.Required ||
			tags.MinValues == nil || *tags.MinValues != 0 || tags.MaxValues != len(toneTags) {
			t.Errorf("tone tags %+v, want every tag, optional, any number", tags)
		}
	}
}

func TestFixCaptionRefusals(t *testing.T) {
	tests := []struct {
		why      string
		who      caller
		authorID snowflake.ID
		content  string
		want     string
	}{
		{"in a DM", caller{id: callerID, inDM: true}, botID, "**Sam:** hi", "I only work inside a server"},
		{"not a note-taker or moderator", plainMember, botID, "**Sam:** hi", "Only note-takers and moderators can fix captions."},
		{"someone else's message", moderator, otherMemberID, "**Sam:** hi", "That isn't one of my captions."},
		{"the bot's opening post", moderator, botID, "📝 **Live captions: x**", "That isn't one of my captions."},
		{"still being spoken", moderator, botID, "**Sam:** so I" + captions.SpeakingLine, "They're still talking."},
	}
	for _, test := range tests {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			sent := openFixCaption(t, env, test.who, test.authorID, test.content)
			if len(sent.popups) != 0 {
				t.Error("no popup should open")
			}
			onlyReply(t, sent, test.want)
		})
	}
}

// ---------- saving a fix ----------

// fixSubmit builds the popup-submit event Discord sends, as a note-taker with
// the display name Sam, for the caption opened in the tests above.
func fixSubmit(t *testing.T, env *testEnv, permissions discord.Permissions, speaker, caption string, tags []string) (*handler.ModalEvent, *[]discord.InteractionResponseType) {
	t.Helper()
	tagsJSON, _ := json.Marshal(tags)
	if tags == nil {
		tagsJSON = []byte("[]")
	}
	customID := fixCaptionPopup(threadID, captionMessageID, captionEditedAt.UnixNano(), "", "").CustomID
	raw := fmt.Sprintf(`{"id":"9","type":5,"application_id":"8","token":"tok","version":1,"guild_id":"%d","channel_id":"%d",`+
		`"member":{"user":{"id":"%d","username":"sam123","global_name":"Sam"},"roles":[],"permissions":"%d"},`+
		`"data":{"custom_id":%q,"components":[`+
		`{"type":18,"id":1,"component":{"type":4,"id":2,"custom_id":"speaker","value":%q}},`+
		`{"type":18,"id":3,"component":{"type":4,"id":4,"custom_id":"caption","value":%q}},`+
		`{"type":18,"id":5,"component":{"type":3,"id":6,"custom_id":"tags","values":%s}}]}}`,
		serverID, threadID, callerID, permissions, customID, speaker, caption, tagsJSON)

	var interaction discord.ModalSubmitInteraction
	if err := json.Unmarshal([]byte(raw), &interaction); err != nil {
		t.Fatalf("building the popup submit: %v", err)
	}

	// The router fills Vars from the custom ID; do the same here.
	parts := strings.Split(customID, "/") // "", "fix-caption", channel, message, edited
	var responded []discord.InteractionResponseType
	event := &handler.ModalEvent{
		ModalSubmitInteractionCreate: &events.ModalSubmitInteractionCreate{
			GenericEvent:           events.NewGenericEvent(env.client, 0, 0),
			ModalSubmitInteraction: interaction,
			Respond: func(kind discord.InteractionResponseType, _ discord.InteractionResponseData, _ ...rest.RequestOpt) error {
				responded = append(responded, kind)
				return nil
			},
		},
		Vars: map[string]string{"channel": parts[2], "message": parts[3], "edited": parts[4]},
		Ctx:  context.Background(),
	}
	return event, &responded
}

// putCaption puts the caption in the fake Discord, last edited at editedAt.
func putCaption(env *testEnv, content string, editedAt time.Time) {
	env.rest.messages = map[snowflake.ID]discord.Message{
		captionMessageID: {ID: captionMessageID, ChannelID: threadID, Content: content, EditedTimestamp: &editedAt},
	}
}

func onlyAnswer(t *testing.T, env *testEnv, responded *[]discord.InteractionResponseType, want string) {
	t.Helper()
	if len(*responded) != 1 || (*responded)[0] != discord.InteractionResponseTypeDeferredCreateMessage {
		t.Errorf("should first answer with a hidden 'thinking…', responded %v", *responded)
	}
	if len(env.rest.responses) != 1 || !strings.Contains(env.rest.responses[0], want) {
		t.Fatalf("want one answer containing %q, got %q", want, env.rest.responses)
	}
}

func TestFixCaptionSaves(t *testing.T) {
	env := newTestEnv(t)
	putCaption(env, "**Sam:** that's grate", captionEditedAt)
	event, responded := fixSubmit(t, env, discord.PermissionManageChannels, "Priya", "that's great", []string{"[laughs]"})

	if err := env.bot.onFixCaptionSubmit(event); err != nil {
		t.Fatal(err)
	}

	onlyAnswer(t, env, responded, "Fixed ✓")
	if len(env.rest.edits) != 1 {
		t.Fatalf("want the caption edited once, got %+v", env.rest.edits)
	}
	edit := env.rest.edits[0]
	if edit.channelID != threadID || edit.messageID != captionMessageID ||
		edit.message.Content != "**Priya:** that's great [laughs]\n-# ✏️ fixed by Sam" {
		t.Errorf("got %+v", edit)
	}
}

func TestFixCaptionDoesntOverwriteSomeoneElsesFix(t *testing.T) {
	env := newTestEnv(t)
	// Someone saved a fix after this popup opened.
	putCaption(env, "**Jo:** fixed already\n-# ✏️ fixed by Jo", captionEditedAt.Add(time.Second))
	event, responded := fixSubmit(t, env, discord.PermissionManageChannels, "Priya", "mine", nil)

	if err := env.bot.onFixCaptionSubmit(event); err != nil {
		t.Fatal(err)
	}

	onlyAnswer(t, env, responded, "Someone changed this caption while you were editing, so I didn't save yours. It now says:\n> **Jo:** fixed already\n> -# ✏️ fixed by Jo")
	if len(env.rest.edits) != 0 {
		t.Error("the other fix must not be overwritten")
	}
}

func TestFixCaptionSubmitRefusals(t *testing.T) {
	tests := []struct {
		why         string
		permissions discord.Permissions
		speaker     string
		caption     string
		setup       func(env *testEnv)
		want        string
	}{
		{"lost their role while the popup was open", 0, "Priya", "hi", nil, "Only note-takers and moderators can fix captions."},
		{"blank speaker", discord.PermissionManageChannels, " ", "hi", nil, "can't be blank"},
		{"too long", discord.PermissionManageChannels, "Priya", strings.Repeat("x", captions.MaxMessageLength), nil, "That's too long for one Discord message"},
		{"caption deleted", discord.PermissionManageChannels, "Priya", "hi",
			func(env *testEnv) { env.rest.getErr = errors.New("unknown message") }, "I couldn't find that caption any more"},
		{"saving fails", discord.PermissionManageChannels, "Priya", "hi",
			func(env *testEnv) { env.rest.editErr = errors.New("discord unreachable") }, "I couldn't save that, try again in a moment. Here's what you wrote:\n> hi"},
	}
	for _, test := range tests {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			putCaption(env, "**Sam:** hi", captionEditedAt)
			if test.setup != nil {
				test.setup(env)
			}
			event, responded := fixSubmit(t, env, test.permissions, test.speaker, test.caption, nil)

			if err := env.bot.onFixCaptionSubmit(event); err != nil {
				t.Fatal(err)
			}
			onlyAnswer(t, env, responded, test.want)
			if test.why != "saving fails" && len(env.rest.edits) != 0 {
				t.Errorf("nothing should be saved, edited %+v", env.rest.edits)
			}
		})
	}
}

func TestFixCaptionSubmitWithABrokenCustomID(t *testing.T) {
	env := newTestEnv(t)
	event, responded := fixSubmit(t, env, discord.PermissionManageChannels, "Priya", "hi", nil)
	event.Vars["message"] = "not-a-number"

	if err := env.bot.onFixCaptionSubmit(event); err != nil {
		t.Fatal(err)
	}
	onlyAnswer(t, env, responded, "Something went wrong with that popup")
}

func TestFixCaptionSubmitWhenThinkingFails(t *testing.T) {
	env := newTestEnv(t)
	event, _ := fixSubmit(t, env, discord.PermissionManageChannels, "Priya", "hi", nil)
	event.Respond = func(discord.InteractionResponseType, discord.InteractionResponseData, ...rest.RequestOpt) error {
		return errors.New("discord unreachable")
	}

	if err := env.bot.onFixCaptionSubmit(event); err == nil {
		t.Fatal("the error should be returned so middleware.Go logs it")
	}
	if len(env.rest.edits) != 0 {
		t.Error("nothing should be saved")
	}
}

// ---------- routing ----------

func TestRouterSendsEverythingToItsHandler(t *testing.T) {
	r := New(nil, nil).Router()
	command := discord.InteractionTypeApplicationCommand
	for _, test := range []struct {
		path string
		kind discord.ApplicationCommandType
	}{
		{"/transcribe/start", discord.ApplicationCommandTypeSlash},
		{"/transcribe/stop", discord.ApplicationCommandTypeSlash},
		{"/captions-setup", discord.ApplicationCommandTypeSlash},
		{"/Fix caption", discord.ApplicationCommandTypeMessage},
	} {
		if !r.Match(test.path, command, int(test.kind)) {
			t.Errorf("nothing handles %s", test.path)
		}
	}

	// The popup's custom ID must reach the submit handler.
	customID := fixCaptionPopup(threadID, captionMessageID, captionEditedAt.UnixNano(), "Sam", "hi").CustomID
	if !r.Match(customID, discord.InteractionTypeModalSubmit, 0) {
		t.Errorf("the popup %q wouldn't reach the submit handler", customID)
	}

	// Fix caption is registered with Discord, for servers only.
	found := false
	for _, c := range Commands {
		if message, ok := c.(discord.MessageCommandCreate); ok && message.Name == "Fix caption" &&
			len(message.Contexts) == 1 && message.Contexts[0] == discord.InteractionContextTypeGuild {
			found = true
		}
	}
	if !found {
		t.Error("Fix caption should be registered as a server-only message command")
	}
}

func TestQuote(t *testing.T) {
	if got := quote("one\ntwo"); got != "> one\n> two" {
		t.Errorf("got %q", got)
	}
}

func TestFixCaptionSubmitInDM(t *testing.T) {
	env := newTestEnv(t)
	var interaction discord.ModalSubmitInteraction
	raw := `{"id":"9","type":5,"application_id":"8","token":"tok","version":1,"user":{"id":"3000","username":"sam123"},` +
		`"data":{"custom_id":"/fix-caption/1/2/3","components":[]}}`
	if err := json.Unmarshal([]byte(raw), &interaction); err != nil {
		t.Fatal(err)
	}
	var responded []discord.InteractionResponseType
	event := &handler.ModalEvent{
		ModalSubmitInteractionCreate: &events.ModalSubmitInteractionCreate{
			GenericEvent:           events.NewGenericEvent(env.client, 0, 0),
			ModalSubmitInteraction: interaction,
			Respond: func(kind discord.InteractionResponseType, _ discord.InteractionResponseData, _ ...rest.RequestOpt) error {
				responded = append(responded, kind)
				return nil
			},
		},
		Vars: map[string]string{"channel": "1", "message": "2", "edited": "3"},
		Ctx:  context.Background(),
	}

	if err := env.bot.onFixCaptionSubmit(event); err != nil {
		t.Fatal(err)
	}
	onlyAnswer(t, env, &responded, "I only work inside a server.")
}
