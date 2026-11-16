package captionbot

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// People in a call who'd rather type than talk write in the voice channel's
// own chat. Each message is copied into the captions thread as
// "**Jo (typed):** …", in order with the spoken captions, so the thread shows
// the whole conversation. Everyone's messages are copied, including people
// with the no-captions role: that role is about not having their voice
// captioned, and what they type is already public in the call's chat.

// OnMessageCreate copies a new message from a captioned call's chat into its
// captions thread.
func (b *Bot) OnMessageCreate(e *events.GuildMessageCreate) {
	call := b.callIn(e.GuildID, e.ChannelID)
	if call == nil || !isPersonsMessage(e.Message) {
		return
	}
	if content, ok := typedMessage(e.Message); ok {
		call.poster.Typed(e.MessageID, content)
	}
}

// OnMessageUpdate updates the copy when someone edits a message they typed.
// Deleting the original leaves the copy where it is.
func (b *Bot) OnMessageUpdate(e *events.GuildMessageUpdate) {
	call := b.callIn(e.GuildID, e.ChannelID)
	if call == nil || !isPersonsMessage(e.Message) {
		return
	}
	if content, ok := typedMessage(e.Message); ok {
		call.poster.EditTyped(e.MessageID, content)
	}
}

// callIn returns the call being captioned in this server if channelID is its
// voice channel (whose chat is where people type), or nil.
func (b *Bot) callIn(serverID, channelID snowflake.ID) *liveCall {
	b.callsMu.Lock()
	defer b.callsMu.Unlock()
	call, found := b.calls[serverID]
	if !found || call.voiceChannelID != channelID {
		return nil
	}
	return call
}

// isPersonsMessage says whether a message is something a person typed: not
// from a bot (including this one's own notice), not a webhook, and an ordinary
// message or reply rather than a system message like "pinned a message".
func isPersonsMessage(message discord.Message) bool {
	if message.Author.Bot || message.WebhookID != nil {
		return false
	}
	return message.Type == discord.MessageTypeDefault || message.Type == discord.MessageTypeReply
}

// typedMessage is the copy of a typed message for the captions thread:
//
//	**Jo (typed):** can you hear me *[sent an attachment: <link>]*
//
// The sender's own formatting is kept; their name is shown as typed. Files
// aren't copied, just noted with a link back to the original. ok is false when
// there's nothing to copy (no text and no files).
func typedMessage(message discord.Message) (content string, ok bool) {
	text := strings.TrimSpace(message.Content)
	note := captions.AttachmentNote(message)
	if text == "" && note == "" {
		return "", false
	}
	if text != "" && note != "" {
		note = " " + note
	}

	prefix := fmt.Sprintf("**%s (typed):** ", captions.EscapeMarkdown(captions.SenderName(message)))
	// Cut the text, not the name or the note, to fit Discord's limit.
	room := captions.MaxMessageLength - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(note)
	return prefix + captions.CutToLength(text, room) + note, true
}
