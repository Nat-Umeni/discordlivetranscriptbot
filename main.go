package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/handler/middleware"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	_ "github.com/joho/godotenv/autoload"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

var commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "transcribe",
		Description: "Used to start bot transcribing sessions for meetings etc",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "start",
				Description: "Start transcribing a meeting live into a channel",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionString{
						Name:        "title",
						Description: "Used for the thread and channel status when the meeting begins",
						Required:    true,
					},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "stop",
				Description: "Tells the currently active transcriber bot to wrap up notes and leave the vc",
			},
		},
	},
	// Admin-only setup. It's its own top-level command because Discord can only
	// hide whole commands: hiding /transcribe from non-admins would also hide
	// /transcribe optout, which everyone needs.
	discord.SlashCommandCreate{
		Name:        "captions-setup",
		Description: "Choose the note-taker role, captions channel and no-captions role for this server",
		// Only members with Manage Server see it by default. Admins can still
		// change who sees it in Server Settings → Integrations.
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageGuild),
		// Servers only: it makes no sense in a DM.
		Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild},
		Options: []discord.ApplicationCommandOption{
			// Both optional: set either or both, or neither to see the current
			// settings.
			discord.ApplicationCommandOptionRole{
				Name:        "notetaker-role",
				Description: "Members with this role can start and stop captions",
			},
			discord.ApplicationCommandOptionChannel{
				Name:        "captions-channel",
				Description: "Each call's captions thread is created in this channel",
				// Text channels only: threads can't be made in voice channels.
				ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText},
			},
			discord.ApplicationCommandOptionRole{
				Name:        "no-captions-role",
				Description: "Members with this role are never captioned",
			},
		},
	},
}

var recogniser *sherpa.OnlineRecognizer

func main() {
	token := os.Getenv("BOT_TOKEN")

	// Load each server's saved settings before anything can use them. = (not :=)
	// so this sets the package-level settings variable instead of making a new
	// local one, which would leave the package-level one nil.
	var err error
	settings, err = loadSettings("settings.json")
	if err != nil {
		log.Fatalf("couldn't load settings: %v", err)
	}

	r := handler.New()
	r.Use(middleware.Go)
	r.Route("/transcribe", func(r handler.Router) {
		r.Command("/start", onTranscribeStart)
		r.Command("/stop", onTranscribeStop)
	})
	r.Command("/captions-setup", onCaptionsSetup)

	recogniser, err = newRecogniser(defaultModelDir)
	if err != nil {
		log.Fatalf("failed to setup the new voice recogniser %v", err)
	}

	// The model is deliberately never freed. On shutdown disgo closes each
	// transcriber from its own goroutine, *after* client.Close returns, and a
	// transcriber finishing someone's last sentence would crash if the model
	// had been freed underneath it. The OS frees it when the bot exits.

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildVoiceStates,
				// Privileged (ticked in the Developer Portal): tells the bot when
				// someone's roles change, so the no-captions role works mid-call.
				gateway.IntentGuildMembers,
				gateway.IntentGuildMessages,
				gateway.IntentMessageContent,
			),
		),
		bot.WithVoiceManagerConfigOpts(voice.WithDaveSessionCreateFunc(golibdave.NewSession)),
		// Turn on disgo's cache for these kinds of data. Discord sends them when the bot
		// starts (and updates as they change); without a flag disgo throws that data
		// away, and lookups like Caches.VoiceState or Caches.SelfMember find nothing.
		bot.WithCacheConfigOpts(cache.WithCaches(
			cache.FlagVoiceStates,
			cache.FlagRoles,
			cache.FlagMembers,
			cache.FlagChannels,
			cache.FlagGuilds,
		)),
		bot.WithEventListeners(r),
		// stop the session if someone else disconnects or moves the bot
		bot.WithEventListenerFunc(onBotLeftVoice),
		bot.WithEventListenerFunc(onBotMovedVoice),
	)

	if err != nil {
		log.Fatal(err)
	}

	err = handler.SyncCommands(client, commands, nil)
	if err != nil {
		log.Fatal(err)
	}

	err = client.OpenGateway(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	defer client.Close(context.Background())

	fmt.Println("Bot is online")

	// Wait for Ctrl+C, a stop from the system (SIGTERM), or the terminal
	// window being closed (SIGHUP).
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	<-sc

	// Then the deferred client.Close leaves every call.
	clearStatusesOnShutdown(client)
}

