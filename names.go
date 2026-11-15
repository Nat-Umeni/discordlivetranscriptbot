package main

import (
	"fmt"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/snowflake/v2"
)

// nameFunc turns a user ID into the name to show on their captions.
type nameFunc func(userID snowflake.ID) string

// memberNames looks up speakers' names in one server, from disgo's member
// cache. Everyone in voice is already cached (Discord sends their member info
// when the bot starts and whenever someone joins voice), so this never waits
// on the network.
//
// The name is looked up for every caption rather than once, so a nickname
// changed mid-call shows straight away.
func memberNames(caches cache.Caches, serverID snowflake.ID) nameFunc {
	return func(userID snowflake.ID) string {
		// Discord never said whose stream this was.
		if userID == 0 {
			return "Unknown speaker"
		}

		member, found := caches.Member(serverID, userID)
		if !found {
			// Shouldn't happen for someone in voice; the ID helps work out why.
			return fmt.Sprintf("user %s", userID)
		}

		// Their nickname in this server, else their display name, else their
		// username: the name people see next to them in the call.
		return member.EffectiveName()
	}
}
