package main

import (
	"fmt"
	"log"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// The permissions the bot needs in the captions channel: see it, post the
// opening message, start a thread from it, and post captions in the thread.
const captionsPermissions = discord.PermissionViewChannel |
	discord.PermissionSendMessages |
	discord.PermissionCreatePublicThreads |
	discord.PermissionSendMessagesInThreads

// The permissions the bot needs in the call: see and join it, post the
// captioning notice in its chat, and set its status.
const voicePermissions = discord.PermissionViewChannel |
	discord.PermissionConnect |
	discord.PermissionSendMessages |
	discord.PermissionSetVoiceChannelStatus

// Discord rejects thread names and voice channel statuses longer than these.
const (
	maxThreadNameLength  = 100
	maxVoiceStatusLength = 500
)

// noPings stops a message notifying anyone. Without it, the "Started by @Sam"
// line would ping Sam, the no-captions role mention would ping everyone with
// it, and a title containing @everyone would ping the whole server.
var noPings = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}}

// captionsCall is everything the announcements need to know about a call.
type captionsCall struct {
	title             string
	voiceChannelID    snowflake.ID
	captionsChannelID snowflake.ID
	noCaptionsRoleID  snowflake.ID
	startedBy         snowflake.ID
}

// announceCaptions tells everyone a call is being captioned, before anything
// is listened to:
//   - a notice in the call's own chat, for the people in it, linking to the
//     captions thread
//   - the captions thread in the captions channel, with an opening post, and
//     whoever started the captions added to it
//   - the voice channel's status, seen by anyone in or joining the call
//
// If the notice or the thread fails, nothing is left behind and the error is
// returned: captions must not run without people being told. The rest only
// make captions easier to find, so if they fail it's logged and captions go
// ahead.
func announceCaptions(client *bot.Client, call captionsCall) (*discord.GuildThread, error) {
	// The notice goes first so people are told before anything else happens.
	// The thread doesn't exist yet, so it points at the captions channel for
	// now.
	notice, err := client.Rest.CreateMessage(call.voiceChannelID, discord.MessageCreate{
		Content:         callNotice(call, 0),
		AllowedMentions: noPings,
	})
	if err != nil {
		return nil, fmt.Errorf("posting the notice in the call's chat: %w", err)
	}

	thread, err := startCaptionsThread(client, call)
	if err != nil {
		deleteOwnMessage(client, call.voiceChannelID, notice.ID)
		return nil, err
	}

	// Now point the notice straight at the thread. The captions channel holds
	// a thread for every call, so a link to the channel leaves people
	// guessing which one is live.
	linked := callNotice(call, thread.ID())
	_, err = client.Rest.UpdateMessage(call.voiceChannelID, notice.ID, discord.MessageUpdate{Content: &linked, AllowedMentions: noPings})
	if err != nil {
		log.Printf("couldn't link the call notice to the captions thread: %v", err)
	}

	// Add whoever started the captions to the thread, so it shows in their
	// sidebar and lights up as captions arrive. Only them: Discord posts an
	// "added … to the thread" line for each person added, which would clutter
	// the captions.
	if err := client.Rest.AddThreadMember(thread.ID(), call.startedBy); err != nil {
		log.Printf("couldn't add %s to the captions thread: %v", call.startedBy, err)
	}

	err = client.Rest.SetVoiceChannelStatus(call.voiceChannelID, discord.VoiceChannelStatusUpdate{Status: liveStatus(call.title)})
	if err != nil {
		log.Printf("couldn't set the voice channel status: %v", err)
	}

	return thread, nil
}

// clearVoiceStatus takes the "live captions" status off a voice channel when
// captions stop. Best effort: it's logged if it fails.
func clearVoiceStatus(client *bot.Client, voiceChannelID snowflake.ID) {
	// An empty status clears it.
	err := client.Rest.SetVoiceChannelStatus(voiceChannelID, discord.VoiceChannelStatusUpdate{Status: ""})
	if err != nil {
		log.Printf("couldn't clear the voice channel status: %v", err)
	}
}