// clearStatusesOnShutdown takes the "live captions" status off every call
// being captioned, while the bot is still in them (setting a call's status
// needs the bot connected to it). Without this, shutting the bot down leaves
// "Live captions" showing on calls nobody is captioning.
func clearStatusesOnShutdown(client *bot.Client) {
	for _, running := range activeSessions.all() {
		clearVoiceStatus(client, running.ChannelID)
	}
}

func onTranscribeStart(e *handler.CommandEvent) error {
	// check where the command was called
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	// only note-takers and moderators can start captions
	if !canManageTranscription(e, *guildID) {
		message := "Only note-takers and moderators can start captions here."
		if _, roleIsSet := settings.NoteTakerRole(*guildID); !roleIsSet {
			message += " An admin can choose the note-taker role with /captions-setup."
		}
		return createHiddenMessageForCommandCaller(e, message)
	}

	// Captions need somewhere to go, and people need a way to opt out, before
	// it's worth joining.
	captionsChannelID, captionsChannelSet := settings.CaptionsChannel(*guildID)
	noCaptionsRoleID, noCaptionsRoleSet := settings.NoCaptionsRole(*guildID)
	var missing []string
	if !captionsChannelSet {
		missing = append(missing, "a captions channel (captions-channel)")
	}
	if !noCaptionsRoleSet {
		missing = append(missing, "a role for people who don't want to be captioned (no-captions-role)")
	}
	if len(missing) > 0 {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("Before I can caption here, an admin needs to pick %s with /captions-setup.",
			strings.Join(missing, " and ")))
	}

	// find the chat
	voiceState, ok := e.Client().Caches.VoiceState(*guildID, e.User().ID)
	if !ok {
		return createHiddenMessageForCommandCaller(e, "I can't see you in a chat, join one then run the command again so I know where to go")
	}

	clickableChatName := discord.ChannelMention(*voiceState.ChannelID)
	title := e.SlashCommandInteractionData().String("title")

	// Claim this server for a new session. A bot can only be in one voice channel
	// per server, so if a session is already running, joining would move the bot
	// out of that call and end it. start checks and records in one locked step,
	// so two people running this at the same moment can't both get through.
	existingSession, claimed := activeSessions.start(*guildID, session{
		StartedBy: e.User().ID,
		ChannelID: *voiceState.ChannelID,
		Title:     title,
	})

	if !claimed {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm already transcribing '%s' in %s, use /transcribe stop first",
			existingSession.Title, discord.ChannelMention(existingSession.ChannelID)))
	}

	// From here on, any return before the bot has joined must give the claim back,
	// or this server would look busy until the bot restarts. A deferred function
	// runs when onTranscribeStart returns, whichever return that is, so it covers
	// every early return below in one place.
	joined := false
	defer func() {
		if !joined {
			activeSessions.remove(*guildID)
		}
	}()

	canJoin, err := checkBotCanJoin(e, *guildID, *voiceState.ChannelID)
	if err != nil {
		log.Printf("Failed to check permissions to join that chat: %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't check whether I'm allowed to join that chat, try again in a moment")
	}

	if !canJoin {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I don't have permission to join %s, ask an admin to give me View Channel, Connect, Send Messages and Set Voice Channel Status there", clickableChatName))
	}

	// checked again here, not just in /captions-setup: the channel's
	// permissions may have changed since
	canPost, err := checkBotCanPostCaptions(e, *guildID, captionsChannelID)
	if err != nil {
		log.Printf("Failed to check permissions in the captions channel: %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't check whether I can post in the captions channel. If it's been deleted, pick another with /captions-setup captions-channel.")
	}

	if !canPost {
		return createHiddenMessageForCommandCaller(e, missingCaptionsPermissionsMessage(captionsChannelID))
	}

	err = createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm joining %s and setting the chat status to '%s'", clickableChatName, title))
	if err != nil {
		return err
	}

	// set up vc
	voiceConnection := e.Client().VoiceManager.CreateConn(*guildID)

	// set a timeout for how long to wait for a response
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// join the chat
	err = voiceConnection.Open(ctx, *voiceState.ChannelID, true, false)
	if err != nil {
		// error whilst trying to join
		voiceConnection.Close(context.Background())
		log.Printf("couldn't join voice: %v", err)

		_, err = e.CreateFollowupMessage(discord.MessageCreate{
			Flags:   discord.MessageFlagEphemeral,
			Content: fmt.Sprintf("I couldn't join %s, try again in a moment", clickableChatName),
		})
		return err
	}

	// Joined, so now tell everyone, before listening to anything: a notice in
	// the call's chat, the captions thread and the call's status. Doing this
	// after joining means a failed join never leaves announcements behind.
	thread, err := announceCaptions(e.Client(), captionsCall{
		title:             title,
		voiceChannelID:    *voiceState.ChannelID,
		captionsChannelID: captionsChannelID,
		noCaptionsRoleID:  noCaptionsRoleID,
		startedBy:         e.User().ID,
	})
	if err != nil {
		// people can't be told, so captions mustn't run: leave again
		voiceConnection.Close(context.Background())
		log.Printf("couldn't announce the captions: %v", err)

		_, err = e.CreateFollowupMessage(discord.MessageCreate{
			Flags: discord.MessageFlagEphemeral,
			Content: fmt.Sprintf("I couldn't post the captions notice in %s's chat and the thread in %s, so I've left again. Try again in a moment.",
				clickableChatName, discord.ChannelMention(captionsChannelID)),
		})
		return err
	}

	// Tell the caller where captions are. If this fails, captions are still
	// running and the thread is in the channel, so log it and carry on.
	_, err = e.CreateFollowupMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: fmt.Sprintf("Captions are live in %s", thread.Mention()),
	})
	if err != nil {
		log.Printf("couldn't send the captions thread link: %v", err)
	}

	// start listening: live captions for each speaker, printed to the terminal
	// under their name in this server, skipping anyone with the no-captions role
	names := memberNames(e.Client().Caches, *guildID)
	optedOut := optedOutOfCaptions(e.Client().Caches, *guildID)
	voiceConnection.SetOpusFrameReceiver(newTranscriber(recogniser, captionPrinter(names), optedOut))

	// joined, so keep the claim: the session now runs until /transcribe stop
	joined = true
	return nil
}

