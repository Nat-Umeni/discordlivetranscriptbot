package captionbot

import (
	"slices"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/speech"
)

// optedOutOfCaptions checks speakers against the server's no-captions role.
// Everyone is captioned unless they have that role.
//
// It's checked for every packet, so it takes effect straight away when someone
// is given the role mid-call (Discord tells the bot about role changes through
// the Server Members intent), and the role is read from settings each time in
// case an admin changes it mid-call.
func (b *Bot) optedOutOfCaptions(caches cache.Caches, serverID snowflake.ID) speech.OptOutFunc {
	return func(userID snowflake.ID) bool {
		roleID, roleSet := b.settings.NoCaptionsRole(serverID)
		if !roleSet {
			// Captions can't start without the role being set, but if an
			// admin's settings file were edited by hand mid-call, nobody has
			// opted out.
			return false
		}

		member, found := caches.Member(serverID, userID)
		if !found {
			// Everyone in voice is cached, so this shouldn't happen. If it
			// does, their roles can't be checked: don't caption them rather
			// than risk captioning someone who said no.
			return true
		}

		return slices.Contains(member.RoleIDs, roleID)
	}
}
