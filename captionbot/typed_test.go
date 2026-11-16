package captionbot

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// typedIn builds a message someone typed in a channel, as Discord sends it.
func typedIn(channelID, messageID snowflake.ID, content string) discord.Message {
	return discord.Message{
		ID:        messageID,
		ChannelID: channelID,
		GuildID:   idPtr(serverID),
		Type:      discord.MessageTypeDefault,
		Author:    discord.User{ID: otherMemberID, Username: "jo_99", GlobalName: strPtr("Jo")},
		Content:   content,
	}
}

func (env *testEnv) messageEvent(message discord.Message) *events.GenericGuildMessage {
	return &events.GenericGuildMessage{
		GenericEvent: events.NewGenericEvent(env.client, 0, 0),
		MessageID:    message.ID,
		Message:      message,
		ChannelID:    message.ChannelID,
		GuildID:      serverID,
	}
}

func (env *testEnv) typed(message discord.Message) {
	env.bot.OnMessageCreate(&events.GuildMessageCreate{GenericGuildMessage: env.messageEvent(message)})
}

func (env *testEnv) edited(message discord.Message) {
	env.bot.OnMessageUpdate(&events.GuildMessageUpdate{GenericGuildMessage: env.messageEvent(message)})
}

// stopAndRead stops captions and returns what the thread shows once every
// post and edit has gone through, oldest message first.
func (env *testEnv) stopAndRead(t *testing.T) []string {
	t.Helper()
	event, _ := env.command(t, moderator, stopCommand())
	if err := env.bot.onTranscribeStop(event); err != nil {
		t.Fatal(err)
	}
	env.waitForPosters(t)

	shown := map[snowflake.ID]string{}
	var order []snowflake.ID
	for _, posted := range env.rest.posted {
		if posted.channelID == threadID {
			shown[posted.messageID] = posted.message.Content
			order = append(order, posted.messageID)
		}
	}
	for _, edit := range env.rest.edits {
		if edit.channelID == threadID {
			shown[edit.messageID] = edit.message.Content
		}
	}
	var texts []string
	for _, id := range order {
		texts = append(texts, shown[id])
	}
	return texts
}

func copiesIn(thread []string) []string {
	var copies []string
	for _, text := range thread {
		if strings.Contains(text, "(typed):**") {
			copies = append(copies, text)
		}
	}
	return copies
}

func TestTypedMessagesAreCopiedIntoTheThread(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.typed(typedIn(meetingChannelID, 600, "can you *hear* me?"))
	reply := typedIn(meetingChannelID, 601, "yes")
	reply.Type = discord.MessageTypeReply
	env.typed(reply)

	thread := env.stopAndRead(t)
	want := []string{"**Jo (typed):** can you *hear* me?", "**Jo (typed):** yes"}
	if got := copiesIn(thread); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("want %q copied (their own formatting kept), thread %q", want, thread)
	}
	if last := thread[len(thread)-1]; !strings.HasPrefix(last, "⏹️") {
		t.Errorf("the copies should come before the closing line, thread %q", thread)
	}
}

func TestTypedEditsAreFollowed(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)

	env.typed(typedIn(meetingChannelID, 600, "helo"))
	env.edited(typedIn(meetingChannelID, 600, "hello"))
	env.edited(typedIn(lockedChannelID, 600, "not this call")) // elsewhere: ignored
	env.edited(typedIn(meetingChannelID, 600, "  "))           // nothing to copy: ignored

	if got := copiesIn(env.stopAndRead(t)); len(got) != 1 || got[0] != "**Jo (typed):** hello" {
		t.Errorf("the copy should follow the edit, got %q", got)
	}
}

