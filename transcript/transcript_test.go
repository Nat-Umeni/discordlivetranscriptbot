package transcript

import (
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

const botID snowflake.ID = 2000

var start = time.Date(2026, 11, 15, 21, 5, 0, 0, time.UTC)

// fromBot is a message the bot posted, minutes after the call started.
func fromBot(minutes int, content string) discord.Message {
	return discord.Message{
		Type:      discord.MessageTypeDefault,
		Author:    discord.User{ID: botID, Bot: true},
		Content:   content,
		CreatedAt: start.Add(time.Duration(minutes) * time.Minute),
	}
}

func strPtr(s string) *string { return &s }

// A thread with one of everything, oldest first.
func sampleThread() []discord.Message {
	jo := discord.Message{
		ID: 700, ChannelID: 600, GuildID: &[]snowflake.ID{1}[0],
		Type:      discord.MessageTypeReply,
		Author:    discord.User{ID: 3001, Username: "jo_99", GlobalName: strPtr("Jo")},
		Content:   "she means the *June* meetup",
		CreatedAt: start.Add(2 * time.Minute),
	}
	added := fromBot(0, "")
	added.Type = discord.MessageTypeRecipientAdd // "X added Y to the thread"
	earlierTranscript := fromBot(3, "📄 Transcript")
	earlierTranscript.Attachments = make([]discord.Attachment, 2)

	return []discord.Message{
		fromBot(0, "📝 **Live captions: Weekly Meetup**\nI'm captioning…"), // opening post
		added,
		fromBot(0, "**Sam:** So I rang him."),
		fromBot(1, "**Jo (typed):** can you *hear* me"),
		fromBot(1, `**xX\_Priya\_Xx:** that's great \*laughs\* [laughs]`+"\n-# ✏️ fixed by Jo"),
		jo,
		fromBot(2, "*[Ali isn't being captioned]*"),
		earlierTranscript,
		fromBot(3, "**Sam:** and then I"+"\n-# speaking…"), // cut off mid-sentence
		fromBot(37, `⏹️ **Captions stopped** by xX\_Sam\_Xx.`),
	}
}

func TestLines(t *testing.T) {
	got := Lines(sampleThread(), botID)
	want := []Line{
		{At: start, Speaker: "Sam", Text: "So I rang him.", Markdown: "So I rang him."},
		{At: start.Add(time.Minute), Speaker: "Jo (typed)", Text: "can you *hear* me", Markdown: "can you *hear* me"},
		{At: start.Add(time.Minute), Speaker: "xX_Priya_Xx", Text: "that's great *laughs* [laughs]", Markdown: `that's great \*laughs\* [laughs]`},
		{At: start.Add(2 * time.Minute), Speaker: "Jo (comment)", Text: "she means the *June* meetup", Markdown: "she means the *June* meetup"},
		{At: start.Add(2 * time.Minute), Text: "[Ali isn't being captioned]", Markdown: "*[Ali isn't being captioned]*"},
		{At: start.Add(3 * time.Minute), Speaker: "Sam", Text: "and then I", Markdown: "and then I"},
		{At: start.Add(37 * time.Minute), Text: "Captions stopped by xX_Sam_Xx.", Markdown: `**Captions stopped** by xX\_Sam\_Xx.`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestCommentWithAttachments(t *testing.T) {
	message := discord.Message{
		ID: 700, ChannelID: 600, GuildID: &[]snowflake.ID{1}[0],
		Type:        discord.MessageTypeDefault,
		Author:      discord.User{ID: 3001, Username: "jo"},
		Attachments: make([]discord.Attachment, 1),
	}
	got := Lines([]discord.Message{message}, botID)
	if len(got) != 1 || got[0].Text != "*[sent an attachment: https://discord.com/channels/1/600/700]*" {
		t.Errorf("got %+v", got)
	}
}

func TestText(t *testing.T) {
	london := time.FixedZone("GMT", 0)
	got := Text("📝 Weekly Meetup - 15 Nov 2026", Lines(sampleThread(), botID), london)
	want := `📝 Weekly Meetup - 15 Nov 2026
Captions from 21:05 to 21:42. (typed) = typed in the call's chat, (comment) = posted in the thread.

[21:05] Sam: So I rang him.
[21:06] Jo (typed): can you *hear* me
[21:06] xX_Priya_Xx: that's great *laughs* [laughs]
[21:07] Jo (comment): she means the *June* meetup
[21:07] [Ali isn't being captioned]
[21:08] Sam: and then I
[21:42] Captions stopped by xX_Sam_Xx.
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Times are shown in the bot's own time zone.
	if paris := Text("x", Lines(sampleThread()[2:3], botID), time.FixedZone("CET", 3600)); !strings.Contains(paris, "[22:05] Sam") {
		t.Errorf("times should follow the time zone given, got:\n%s", paris)
	}
}

func TestMarkdown(t *testing.T) {
	got := Markdown("📝 Weekly_Meetup - 15 Nov 2026", Lines(sampleThread()[2:10], botID), time.UTC)
	want := `# 📝 Weekly\_Meetup - 15 Nov 2026

*Captions from 21:05 to 21:42. (typed) = typed in the call's chat, (comment) = posted in the thread.*

**21:05 · Sam:** So I rang him.

**21:06 · Jo (typed):** can you *hear* me

**21:06 · xX\_Priya\_Xx:** that's great \*laughs\* [laughs]

**21:07 · Jo (comment):** she means the *June* meetup

**21:07** *[Ali isn't being captioned]*

**21:08 · Sam:** and then I

**21:42** **Captions stopped** by xX\_Sam\_Xx.

`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEmptyTranscript(t *testing.T) {
	if got := Text("Quiet", nil, time.UTC); got != "Quiet\nNothing was captioned.\n\n" {
		t.Errorf("got %q", got)
	}
}

func TestFileName(t *testing.T) {
	for _, test := range []struct{ title, want string }{
		{"📝 Weekly Meetup - 15 Nov 2026", "weekly-meetup-15-nov-2026.txt"},
		{"AGM: Q&A!!", "agm-q-a.txt"},
		{"Café ☕", "caf.txt"},
		{"📝 ✨", "transcript.txt"},
	} {
		if got := FileName(test.title, "txt"); got != test.want {
			t.Errorf("FileName(%q) = %q, want %q", test.title, got, test.want)
		}
	}
}
