package captionbot

import (
	"encoding/json"
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

// threadJSON is a captions thread as Discord describes it in full (when
// looked up).
func threadJSON(ownerID, parentID snowflake.ID) string {
	return fmt.Sprintf(`{"id":"%d","type":11,"guild_id":"%d","parent_id":"%d","owner_id":"%d","name":"📝 Weekly Meetup - 15 Nov 2026"}`,
		threadID, serverID, parentID, ownerID)
}

// sentWithCommand is the thread as a command run inside it describes it: no
// owner, so the bot has to look the thread up to know who started it.
var sentWithCommand = fmt.Sprintf(`{"id":"%d","type":11,"guild_id":"%d","parent_id":"%d","name":"📝 Weekly Meetup - 15 Nov 2026"}`,
	threadID, serverID, captionsChannelID)

// lookUpAs makes the fake Discord describe the thread like this when the bot
// looks it up.
func lookUpAs(t *testing.T, env *testEnv, channelJSON string) {
	t.Helper()
	var channel discord.UnmarshalChannel
	if err := json.Unmarshal([]byte(channelJSON), &channel); err != nil {
		t.Fatal(err)
	}
	env.rest.channels = map[snowflake.ID]discord.Channel{threadID: channel.Channel}
}

func inChannel(who caller, channelJSON string) caller {
	who.inChannel = channelJSON
	return who
}

func TestExportRebuildsTheTranscript(t *testing.T) {
	env := newTestEnv(t)
	lookUpAs(t, env, threadJSON(botID, captionsChannelID))
	env.rest.posted = append(env.rest.posted,
		postedMessage{channelID: threadID, messageID: 9001, message: discord.MessageCreate{Content: "**Sam:** fixed text"}})
	event, sent := env.command(t, inChannel(moderator, sentWithCommand), exportCommand())

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

// Turned away before anything else: the hidden reply says why.
func TestExportRefusedStraightAway(t *testing.T) {
	for _, test := range []struct {
		why  string
		who  caller
		want string
	}{
		{"in a DM", caller{id: callerID, inDM: true}, "I only work inside a server"},
		{"not a note-taker or moderator", inChannel(plainMember, sentWithCommand), "Only note-takers and moderators can rebuild transcripts."},
	} {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			event, sent := env.command(t, test.who, exportCommand())
			if err := env.bot.onTranscribeExport(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, test.want)
		})
	}
}

// Turned away once the thread's been looked up: the "thinking…" reply is
// updated to say why, and nothing is posted.
func TestExportOutsideACaptionsThread(t *testing.T) {
	textChannel := fmt.Sprintf(`{"id":"%d","type":0,"guild_id":"%d","name":"captions"}`, threadID, serverID)
	notThere := "Run /transcribe export inside a captions thread, and I'll rebuild that thread's transcript."
	for _, test := range []struct {
		why       string
		sentWith  string // the channel as the command describes it ("" = none)
		lookedUp  string // the channel as looking it up describes it
		lookupErr error
		want      string
	}{
		{"no channel sent with the command", "", "", nil, notThere},
		{"not a thread", textChannel, textChannel, nil, notThere},
		{"a thread the bot didn't start", sentWithCommand, threadJSON(otherMemberID, captionsChannelID), nil, notThere},
		{"a thread somewhere else", sentWithCommand, threadJSON(botID, meetingChannelID), nil, notThere},
		{"looking it up fails", sentWithCommand, "", errorString("discord unreachable"), "I couldn't look this thread up, try again in a moment."},
	} {
		t.Run(test.why, func(t *testing.T) {
			env := newTestEnv(t)
			if test.lookedUp != "" {
				lookUpAs(t, env, test.lookedUp)
			}
			env.rest.channelErr = test.lookupErr
			event, _ := env.command(t, inChannel(moderator, test.sentWith), exportCommand())

			if err := env.bot.onTranscribeExport(event); err != nil {
				t.Fatal(err)
			}
			if len(env.rest.responses) != 1 || env.rest.responses[0] != test.want {
				t.Errorf("want %q, got %q", test.want, env.rest.responses)
			}
			if len(env.rest.posted) != 0 {
				t.Errorf("nothing should be posted, got %+v", env.rest.posted)
			}
		})
	}
}

func TestExportWhenRebuildingFails(t *testing.T) {
	captureLog(t)
	env := newTestEnv(t)
	lookUpAs(t, env, threadJSON(botID, captionsChannelID))
	env.rest.historyErr = errorString("no access")
	event, _ := env.command(t, inChannel(moderator, sentWithCommand), exportCommand())

	if err := env.bot.onTranscribeExport(event); err != nil {
		t.Fatal(err)
	}
	if len(env.rest.responses) != 1 || env.rest.responses[0] != "I couldn't rebuild the transcript, try again in a moment." {
		t.Errorf("got %q", env.rest.responses)
	}
}

func TestExportWhenThinkingFails(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, inChannel(moderator, sentWithCommand), exportCommand())
	sent.err = errorString("discord unreachable")

	if err := env.bot.onTranscribeExport(event); err == nil {
		t.Fatal("the error should be returned so middleware.Go logs it")
	}
	if len(env.rest.posted) != 0 {
		t.Error("nothing should be posted")
	}
}

// An admin picked a new captions channel: threads in the old one are still
// captions threads, so their transcripts can still be rebuilt.
func TestExportWorksInAnOldCaptionsChannel(t *testing.T) {
	env := newTestEnv(t)
	if err := env.bot.settings.SetCaptionsChannel(serverID, lockedCaptionsChannelID); err != nil {
		t.Fatal(err)
	}
	lookUpAs(t, env, threadJSON(botID, captionsChannelID)) // a thread in the old channel
	event, _ := env.command(t, inChannel(moderator, sentWithCommand), exportCommand())

	if err := env.bot.onTranscribeExport(event); err != nil {
		t.Fatal(err)
	}
	if len(env.rest.responses) != 1 || env.rest.responses[0] != "Posted the updated transcript in this thread." {
		t.Errorf("got %q", env.rest.responses)
	}
}
