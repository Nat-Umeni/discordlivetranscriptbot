package captionbot

import (
	"path/filepath"
	"testing"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/settings"
)

func TestOptedOutOfCaptions(t *testing.T) {
	const server, noCaptions, otherRole snowflake.ID = 1, 50, 51
	const withRole, withoutRole, notCached snowflake.ID = 10, 11, 12

	store, err := settings.Load(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := New(store, nil)

	caches := cache.New(cache.WithCaches(cache.FlagMembers))
	caches.AddMember(discord.Member{GuildID: server, User: discord.User{ID: withRole}, RoleIDs: []snowflake.ID{otherRole, noCaptions}})
	caches.AddMember(discord.Member{GuildID: server, User: discord.User{ID: withoutRole}, RoleIDs: []snowflake.ID{otherRole}})

	optedOut := b.optedOutOfCaptions(caches, server)

	if optedOut(withRole) || optedOut(withoutRole) {
		t.Error("with no no-captions role picked, nobody has opted out")
	}

	if err := store.SetNoCaptionsRole(server, noCaptions); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		why    string
		userID snowflake.ID
		want   bool
	}{
		{"has the no-captions role", withRole, true},
		{"doesn't have it", withoutRole, false},
		{"not cached, so their roles can't be checked: don't risk it", notCached, true},
	}
	for _, test := range tests {
		if got := optedOut(test.userID); got != test.want {
			t.Errorf("%s: got %v, want %v", test.why, got, test.want)
		}
	}

	// An admin switching the role mid-call takes effect on the next check.
	if err := store.SetNoCaptionsRole(server, otherRole); err != nil {
		t.Fatal(err)
	}
	if !optedOut(withoutRole) {
		t.Error("a changed no-captions role should apply straight away")
	}
}
