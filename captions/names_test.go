package captions

import (
	"testing"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func strPtr(s string) *string { return &s }

func TestMemberNames(t *testing.T) {
	const thisServer, otherServer snowflake.ID = 1, 2

	// The real disgo cache, filled the way Discord's member info fills it.
	caches := cache.New(cache.WithCaches(cache.FlagMembers))
	caches.AddMember(discord.Member{GuildID: thisServer, Nick: strPtr("Sammy"),
		User: discord.User{ID: 10, Username: "sam123", GlobalName: strPtr("Sam")}})
	caches.AddMember(discord.Member{GuildID: thisServer,
		User: discord.User{ID: 11, Username: "jo_99", GlobalName: strPtr("Jo")}})
	caches.AddMember(discord.Member{GuildID: thisServer,
		User: discord.User{ID: 12, Username: "priya_"}})
	// Jo has a nickname in another server, which mustn't leak into this one.
	caches.AddMember(discord.Member{GuildID: otherServer, Nick: strPtr("Jojo"),
		User: discord.User{ID: 11, Username: "jo_99", GlobalName: strPtr("Jo")}})

	nameFor := MemberNames(caches, thisServer)

	tests := []struct {
		why    string
		userID snowflake.ID
		want   string
	}{
		{"a server nickname wins", 10, "Sammy"},
		{"no nickname: display name", 11, "Jo"},
		{"no nickname or display name: username", 12, "priya_"},
		{"Discord never said whose stream it was", 0, "Unknown speaker"},
		{"not cached: the ID, to help work out why", 999, "user 999"},
	}
	for _, test := range tests {
		if got := nameFor(test.userID); got != test.want {
			t.Errorf("%s: got %q, want %q", test.why, got, test.want)
		}
	}
}

func TestSenderName(t *testing.T) {
	message := discord.Message{Author: discord.User{ID: 10, Username: "jo_99", GlobalName: strPtr("Jo")}}
	if got := SenderName(message); got != "Jo" {
		t.Errorf("no nickname: want the display name, got %q", got)
	}

	message.Member = &discord.Member{Nick: strPtr("Jojo")}
	if got := SenderName(message); got != "Jojo" {
		t.Errorf("a nickname wins, got %q", got)
	}

	message.Member = &discord.Member{} // in the server, no nickname
	message.Author.GlobalName = nil
	if got := SenderName(message); got != "jo_99" {
		t.Errorf("no nickname or display name: want the username, got %q", got)
	}
}

func TestAttachmentNote(t *testing.T) {
	message := discord.Message{ID: 700, ChannelID: 600, GuildID: snowflakePtr(1)}
	link := "https://discord.com/channels/1/600/700"

	if got := AttachmentNote(message); got != "" {
		t.Errorf("no files: want no note, got %q", got)
	}
	message.Attachments = make([]discord.Attachment, 1)
	if got, want := AttachmentNote(message), "*[sent an attachment: "+link+"]*"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	message.Attachments = make([]discord.Attachment, 3)
	if got, want := AttachmentNote(message), "*[sent 3 attachments: "+link+"]*"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func snowflakePtr(id snowflake.ID) *snowflake.ID { return &id }
