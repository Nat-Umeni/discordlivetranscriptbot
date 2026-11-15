package captionbot

import (
	"log"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"
)

func (b *Bot) onCaptionsSetup(e *handler.CommandEvent) error {
	// The command is server-only (Contexts), but check anyway: e.GuildID() is a
	// pointer, and dereferencing nil would crash the bot.
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	// Every option is optional; the bools say whether each was given.
	data := e.SlashCommandInteractionData()
	role, roleGiven := data.OptRole("notetaker-role")
	channel, channelGiven := data.OptChannel("captions-channel")
	noCaptionsRole, noCaptionsRoleGiven := data.OptRole("no-captions-role")

	// Check the channel before saving anything, so a channel the bot can't
	// post in is never saved and only discovered at the next start.
	if channelGiven {
		canPost, err := checkBotCanPostCaptions(e, *guildID, channel.ID)
		if err != nil {
			log.Printf("Failed to check permissions in the chosen captions channel: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't check whether I can post in that channel, try again in a moment")
		}
		if !canPost {
			return createHiddenMessageForCommandCaller(e, missingCaptionsPermissionsMessage(channel.ID))
		}
	}

	if roleGiven {
		if err := b.settings.SetNoteTakerRole(*guildID, role.ID); err != nil {
			log.Printf("couldn't save the note-taker role: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}
	if channelGiven {
		if err := b.settings.SetCaptionsChannel(*guildID, channel.ID); err != nil {
			log.Printf("couldn't save the captions channel: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}
	if noCaptionsRoleGiven {
		if err := b.settings.SetNoCaptionsRole(*guildID, noCaptionsRole.ID); err != nil {
			log.Printf("couldn't save the no-captions role: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}

	// Show the settings as they are now, whether or not anything changed.
	message := "Here's how captions are set up here:"
	if roleGiven || channelGiven || noCaptionsRoleGiven {
		message = "Got it. " + message
	}
	return createHiddenMessageForCommandCaller(e, message+"\n"+b.describeCaptionsSettings(*guildID))
}

// describeCaptionsSettings lists a server's captions settings for
// /captions-setup's reply.
func (b *Bot) describeCaptionsSettings(serverID snowflake.ID) string {
	noteTakers := "not set (only moderators can start captions)"
	if roleID, ok := b.settings.NoteTakerRole(serverID); ok {
		noteTakers = discord.RoleMention(roleID) + " (they can start and stop captions)"
	}

	captionsChannel := "not set (I need one before I can start captions)"
	if channelID, ok := b.settings.CaptionsChannel(serverID); ok {
		captionsChannel = discord.ChannelMention(channelID)
	}

	notCaptioned := "not set (I need one before I can start captions, so people can opt out)"
	if roleID, ok := b.settings.NoCaptionsRole(serverID); ok {
		notCaptioned = "anyone with " + discord.RoleMention(roleID)
	}

	return "- Note-takers: " + noteTakers +
		"\n- Captions go in: " + captionsChannel +
		"\n- Not captioned: " + notCaptioned
}
