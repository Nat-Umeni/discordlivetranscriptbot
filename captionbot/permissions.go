package captionbot

import (
	"fmt"
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
)

// canManageTranscription reports whether the person who ran the command may start
// or stop captions in this server. Moderators (Manage Channels) always can, and so
// can anyone with the note-taker role an admin picked with /captions-setup.
func (b *Bot) canManageTranscription(e *handler.CommandEvent, serverID snowflake.ID) bool {
	return b.memberCanManageTranscription(e.Member(), serverID)
}

// memberCanManageTranscription is canManageTranscription's rule for any
// interaction that carries the caller's member info, such as a popup being
// submitted.
func (b *Bot) memberCanManageTranscription(member *discord.ResolvedMember, serverID snowflake.ID) bool {
	// Discord sends the caller's roles and permissions with every interaction,
	// so this needs no cache lookups. Member is nil only in DMs, which callers
	// rule out first, but check anyway rather than risk a crash.
	if member == nil {
		return false
	}

	if member.Permissions.Has(discord.PermissionManageChannels) {
		return true
	}

	noteTakerRoleID, roleIsSet := b.settings.NoteTakerRole(serverID)
	return roleIsSet && slices.Contains(member.RoleIDs, noteTakerRoleID)
}

func checkBotCanJoin(e *handler.CommandEvent, serverID, channelID snowflake.ID) (bool, error) {
	perms, err := botPermissionsIn(e, serverID, channelID)
	if err != nil {
		return false, err
	}

	return perms.Has(voicePermissions), nil
}

// checkBotCanPostCaptions says whether the bot can post the opening message
// and the captions thread in a channel. The error means it couldn't check.
func checkBotCanPostCaptions(e *handler.CommandEvent, serverID, channelID snowflake.ID) (bool, error) {
	perms, err := botPermissionsIn(e, serverID, channelID)
	if err != nil {
		return false, err
	}

	return perms.Has(captionsPermissions), nil
}

// missingCaptionsPermissionsMessage tells an admin what to fix when the bot
// can't post captions in a channel.
func missingCaptionsPermissionsMessage(channelID snowflake.ID) string {
	return fmt.Sprintf("I can't post captions in %s. Ask an admin to give me View Channel, Send Messages, "+
		"Create Public Threads, Send Messages in Threads, Read Message History and Attach Files there.",
		discord.ChannelMention(channelID))
}

// botPermissionsIn works out the bot's permissions in one channel, from disgo's
// cache. The error means it couldn't check, not that the bot isn't allowed.
func botPermissionsIn(e *handler.CommandEvent, serverID, channelID snowflake.ID) (discord.Permissions, error) {
	// Look up the bot's own member info (which roles it has) in disgo's cache.
	// ok is false if the cache doesn't have it. That doesn't mean the bot isn't in
	// the server; it means we can't check its permissions.
	botMember, memberCacheOk := e.Client().Caches.SelfMember(serverID)
	if !memberCacheOk {
		return 0, fmt.Errorf("could not determine if the bot was in the cache")
	}

	channel, channelCacheOk := e.Client().Caches.Channel(channelID)
	if !channelCacheOk {
		return 0, fmt.Errorf("could not find channel %s in the cache to check permissions", channelID)
	}

	return e.Client().Caches.MemberPermissionsInChannel(channel, botMember), nil
}
