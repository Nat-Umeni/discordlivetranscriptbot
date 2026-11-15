// Package captions is where captions go and what they look like: the
// captions thread (posted and edited live), the terminal, speakers' names,
// and the caption message format that note-takers' fixes read and write.
package captions

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
)

const (
	// SpeakingLine is shown under a caption while the person is still
	// talking. "-#" is Discord's small grey text.
	SpeakingLine = "\n-# speaking…"

	// MaxMessageLength is Discord's limit for one message.
	MaxMessageLength = 2000

	// MaxTextLength is the most of a caption's text that's shown, leaving
	// room in the message for the name and the "speaking…" line.
	MaxTextLength = 1900

	// Starts the line under a fixed caption, e.g. "-# ✏️ fixed by Jo".
	fixedByPrefix = "-# ✏️ fixed by "
)

var (
	ErrBlank   = errors.New("speaker and caption can't be blank")
	ErrTooLong = errors.New("too long for one Discord message")
)

// NoPings stops a message notifying anyone. Without it, a "Started by @Sam"
// line would ping Sam, a role mention would ping everyone with it, and a
// title or caption containing @everyone would ping the whole server.
var NoPings = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}}

// Message is what a caption looks like in the thread:
//
//	**Sam:** so I rang him
//	-# speaking…
//
// with the "speaking…" line gone once the sentence has ended. A notice is the
// one-off line for someone who isn't being captioned.
func Message(name, text string, final, notice bool) string {
	if notice {
		return fmt.Sprintf("*[%s isn't being captioned]*", EscapeMarkdown(name))
	}

	message := fmt.Sprintf("**%s:** %s", EscapeMarkdown(name), CutToLength(EscapeMarkdown(text), MaxTextLength))
	if !final {
		message += SpeakingLine
	}
	return message
}

// Fixed is a caption rewritten by a note-taker:
//
//	**Priya:** that's great [laughs]
//	-# ✏️ fixed by Jo
//
// Picked tone tags go at the end. Everyone's text shows exactly as typed
// (markdown is escaped, as in live captions).
func Fixed(speaker, text string, tags []string, fixer string) (string, error) {
	speaker = strings.TrimSpace(speaker)
	text = strings.TrimSpace(text)
	if speaker == "" || text == "" {
		return "", ErrBlank
	}
	if len(tags) > 0 {
		text += " " + strings.Join(tags, " ")
	}

	content := fmt.Sprintf("**%s:** %s\n%s%s", EscapeMarkdown(speaker), EscapeMarkdown(text), fixedByPrefix, EscapeMarkdown(fixer))
	if utf8.RuneCountInString(content) > MaxMessageLength {
		return "", ErrTooLong
	}
	return content, nil
}

// Parse reads the speaker and text back out of a caption message
// ("**Sam:** text", maybe with a "fixed by" line under it), so a fix can
// start from what's there. ok is false for anything that isn't a caption,
// like the thread's opening post or a notice.
func Parse(content string) (speaker, text string, ok bool) {
	if i := strings.LastIndex(content, "\n"+fixedByPrefix); i >= 0 {
		content = content[:i]
	}
	if !strings.HasPrefix(content, "**") {
		return "", "", false
	}
	end := strings.Index(content, ":** ")
	if end < 0 {
		return "", "", false
	}

	speaker = UnescapeMarkdown(content[len("**"):end])
	text = UnescapeMarkdown(content[end+len(":** "):])
	if speaker == "" {
		return "", "", false
	}
	return speaker, text, true
}

// markdownEscaper puts a backslash before the characters Discord treats as
// formatting, so they show as typed. Backslash itself goes first, or the
// backslashes added for the others would be escaped too.
var markdownEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, `|`, `\|`, "`", "\\`")

// EscapeMarkdown stops a name like xX_Sam_Xx turning into italics, or a
// caption's symbols changing how the message looks.
func EscapeMarkdown(text string) string {
	return markdownEscaper.Replace(text)
}

// UnescapeMarkdown undoes EscapeMarkdown, so a fix shows "xX_Sam_Xx", not
// "xX\_Sam\_Xx".
func UnescapeMarkdown(text string) string {
	var out strings.Builder
	escaped := false
	for _, r := range text {
		if !escaped && r == '\\' {
			escaped = true
			continue
		}
		if escaped && !strings.ContainsRune("\\*_~|`", r) {
			out.WriteRune('\\') // a backslash that wasn't escaping anything
		}
		out.WriteRune(r)
		escaped = false
	}
	if escaped {
		out.WriteRune('\\')
	}
	return out.String()
}

// CutToLength shortens text to at most limit characters, ending with "…" if
// it had to cut.
func CutToLength(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}
