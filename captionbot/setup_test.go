package captionbot

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func TestCaptionsSetupSavesTheRole(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, 0, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	for _, want := range []string{
		"Note-takers: " + discord.RoleMention(noteTakerRoleID),
		"Captions go in: " + discord.ChannelMention(captionsChannelID),        // set by newTestEnv, kept
		"Not captioned: anyone with " + discord.RoleMention(noCaptionsRoleID), // same
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}

	got, ok := env.bot.settings.NoteTakerRole(serverID)
	if !ok || got != noteTakerRoleID {
		t.Fatalf("role not saved: got (%v, %v)", got, ok)
	}
	reloaded := env.reloadSettings(t)
	if got, ok := reloaded.NoteTakerRole(serverID); !ok || got != noteTakerRoleID {
		t.Fatalf("role not written to settings.json: got (%v, %v)", got, ok)
	}
	if got, _ := reloaded.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("setting the role must not wipe the captions channel, got %v", got)
	}
}

func TestCaptionsSetupSavesTheChannel(t *testing.T) {
	env := newTestEnv(t)
	env.clearSettings(t) // a server nobody has set up yet
	event, sent := env.command(t, moderator, setupCommand(0, captionsChannelID, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	for _, want := range []string{
		"Captions go in: " + discord.ChannelMention(captionsChannelID),
		"Note-takers: not set (only moderators can start captions)",
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}

	reloaded := env.reloadSettings(t)
	if got, ok := reloaded.CaptionsChannel(serverID); !ok || got != captionsChannelID {
		t.Fatalf("channel not written to settings.json: got (%v, %v)", got, ok)
	}
	if _, ok := reloaded.NoteTakerRole(serverID); ok {
		t.Error("saving only a channel shouldn't make a note-taker role appear")
	}
}

func TestCaptionsSetupSavesBoth(t *testing.T) {
	env := newTestEnv(t)
	env.clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, captionsChannelID, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "Got it.")
	if got, _ := env.bot.settings.NoteTakerRole(serverID); got != noteTakerRoleID {
		t.Errorf("role not saved, got %v", got)
	}
	if got, _ := env.bot.settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("channel not saved, got %v", got)
	}
}

func TestCaptionsSetupWithNothingShowsTheSettings(t *testing.T) {
	env := newTestEnv(t)
	env.clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(0, 0, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Here's how captions are set up here:")
	if strings.HasPrefix(reply.Content, "Got it") {
		t.Error("nothing was changed, so it shouldn't say it saved anything")
	}
	for _, want := range []string{
		"Captions go in: not set (I need one before I can start captions)",
		"Not captioned: not set (I need one before I can start captions, so people can opt out)",
	} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply %q should contain %q", reply.Content, want)
		}
	}
}

func TestCaptionsSetupSavesTheNoCaptionsRole(t *testing.T) {
	env := newTestEnv(t)
	env.clearSettings(t)
	event, sent := env.command(t, moderator, setupCommand(0, 0, noCaptionsRoleID))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	reply := onlyReply(t, sent, "Got it.")
	if want := "Not captioned: anyone with " + discord.RoleMention(noCaptionsRoleID); !strings.Contains(reply.Content, want) {
		t.Errorf("reply %q should contain %q", reply.Content, want)
	}

	reloaded := env.reloadSettings(t)
	if got, ok := reloaded.NoCaptionsRole(serverID); !ok || got != noCaptionsRoleID {
		t.Fatalf("no-captions role not written to settings.json: got (%v, %v)", got, ok)
	}
	if _, ok := reloaded.NoteTakerRole(serverID); ok {
		t.Error("the no-captions role must not be saved as the note-taker role")
	}
}

func TestCaptionsSetupRefusesAChannelTheBotCantPostIn(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(noteTakerRoleID, lockedCaptionsChannelID, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I can't post captions in "+discord.ChannelMention(lockedCaptionsChannelID)+
		". Ask an admin to give me View Channel, Send Messages, Create Public Threads and Send Messages in Threads there.")
	if got, _ := env.bot.settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("the refused channel must not be saved, got %v", got)
	}
	if _, ok := env.bot.settings.NoteTakerRole(serverID); ok {
		t.Error("nothing should be saved when the command is refused, including the role")
	}
}

func TestCaptionsSetupWhenTheChannelIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, setupCommand(0, 6999, 0)) // a channel the cache doesn't know

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I couldn't check whether I can post in that channel")
	if got, _ := env.bot.settings.CaptionsChannel(serverID); got != captionsChannelID {
		t.Errorf("an unchecked channel must not be saved, got %v", got)
	}
}

func TestCaptionsSetupWhenSavingFails(t *testing.T) {
	for _, test := range []struct {
		name                        string
		roleID, channel, noCaptions snowflake.ID
	}{
		{"note-taker role", noteTakerRoleID, 0, 0},
		{"channel", 0, captionsChannelID, 0},
		{"no-captions role", 0, 0, noCaptionsRoleID},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.useSettingsFile(t, filepath.Join(t.TempDir(), "missing-folder", "settings.json"))
			event, sent := env.command(t, moderator, setupCommand(test.roleID, test.channel, test.noCaptions))

			if err := env.bot.onCaptionsSetup(event); err != nil {
				t.Fatal(err)
			}
			onlyReply(t, sent, "I couldn't save that")
		})
	}
}

func TestCaptionsSetupInDM(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, caller{id: callerID, inDM: true}, setupCommand(noteTakerRoleID, 0, 0))

	if err := env.bot.onCaptionsSetup(event); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "I only work inside a server")
}
