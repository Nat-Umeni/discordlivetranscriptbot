package captionbot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// transcriptFiles finds the files attached to the last post in a channel
// with this heading.
func transcriptFiles(t *testing.T, env *testEnv, channelID snowflake.ID, heading string) map[string]string {
	t.Helper()
	for i := len(env.rest.posted) - 1; i >= 0; i-- {
		posted := env.rest.posted[i]
		if posted.channelID == channelID && posted.message.Content == heading {
			noPingsIn(t, posted.message)
			return posted.files
		}
	}
	t.Fatalf("no %q post in %v; posted %+v", heading, channelID, env.rest.posted)
	return nil
}

// oneOfEach finds the .txt and .md files, checking there are just those two.
func oneOfEach(t *testing.T, files map[string]string, namePrefix string) (text, markdown string) {
	t.Helper()
	if len(files) != 2 {
		t.Fatalf("want a .txt and a .md file, got %v", files)
	}
	for name, content := range files {
		if !strings.HasPrefix(name, namePrefix) {
			t.Errorf("file %q should be named after the thread (%s…)", name, namePrefix)
		}
		switch {
		case strings.HasSuffix(name, ".txt"):
			text = content
		case strings.HasSuffix(name, ".md"):
			markdown = content
		default:
			t.Errorf("unexpected file %q", name)
		}
	}
	return text, markdown
}

func TestStoppingPostsTheTranscript(t *testing.T) {
	env := newTestEnv(t)
	env.client.Caches.AddMember(discord.Member{GuildID: serverID,
		User: discord.User{ID: callerID, Username: "sam123", GlobalName: strPtr("Sam")}})
	call := env.startCaptions(t)
	call.poster.Caption(callerID, "So I rang him.", true)
	env.typed(typedIn(meetingChannelID, 600, "can you hear me"))
	env.rest.mu.Lock()
	env.rest.comments = append(env.rest.comments, discord.Message{
		ID: 9900, ChannelID: threadID, Type: discord.MessageTypeDefault,
		Author:  discord.User{ID: otherMemberID, Username: "jo_99", GlobalName: strPtr("Jo")},
		Content: "she means the June meetup", CreatedAt: postedAt.Add(30 * time.Minute),
	})
	env.rest.mu.Unlock()

	thread := env.stopAndRead(t)
	if thread[len(thread)-1] != transcriptHeading {
		t.Errorf("the transcript should be the last post, after the closing line: %q", thread)
	}

	text, markdown := oneOfEach(t, transcriptFiles(t, env, threadID, transcriptHeading), "weekly-meetup-")
	for _, want := range []string{
		"📝 Weekly Meetup - ",
		"Sam: So I rang him.",
		"Jo (typed): can you hear me",
		"Jo (comment): she means the June meetup",
		"Captions stopped by caller.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript should contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Live captions") {
		t.Errorf("the opening post isn't part of the transcript:\n%s", text)
	}
	if !strings.Contains(markdown, "· Sam:** So I rang him.") {
		t.Errorf("the .md file should have bold names:\n%s", markdown)
	}
}

func TestTranscriptWaitsForTheLastCaptionsThenGivesUp(t *testing.T) {
	logs := captureLog(t)
	env := newTestEnv(t)
	env.bot.posterWait = 20 * time.Millisecond
	stuck := captions.NewThreadPoster(env.rest, threadID, func(snowflake.ID) string { return "Sam" }, nil)
	defer stuck.Closed()

	env.bot.transcriptWhenPosted(env.client, &liveCall{poster: stuck, threadID: threadID, threadName: "x"})
	env.waitForPosters(t)

	if !strings.Contains(logs.String(), "never finished posting, so I didn't post its transcript") {
		t.Errorf("giving up should be logged, logged:\n%s", logs)
	}
	if len(env.rest.posted) != 0 {
		t.Errorf("no transcript should be posted, got %+v", env.rest.posted)
	}
}

func TestTranscriptFailuresAreLogged(t *testing.T) {
	for _, test := range []struct {
		why   string
		setup func(r *fakeRest)
		want  string
	}{
		{"reading the thread fails", func(r *fakeRest) { r.historyErr = errorString("no access") }, "reading the thread: no access"},
		{"posting fails", func(r *fakeRest) { r.postErrIn = map[snowflake.ID]error{threadID: errorString("no files allowed")} }, "posting it: no files allowed"},
	} {
		t.Run(test.why, func(t *testing.T) {
			logs := captureLog(t)
			env := newTestEnv(t)
			test.setup(env.rest)
			done := captions.NewThreadPoster(env.rest, threadID, func(snowflake.ID) string { return "Sam" }, nil)
			done.Closed()

			env.bot.transcriptWhenPosted(env.client, &liveCall{poster: done, threadID: threadID, threadName: "x"})
			env.waitForPosters(t)

			if !strings.Contains(logs.String(), "couldn't post the transcript in thread") || !strings.Contains(logs.String(), test.want) {
				t.Errorf("want %q logged, logged:\n%s", test.want, logs)
			}
		})
	}
}