// startCaptionsThread creates the captions thread in the captions channel and
// posts the opening message inside it. The thread stands on its own rather
// than hanging off a message, so the opening message only appears once:
// inside the thread. (Discord still shows a small "started a thread" line in
// the channel.)
func startCaptionsThread(client *bot.Client, call captionsCall) (*discord.GuildThread, error) {
	thread, err := client.Rest.CreateThread(call.captionsChannelID, discord.GuildPublicThreadCreate{
		Name: captionsThreadName(call.title, time.Now()),
		// Archive it after a day with no new messages. Archived threads can
		// still be read; they just drop out of the channel's thread list.
		AutoArchiveDuration: discord.AutoArchiveDuration24h,
	})
	if err != nil {
		return nil, fmt.Errorf("creating the captions thread: %w", err)
	}

	_, err = client.Rest.CreateMessage(thread.ID(), discord.MessageCreate{
		Content:         captionsIntro(call),
		AllowedMentions: noPings,
	})
	if err != nil {
		// Don't leave an empty thread for captions that aren't happening.
		// Deleting a thread needs Manage Threads, which the bot may not have,
		// so this can fail; it's logged if so.
		if deleteErr := client.Rest.DeleteChannel(thread.ID()); deleteErr != nil {
			log.Printf("couldn't delete the empty captions thread %s: %v", thread.ID(), deleteErr)
		}
		return nil, fmt.Errorf("posting the opening message: %w", err)
	}

	return thread, nil
}

// deleteOwnMessage removes a message the bot posted (it can always delete its
// own). Best effort: it's logged if it fails.
func deleteOwnMessage(client *bot.Client, channelID, messageID snowflake.ID) {
	if err := client.Rest.DeleteMessage(channelID, messageID); err != nil {
		log.Printf("couldn't delete my message %s after captions failed to start: %v", messageID, err)
	}
}

// callNotice goes in the call's own chat, so the people in it know they're
// being captioned, where the captions are, and how to opt out. It links to the
// captions thread, or to the captions channel while threadID is 0 (the thread
// isn't made yet).
func callNotice(call captionsCall, threadID snowflake.ID) string {
	captionsAt := call.captionsChannelID
	if threadID != 0 {
		captionsAt = threadID
	}
	return fmt.Sprintf("🔴 **This call is being captioned live** so everyone can follow along, "+
		"including members who are Deaf or hard of hearing. Captions are in %s.\n"+
		"I only keep the text: audio is never recorded or stored.\n"+
		"%s",
		discord.ChannelMention(captionsAt), optOutLine(call.noCaptionsRoleID))
}

// captionsIntro is the opening post of the captions thread.
func captionsIntro(call captionsCall) string {
	return fmt.Sprintf("📝 **Live captions: %s**\n"+
		"I'm captioning %s live in this thread, so everyone can follow along, "+
		"including members who are Deaf or hard of hearing.\n"+
		"I only post text: the call's audio is never recorded or stored.\n"+
		"%s\n"+
		"Started by %s",
		call.title, discord.ChannelMention(call.voiceChannelID), optOutLine(call.noCaptionsRoleID),
		discord.UserMention(call.startedBy))
}

// optOutLine tells people how to opt out. The call notice and the thread's
// opening post both end with it, so they always say the same thing.
func optOutLine(noCaptionsRoleID snowflake.ID) string {
	return fmt.Sprintf("Anyone with %s isn't captioned. Ask an admin how to get the role if you'd like to opt out.",
		discord.RoleMention(noCaptionsRoleID))
}

// liveStatus is the voice channel status while captions run, e.g.
// "🔴 Live captions: Weekly Meetup".
func liveStatus(title string) string {
	return cutToLength("🔴 Live captions: "+title, maxVoiceStatusLength)
}

// captionsThreadName gives e.g. "📝 Weekly Meetup - 15 Nov 2026". Long titles
// are cut short with "…" so the name fits Discord's limit, keeping the date on
// the end.
func captionsThreadName(title string, now time.Time) string {
	prefix := "📝 "
	suffix := " - " + now.Format("2 Jan 2006")

	// Count in runes (characters), not bytes: the emoji is several bytes but
	// counts as one character to Discord.
	room := maxThreadNameLength - len([]rune(prefix)) - len([]rune(suffix))
	return prefix + cutToLength(title, room) + suffix
}

// cutToLength shortens text to at most limit characters, ending with "…" if
// it had to cut.
func cutToLength(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}
