package captions

import (
	"fmt"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// NameFunc turns a user ID into the name to show on their captions.
type NameFunc func(userID snowflake.ID) string

// MemberNames looks up speakers' names in one server, from disgo's member
// cache. Everyone in voice is already cached (Discord sends their member info
// when the bot starts and whenever someone joins voice), so this never waits
// on the network.
//
// The name is looked up for every caption rather than once, so a nickname
// changed mid-call shows straight away.
func MemberNames(caches cache.Caches, serverID snowflake.ID) NameFunc {
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

// SenderName is the name people see next to a message's sender in its server:
// their nickname there, else their display name, else their username. It
// comes from the message itself, so it works for anyone, in the call or not.
func SenderName(message discord.Message) string {
	if message.Member != nil && message.Member.Nick != nil {
		return *message.Member.Nick
	}
	return message.Author.EffectiveName()
}

// AttachmentNote notes files sent with a message, linking to the original so
// readers can open them. Empty if there were none.
func AttachmentNote(message discord.Message) string {
	switch count := len(message.Attachments); count {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("*[sent an attachment: %s]*", message.JumpURL())
	default:
		return fmt.Sprintf("*[sent %d attachments: %s]*", count, message.JumpURL())
	}
}
