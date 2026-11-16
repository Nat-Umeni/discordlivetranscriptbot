package captionbot

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

var testCall = captionsCall{
	title:             "Weekly Meetup",
	voiceChannelID:    meetingChannelID,
	captionsChannelID: captionsChannelID,
	noCaptionsRoleID:  noCaptionsRoleID,
	startedBy:         callerID,
}

func TestCaptionsThreadName(t *testing.T) {
	when := time.Date(2026, 11, 15, 19, 30, 0, 0, time.Local)

	if got, want := captionsThreadName("Weekly Meetup", when), "📝 Weekly Meetup - 15 Nov 2026"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A title too long for Discord's 100-character limit is cut short with "…",
	// keeping the date on the end.
	long := strings.Repeat("Annual general meeting ", 10)
	name := captionsThreadName(long, when)
	if n := utf8.RuneCountInString(name); n != maxThreadNameLength {
		t.Errorf("long name is %d characters, want exactly %d: %q", n, maxThreadNameLength, name)
	}
	if !strings.HasSuffix(name, "… - 15 Nov 2026") {
		t.Errorf("long name should end with the cut mark and the date, got %q", name)
	}
}

func TestLiveStatus(t *testing.T) {
	if got, want := liveStatus("Weekly Meetup"), "🔴 Live captions: Weekly Meetup"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Discord's limit for a status is 500 characters.
	status := liveStatus(strings.Repeat("x", 600))
	if n := utf8.RuneCountInString(status); n != maxVoiceStatusLength || !strings.HasSuffix(status, "…") {
		t.Errorf("long status should be cut to %d characters ending in …, got %d", maxVoiceStatusLength, n)
	}
}

func TestCallNotice(t *testing.T) {
	// Before the thread exists it points at the captions channel; after, at
	// the thread itself.
	if before := callNotice(testCall, 0); !strings.Contains(before, "Captions are in "+discord.ChannelMention(captionsChannelID)+".") {
		t.Errorf("before the thread exists, the notice should link the captions channel: %q", before)
	}
	notice := callNotice(testCall, threadID)

	for _, want := range []string{
		"This call is being captioned live",
		"Deaf or hard of hearing",
		"Captions are in " + discord.ChannelMention(threadID) + ".",
		"audio is never recorded or stored",
		"Anyone with " + discord.RoleMention(noCaptionsRoleID) + " isn't captioned. Ask an admin how to get the role if you'd like to opt out.",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q should contain %q", notice, want)
		}
	}
}

func TestCaptionsIntro(t *testing.T) {
	intro := captionsIntro(testCall)

	for _, want := range []string{
		"Live captions: Weekly Meetup",
		"captioning " + discord.ChannelMention(meetingChannelID) + " live in this thread",
		"Deaf or hard of hearing",
		"audio is never recorded or stored",
		"Anyone with " + discord.RoleMention(noCaptionsRoleID) + " isn't captioned. Ask an admin how to get the role if you'd like to opt out.",
		"Started by " + discord.UserMention(callerID),
	} {
		if !strings.Contains(intro, want) {
			t.Errorf("intro %q should contain %q", intro, want)
		}
	}
}

// If the opening post fails and the cleanup fails too (deleting a thread needs
// Manage Threads, which the bot may not have), the post's error is still the
// one returned, and each failed cleanup is logged.
func TestAnnounceCaptionsWhenTheCleanupFailsToo(t *testing.T) {
	logs := captureLog(t)
	fake := &fakeRest{
		postErrIn: map[snowflake.ID]error{threadID: errors.New("post refused")},
		deleteErr: errors.New("delete refused"),
	}

	_, _, err := announceCaptions(&bot.Client{Rest: fake}, testCall)

	if err == nil || !strings.Contains(err.Error(), "post refused") {
		t.Fatalf("want the opening post's error back, got %v", err)
	}
	for _, want := range []string{"couldn't delete the empty captions thread", "couldn't delete my message"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, logged:\n%s", want, logs)
		}
	}
}

func TestAnnounceCaptionsLogsAFailedStatus(t *testing.T) {
	logs := captureLog(t)
	fake := &fakeRest{statusErr: errors.New("status refused")}

	thread, _, err := announceCaptions(&bot.Client{Rest: fake}, testCall)

	if err != nil || thread == nil {
		t.Fatalf("a failed status shouldn't stop captions, got (%v, %v)", thread, err)
	}
	if !strings.Contains(logs.String(), "couldn't set the voice channel status") {
		t.Errorf("the failed status should be logged, logged:\n%s", logs)
	}
}

func TestClearVoiceStatus(t *testing.T) {
	fake := &fakeRest{}
	clearVoiceStatus(&bot.Client{Rest: fake}, meetingChannelID)
	if len(fake.statuses) != 1 || fake.statuses[0].channelID != meetingChannelID || fake.statuses[0].message.Content != "" {
		t.Errorf("want the status cleared with an empty status, got %+v", fake.statuses)
	}

	logs := captureLog(t)
	fake.statusErr = errors.New("not in the call any more")
	clearVoiceStatus(&bot.Client{Rest: fake}, meetingChannelID)
	if !strings.Contains(logs.String(), "couldn't clear the voice channel status") {
		t.Errorf("a failed clear should be logged, logged:\n%s", logs)
	}
}