func onTranscribeStop(e *handler.CommandEvent) error {
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	current, running := activeSessions.get(*guildID)
	if !running {
		return createHiddenMessageForCommandCaller(e, "I'm not transcribing in this server right now.")
	}

	// The person who started it can always stop it, so a session can't get stuck
	// waiting for a moderator. Anyone else needs to be a note-taker or moderator,
	// so a random member can't cut the captions off mid-meeting.
	if e.User().ID != current.StartedBy && !canManageTranscription(e, *guildID) {
		return createHiddenMessageForCommandCaller(e, "Only the person who started these captions, note-takers or moderators can stop them.")
	}

	// Reply first: leaving can take a moment, and Discord gives 3 seconds to answer.
	err := createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm leaving %s and stopping the captions for '%s'",
		discord.ChannelMention(current.ChannelID), current.Title))
	if err != nil {
		return err
	}

	// Take the "live captions" status off the call while the bot is still in
	// it (setting a call's status needs the bot to be connected to it).
	clearVoiceStatus(e.Client(), current.ChannelID)

	// Leave the voice channel. GetConn is nil if the bot was already removed from
	// voice (e.g. someone disconnected it), in which case there's nothing to close.
	voiceConnection := e.Client().VoiceManager.GetConn(*guildID)
	if voiceConnection != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		voiceConnection.Close(ctx)
	}

	// Forget the session last, so a /transcribe start can't slip in while the bot
	// is still leaving.
	activeSessions.remove(*guildID)
	return nil
}

// onBotLeftVoice runs whenever anyone leaves a voice channel. When it's the bot and a
// session is still running, someone disconnected it by hand (or Discord dropped it).
func onBotLeftVoice(e *events.GuildVoiceLeave) {
	// GuildVoiceLeave fires for every member; only the bot leaving matters here.
	if e.VoiceState.UserID != e.Client().ID() {
		return
	}
	endSessionAfterOutsideVoiceChange(e.Client(), e.VoiceState.GuildID, "disconnected from voice")
}

// onBotMovedVoice runs whenever anyone is moved to a different voice channel. Our own
// code never moves the bot (/transcribe start refuses while a session is running, and
// joining with a fresh connection counts as a join, not a move), so if the bot moves
// while a session is running, someone else dragged it.
func onBotMovedVoice(e *events.GuildVoiceMove) {
	// GuildVoiceMove fires for every member; only the bot moving matters here.
	if e.VoiceState.UserID != e.Client().ID() {
		return
	}
	endSessionAfterOutsideVoiceChange(e.Client(), e.VoiceState.GuildID, "moved to another channel")
}

