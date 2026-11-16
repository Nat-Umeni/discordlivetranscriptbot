// Package transcript turns a captions thread back into a transcript people
// can download: who said or typed what, and when, with note-takers' fixes
// already in (the thread holds the fixed text) and their comments marked.
package transcript

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// Line is one line of a transcript.
type Line struct {
	At time.Time
	// Speaker is who said or wrote it, e.g. "Sam", "Jo (typed)" or
	// "Jo (comment)". Empty for notes like "Captions stopped by Sam."
	Speaker string
	// Text is plain text; Markdown is the same with Discord's formatting kept,
	// for the .md file.
	Text, Markdown string
}

// Lines reads a captions thread's messages (oldest first) into transcript
// lines. botID is this bot, so its captions and notes can be told apart from
// other people's comments.
func Lines(messages []discord.Message, botID snowflake.ID) []Line {
	var lines []Line
	for _, message := range messages {
		// System messages like "X added Y to the thread" aren't part of it.
		if message.Type != discord.MessageTypeDefault && message.Type != discord.MessageTypeReply {
			continue
		}
		if message.Author.ID == botID {
			if line, ok := botLine(message); ok {
				lines = append(lines, line)
			}
			continue
		}
		lines = append(lines, commentLine(message))
	}
	return lines
}

// botLine reads one of the bot's own messages: a caption or typed copy, a
// notice, or a closing line. ok is false for the rest (the opening post,
// earlier transcripts).
func botLine(message discord.Message) (Line, bool) {
	if len(message.Attachments) > 0 {
		return Line{}, false // an earlier transcript
	}

	// A caption still showing "speaking…" if captions were cut off mid-sentence.
	content := strings.TrimSuffix(message.Content, captions.SpeakingLine)
	if speaker, text, ok := captions.Split(content); ok {
		return Line{
			At:       message.CreatedAt,
			Speaker:  captions.UnescapeMarkdown(speaker),
			Text:     captions.UnescapeMarkdown(text),
			Markdown: text,
		}, true
	}

	// "*[Ali isn't being captioned]*" and "⏹️ **Captions stopped** by Sam."
	if strings.HasPrefix(content, "*[") || strings.HasPrefix(content, "⏹️") {
		markdown := strings.TrimPrefix(content, "⏹️ ")
		plain := strings.NewReplacer("**", "", "*[", "[", "]*", "]").Replace(markdown)
		return Line{At: message.CreatedAt, Text: captions.UnescapeMarkdown(plain), Markdown: markdown}, true
	}
	return Line{}, false
}

// commentLine is a message someone else posted in the thread, e.g. a
// note-taker adding context.
func commentLine(message discord.Message) Line {
	text := strings.TrimSpace(message.Content)
	if note := captions.AttachmentNote(message); note != "" {
		text = strings.TrimSpace(text + " " + note)
	}
	return Line{
		At:       message.CreatedAt,
		Speaker:  captions.SenderName(message) + " (comment)",
		Text:     text,
		Markdown: text,
	}
}

// Text is the transcript as a plain text file.
func Text(title string, lines []Line, loc *time.Location) string {
	var out strings.Builder
	out.WriteString(title + "\n" + summary(lines, loc) + "\n\n")
	for _, line := range lines {
		fmt.Fprintf(&out, "[%s] ", line.At.In(loc).Format("15:04"))
		if line.Speaker != "" {
			out.WriteString(line.Speaker + ": ")
		}
		out.WriteString(line.Text + "\n")
	}
	return out.String()
}

// Markdown is the transcript as a markdown file, with bold names and each
// person's own formatting kept.
func Markdown(title string, lines []Line, loc *time.Location) string {
	var out strings.Builder
	out.WriteString("# " + captions.EscapeMarkdown(title) + "\n\n*" + summary(lines, loc) + "*\n\n")
	for _, line := range lines {
		at := line.At.In(loc).Format("15:04")
		if line.Speaker == "" {
			fmt.Fprintf(&out, "**%s** %s\n\n", at, line.Markdown)
			continue
		}
		fmt.Fprintf(&out, "**%s · %s:** %s\n\n", at, captions.EscapeMarkdown(line.Speaker), line.Markdown)
	}
	return out.String()
}

// summary is the line under the title.
func summary(lines []Line, loc *time.Location) string {
	if len(lines) == 0 {
		return "Nothing was captioned."
	}
	return fmt.Sprintf("Captions from %s to %s. (typed) = typed in the call's chat, (comment) = posted in the thread.",
		lines[0].At.In(loc).Format("15:04"), lines[len(lines)-1].At.In(loc).Format("15:04"))
}

// FileName turns a thread's name into a file name, e.g.
// "📝 Weekly Meetup - 15 Nov 2026" → "weekly-meetup-15-nov-2026.txt".
func FileName(title, extension string) string {
	var name strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if dash && name.Len() > 0 {
				name.WriteByte('-')
			}
			name.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	if name.Len() == 0 {
		return "transcript." + extension
	}
	return name.String() + "." + extension
}
