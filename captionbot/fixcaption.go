package captionbot

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// "Fix caption" lets note-takers correct a finished caption: change the words,
// credit the right speaker (a guest, or "Room mic" → the real person) and add
// tone tags like [laughs]. Right-click a caption → Apps → Fix caption opens a
// popup pre-filled with the caption; saving rewrites the message with a
// "fixed by" line so readers know a person corrected it.

// fixCaptionCommand puts "Fix caption" in the Apps menu of every message. It
// can't be limited to note-takers by Discord (that only works by permission,
// and note-takers aren't necessarily admins), so the handler checks instead.
var fixCaptionCommand = discord.MessageCommandCreate{
	Name:     "Fix caption",
	Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild},
}

// toneTags are the quick picks in the popup. [Bracketed] descriptions are the
// captioning convention Deaf and hard-of-hearing readers are used to. Note-takers
// can also type any tag straight into the caption.
var toneTags = []string{
	"[laughs]", "[laughing]", "[sarcastic]", "[joking]", "[serious]",
	"[whispering]", "[shouting]", "[sighs]", "[crying]", "[applause]",
	"[music]", "[crosstalk]", "[inaudible]", "[pause]",
}

const (
	// The popup's fields, matched up again when it's submitted.
	speakerField = "speaker"
	captionField = "caption"
	tagsField    = "tags"

	maxSpeakerLength = 80
)

// captionFixMu makes fixes happen one at a time, so two note-takers saving the
// same caption at once can't both pass the "has it changed?" check.
var captionFixMu sync.Mutex

// onFixCaption opens the popup for a caption someone right-clicked.
func (b *Bot) onFixCaption(data discord.MessageCommandInteractionData, e *handler.CommandEvent) error {
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	if !b.canManageTranscription(e, *guildID) {
		return createHiddenMessageForCommandCaller(e, "Only note-takers and moderators can fix captions.")
	}

	target := data.TargetMessage()
	notACaption := "That isn't one of my captions. Fix caption only works on captions I've posted."
	if target.Author.ID != e.Client().ID() {
		return createHiddenMessageForCommandCaller(e, notACaption)
	}

	// A caption still growing would be overwritten by the next words, and
	// would overwrite the fix.
	if strings.HasSuffix(target.Content, captions.SpeakingLine) {
		return createHiddenMessageForCommandCaller(e, "They're still talking. Fix it once the caption's finished (the \"speaking…\" line goes when it is).")
	}

	speaker, text, ok := captions.Parse(target.Content)
	if !ok {
		return createHiddenMessageForCommandCaller(e, notACaption)
	}

	return e.Modal(fixCaptionPopup(target.ChannelID, target.ID, editedMark(target), speaker, text))
}

// fixCaptionPopup is the popup, pre-filled with the caption as it is now. Its
// custom ID carries the message and when it was last edited, so the submit can
// tell whether someone else changed it in the meantime without storing
// anything.
func fixCaptionPopup(channelID, messageID snowflake.ID, edited int64, speaker, text string) discord.ModalCreate {
	options := make([]discord.StringSelectMenuOption, len(toneTags))
	for i, tag := range toneTags {
		options[i] = discord.NewStringSelectMenuOption(tag, tag)
	}
	noneNeeded := 0
	tags := discord.NewStringSelectMenu(tagsField, "Pick any to add at the end (optional)", options...).
		WithMaxValues(len(toneTags))
	tags.MinValues = &noneNeeded
	tags.Required = false

	captionLabel := discord.NewLabel("Caption", discord.NewParagraphTextInput(captionField).
		WithValue(text).WithRequired(true).WithMaxLength(captions.MaxTextLength))
	captionLabel.Description = "Fix any words, and type tags like [laughs] anywhere."

	speakerLabel := discord.NewLabel("Speaker", discord.NewShortTextInput(speakerField).
		WithValue(speaker).WithRequired(true).WithMaxLength(maxSpeakerLength))
	speakerLabel.Description = "Anyone's name, e.g. a guest using someone else's mic."

	return discord.NewModalCreate(
		fmt.Sprintf("/fix-caption/%d/%d/%d", channelID, messageID, edited),
		"Fix caption",
		speakerLabel,
		captionLabel,
		discord.NewLabel("Tone tags", tags),
	)
}

// onFixCaptionSubmit saves a fix from the popup.
func (b *Bot) onFixCaptionSubmit(e *handler.ModalEvent) error {
	// Answer straight away with a hidden "thinking…": fetching and editing the
	// caption can take longer than the 3 seconds Discord allows.
	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}
	answer := func(text string) error {
		_, err := e.UpdateInteractionResponse(discord.MessageUpdate{Content: &text})
		return err
	}

	guildID := e.GuildID()
	if guildID == nil {
		return answer("I only work inside a server.")
	}
	// Checked again: someone's roles can change while the popup is open.
	if !b.memberCanManageTranscription(e.Member(), *guildID) {
		return answer("Only note-takers and moderators can fix captions.")
	}

	channelID, err1 := snowflake.Parse(e.Vars["channel"])
	messageID, err2 := snowflake.Parse(e.Vars["message"])
	edited, err3 := strconv.ParseInt(e.Vars["edited"], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		log.Printf("fix caption popup with a broken custom ID %q: %v", e.Data.CustomID, err)
		return answer("Something went wrong with that popup, try Fix caption again.")
	}

	speaker := e.Data.Text(speakerField)
	text := e.Data.Text(captionField)
	content, err := captions.Fixed(speaker, text, e.Data.StringValues(tagsField), e.Member().EffectiveName())
	if errors.Is(err, captions.ErrBlank) {
		return answer("The speaker and caption can't be blank, so I didn't save that.")
	}
	if err != nil {
		return answer(fmt.Sprintf("That's too long for one Discord message (%d characters), so I didn't save it. Here's what you wrote, to shorten and try again:\n%s",
			captions.MaxMessageLength, quote(text)))
	}

	captionFixMu.Lock()
	defer captionFixMu.Unlock()

	current, err := e.Client().Rest.GetMessage(channelID, messageID)
	if err != nil {
		log.Printf("couldn't fetch the caption to fix: %v", err)
		return answer("I couldn't find that caption any more, so I didn't save your fix.")
	}

	// Someone else saved a fix while this popup was open: don't overwrite it.
	if editedMark(*current) != edited {
		return answer("Someone changed this caption while you were editing, so I didn't save yours. It now says:\n" +
			quote(current.Content) + "\nRun Fix caption again to change it.")
	}

	_, err = e.Client().Rest.UpdateMessage(channelID, messageID, discord.MessageUpdate{Content: &content, AllowedMentions: captions.NoPings})
	if err != nil {
		log.Printf("couldn't save a caption fix: %v", err)
		return answer("I couldn't save that, try again in a moment. Here's what you wrote:\n" + quote(text))
	}

	return answer("Fixed ✓")
}

// editedMark is when a message was last edited, as a number for the popup's
// custom ID; 0 if it never has been.
func editedMark(message discord.Message) int64 {
	if message.EditedTimestamp == nil {
		return 0
	}
	return message.EditedTimestamp.UnixNano()
}

// quote shows text as a Discord block quote.
func quote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}