func TestTypedMessagesThatArentCopied(t *testing.T) {
	bot := typedIn(meetingChannelID, 602, "beep")
	bot.Author.Bot = true
	webhook := typedIn(meetingChannelID, 603, "from a webhook")
	webhook.WebhookID = idPtr(1)
	pinned := typedIn(meetingChannelID, 604, "")
	pinned.Type = discord.MessageTypeChannelPinnedMessage

	for _, test := range []struct {
		why     string
		message discord.Message
	}{
		{"another channel", typedIn(lockedChannelID, 600, "hi")},
		{"a bot", bot},
		{"a webhook", webhook},
		{"a system message", pinned},
		{"nothing to copy", typedIn(meetingChannelID, 605, "   ")},
	} {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			env.startCaptions(t)
			env.typed(test.message)
			if got := copiesIn(env.stopAndRead(t)); len(got) != 0 {
				t.Errorf("nothing should be copied, got %q", got)
			}
		})
	}
}

func TestTypedMessagesWithoutCaptionsRunningAreIgnored(t *testing.T) {
	env := newTestEnv(t)
	env.typed(typedIn(meetingChannelID, 600, "hi")) // no call: must not panic or post
	if len(env.rest.posted) != 0 {
		t.Errorf("nothing should be posted, got %+v", env.rest.posted)
	}
}

// The no-captions role is about voice: what they type is still copied.
func TestTypedMessagesFromSomeoneWithTheNoCaptionsRoleAreCopied(t *testing.T) {
	env := newTestEnv(t)
	env.startCaptions(t)
	message := typedIn(meetingChannelID, 600, "typing instead")
	message.Member = &discord.Member{RoleIDs: []snowflake.ID{noCaptionsRoleID}}

	env.typed(message)

	if got := copiesIn(env.stopAndRead(t)); len(got) != 1 {
		t.Errorf("want their typed message copied, got %q", got)
	}
}

func TestTypedMessage(t *testing.T) {
	withFiles := func(text string, files int) discord.Message {
		message := typedIn(meetingChannelID, 700, text)
		message.Attachments = make([]discord.Attachment, files)
		return message
	}
	link := fmt.Sprintf("https://discord.com/channels/%d/%d/700", serverID, meetingChannelID)

	for _, test := range []struct {
		why     string
		message discord.Message
		want    string
	}{
		{"one file", withFiles("look", 1), "**Jo (typed):** look *[sent an attachment: " + link + "]*"},
		{"several files", withFiles("look", 3), "**Jo (typed):** look *[sent 3 attachments: " + link + "]*"},
		{"files only", withFiles("", 1), "**Jo (typed):** *[sent an attachment: " + link + "]*"},
	} {
		got, ok := typedMessage(test.message)
		if !ok || got != test.want {
			t.Errorf("%s: got (%q, %v), want %q", test.why, got, ok, test.want)
		}
	}

	// Discord refuses messages over 2000 characters: the text is cut, keeping
	// the name and the file note.
	long, _ := typedMessage(withFiles(strings.Repeat("a", 3000), 1))
	if n := utf8.RuneCountInString(long); n > 2000 || !strings.HasPrefix(long, "**Jo (typed):** aaa") ||
		!strings.HasSuffix(long, "… *[sent an attachment: "+link+"]*") {
		t.Errorf("a long message should be cut to fit (got %d characters): ...%q", n, long[len(long)-80:])
	}
}

func TestTypedName(t *testing.T) {
	message := typedIn(meetingChannelID, 700, "hi")
	if got := typedName(message); got != "Jo" {
		t.Errorf("no nickname: want the display name, got %q", got)
	}

	message.Member = &discord.Member{Nick: strPtr("xX_Jo_Xx")}
	if got := typedName(message); got != "xX_Jo_Xx" {
		t.Errorf("a nickname wins, got %q", got)
	}
	if got, _ := typedMessage(message); got != `**xX\_Jo\_Xx (typed):** hi` {
		t.Errorf("the name should show as typed, got %q", got)
	}

	message.Member = &discord.Member{} // in the server, no nickname
	message.Author.GlobalName = nil
	if got := typedName(message); got != "jo_99" {
		t.Errorf("no nickname or display name: want the username, got %q", got)
	}
}

func TestCallNoticeMentionsTyping(t *testing.T) {
	if notice := callNotice(testCall, threadID); !strings.Contains(notice, "Rather type? What you send in this chat goes into the captions too.") {
		t.Errorf("the notice should say typed messages are copied: %q", notice)
	}
}
