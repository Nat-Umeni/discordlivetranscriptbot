package captionbot

import (
	"testing"
)

func TestCanManageTranscription(t *testing.T) {
	tests := []struct {
		name    string
		who     caller
		roleSet bool
		want    bool
	}{
		{"moderator", moderator, false, true},
		{"note-taker", noteTaker, true, true},
		{"has the role's ID but no role is set yet", noteTaker, false, false},
		{"plain member, no role set", plainMember, false, false},
		{"plain member, role set", plainMember, true, false},
		{"DM (no member)", caller{id: callerID, inDM: true}, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			if test.roleSet {
				_ = env.bot.settings.SetNoteTakerRole(serverID, noteTakerRoleID)
			}
			event, _ := env.command(t, test.who, stopCommand())

			if got := env.bot.canManageTranscription(event, serverID); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}