// endSessionAfterOutsideVoiceChange stops a server's session when the bot was
// disconnected or moved by someone else rather than by /transcribe stop or start.
// Without this the server would look busy, and /transcribe start would keep saying
// "already transcribing", until someone ran /transcribe stop. After a move, the bot
// would also carry on in a channel nobody asked it to caption.
func endSessionAfterOutsideVoiceChange(client *bot.Client, guildID snowflake.ID, whatHappened string) {
	// No session means the change was ours: /transcribe stop or a failed join
	// already cleaned up, so there's nothing to do.
	current, running := activeSessions.get(guildID)
	if !running {
		return
	}

	log.Printf("I was %s in server %s by someone else, ending that session", whatHappened, guildID)

	// Try to take the "live captions" status off the call. The bot may already
	// be out of it, in which case Discord can refuse; that's only logged.
	clearVoiceStatus(client, current.ChannelID)

	// Close leaves voice (if a move left the bot connected somewhere) and removes
	// disgo's connection. disgo keeps a dropped connection registered, and
	// CreateConn would hand that dead one to the next /transcribe start.
	voiceConnection := client.VoiceManager.GetConn(guildID)
	if voiceConnection != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		voiceConnection.Close(ctx)
	}

	activeSessions.remove(guildID)
}

func onCaptionsSetup(e *handler.CommandEvent) error {
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
		if err := settings.SetNoteTakerRole(*guildID, role.ID); err != nil {
			log.Printf("couldn't save the note-taker role: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}
	if channelGiven {
		if err := settings.SetCaptionsChannel(*guildID, channel.ID); err != nil {
			log.Printf("couldn't save the captions channel: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}
	if noCaptionsRoleGiven {
		if err := settings.SetNoCaptionsRole(*guildID, noCaptionsRole.ID); err != nil {
			log.Printf("couldn't save the no-captions role: %v", err)
			return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
		}
	}

	// Show the settings as they are now, whether or not anything changed.
	message := "Here's how captions are set up here:"
	if roleGiven || channelGiven || noCaptionsRoleGiven {
		message = "Got it. " + message
	}
	return createHiddenMessageForCommandCaller(e, message+"\n"+describeCaptionsSettings(*guildID))
}

// describeCaptionsSettings lists a server's captions settings for
// /captions-setup's reply.
func describeCaptionsSettings(serverID snowflake.ID) string {
	noteTakers := "not set (only moderators can start captions)"
	if roleID, ok := settings.NoteTakerRole(serverID); ok {
		noteTakers = discord.RoleMention(roleID) + " (they can start and stop captions)"
	}

	captionsChannel := "not set (I need one before I can start captions)"
	if channelID, ok := settings.CaptionsChannel(serverID); ok {
		captionsChannel = discord.ChannelMention(channelID)
	}

	notCaptioned := "not set (I need one before I can start captions, so people can opt out)"
	if roleID, ok := settings.NoCaptionsRole(serverID); ok {
		notCaptioned = "anyone with " + discord.RoleMention(roleID)
	}

	return "- Note-takers: " + noteTakers +
		"\n- Captions go in: " + captionsChannel +
		"\n- Not captioned: " + notCaptioned
}

// canManageTranscription reports whether the person who ran the command may start
// or stop captions in this server. Moderators (Manage Channels) always can, and so
// can anyone with the note-taker role an admin picked with /captions-setup.
func canManageTranscription(e *handler.CommandEvent, serverID snowflake.ID) bool {
	// Discord sends the caller's roles and permissions with every command, so this
	// needs no cache lookups. Member is nil only in DMs, which callers rule out
	// first, but check anyway rather than risk a crash.
	member := e.Member()
	if member == nil {
		return false
	}

	if member.Permissions.Has(discord.PermissionManageChannels) {
		return true
	}

	noteTakerRoleID, roleIsSet := settings.NoteTakerRole(serverID)
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
		"Create Public Threads and Send Messages in Threads there.", discord.ChannelMention(channelID))
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

func createHiddenMessageForCommandCaller(e *handler.CommandEvent, message string) error {
	return e.CreateMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: message,
	})
}