// Discord hands messages back 100 at a time, newest first: all of them should
// come back, oldest first.
func TestFetchThreadReadsEveryPage(t *testing.T) {
	env := newTestEnv(t)
	const longThread snowflake.ID = 7100
	for i := range 250 {
		env.rest.comments = append(env.rest.comments, discord.Message{
			ID: longThread + snowflake.ID(1+i), ChannelID: longThread, Type: discord.MessageTypeDefault,
			Author: discord.User{ID: otherMemberID, Username: "jo"}, Content: fmt.Sprint(i),
		})
	}

	messages, err := fetchThread(env.client, longThread)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 250 {
		t.Fatalf("want all 250 messages, got %d", len(messages))
	}
	for i, message := range messages {
		if message.Content != fmt.Sprint(i) {
			t.Fatalf("message %d is %q: want oldest first", i, message.Content)
		}
	}
}

// ---------- /transcribe export ----------

func exportCommand() string {
	return `{"id":"1","name":"transcribe","type":1,"options":[{"name":"export","type":1}]}`
}

// captionsThreadJSON is the thread a command was run in, as Discord sends it.
func captionsThreadJSON(ownerID, parentID snowflake.ID) string {
	return fmt.Sprintf(`{"id":"%d","type":11,"guild_id":"%d","parent_id":"%d","owner_id":"%d","name":"📝 Weekly Meetup - 15 Nov 2026"}`,
		threadID, serverID, parentID, ownerID)
}

func inChannel(who caller, channelJSON string) caller {
	who.inChannel = channelJSON
	return who
}

func TestExportRebuildsTheTranscript(t *testing.T) {
	env := newTestEnv(t)
	env.rest.mu.Lock()
	env.rest.posted = append(env.rest.posted,
		postedMessage{channelID: threadID, messageID: 9001, message: discord.MessageCreate{Content: "**Sam:** fixed text"}})
	env.rest.mu.Unlock()
	event, sent := env.command(t, inChannel(moderator, captionsThreadJSON(botID, captionsChannelID)), exportCommand())

	if err := env.bot.onTranscribeExport(event); err != nil {
		t.Fatal(err)
	}

	if len(sent.messages) != 1 || !sent.messages[0].Flags.Has(discord.MessageFlagEphemeral) {
		t.Errorf("should first answer with a hidden 'thinking…', got %+v", sent.messages)
	}
	if len(env.rest.responses) != 1 || env.rest.responses[0] != "Posted the updated transcript in this thread." {
		t.Errorf("got %q", env.rest.responses)
	}
	text, _ := oneOfEach(t, transcriptFiles(t, env, threadID, "📄 Updated transcript (rebuilt by caller)"), "weekly-meetup-15-nov-2026")
	if !strings.Contains(text, "Sam: fixed text") {
		t.Errorf("the rebuilt transcript should have the fixed text:\n%s", text)
	}
}

func TestExportRefusals(t *testing.T) {
	textChannel := fmt.Sprintf(`{"id":"%d","type":0,"guild_id":"%d","name":"captions"}`, captionsChannelID, serverID)
	for _, test := range []struct {
		why  string
		who  caller
		want string
	}{
		{"in a DM", caller{id: callerID, inDM: true}, "I only work inside a server"},
		{"not a note-taker or moderator", inChannel(plainMember, captionsThreadJSON(botID, captionsChannelID)), "Only note-takers and moderators can rebuild transcripts."},
		{"not in a thread", inChannel(moderator, textChannel), "Run /transcribe export inside a captions thread"},
		{"a thread the bot didn't start", inChannel(moderator, captionsThreadJSON(otherMemberID, captionsChannelID)), "Run /transcribe export inside a captions thread"},
		{"a thread somewhere else", inChannel(moderator, captionsThreadJSON(botID, meetingChannelID)), "Run /transcribe export inside a captions thread"},
	} {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			event, sent := env.command(t, test.who, exportCommand())
			if err := env.bot.onTranscribeExport(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, test.want)
			if len(env.rest.posted) != 0 {
				t.Errorf("nothing should be posted, got %+v", env.rest.posted)
			}
		})
	}
}

func TestExportWhenRebuildingFails(t *testing.T) {
	captureLog(t)
	env := newTestEnv(t)
	env.rest.historyErr = errorString("no access")
	event, _ := env.command(t, inChannel(moderator, captionsThreadJSON(botID, captionsChannelID)), exportCommand())

	if err := env.bot.onTranscribeExport(event); err != nil {
		t.Fatal(err)
	}
	if len(env.rest.responses) != 1 || env.rest.responses[0] != "I couldn't rebuild the transcript, try again in a moment." {
		t.Errorf("got %q", env.rest.responses)
	}
}

func TestExportWhenThinkingFails(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, inChannel(moderator, captionsThreadJSON(botID, captionsChannelID)), exportCommand())
	sent.err = errorString("discord unreachable")

	if err := env.bot.onTranscribeExport(event); err == nil {
		t.Fatal("the error should be returned so middleware.Go logs it")
	}
	if len(env.rest.posted) != 0 {
		t.Error("nothing should be posted")
	}
}
