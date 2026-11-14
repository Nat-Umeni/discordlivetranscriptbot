package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
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
		Description: "Choose which role counts as note-takers for captions in this server",
		// Only members with Manage Server see it by default. Admins can still
		// change who sees it in Server Settings → Integrations.
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageGuild),
		// Servers only: it makes no sense in a DM.
		Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild},
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionRole{
				Name:        "notetaker-role",
				Description: "Members with this role can start and stop captions",
				Required:    true,
			},
		},
	},
}

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

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentsNonPrivileged, gateway.IntentMessageContent),
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

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
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
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I don't have permission to join %s, ask an admin to give me View Channel and Connect there", clickableChatName))
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

	voiceConnection.SetOpusFrameReceiver(newPacketCounter())

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
	if _, running := activeSessions.get(guildID); !running {
		return
	}

	log.Printf("I was %s in server %s by someone else, ending that session", whatHappened, guildID)

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

	role := e.SlashCommandInteractionData().Role("notetaker-role")

	err := settings.SetNoteTakerRole(*guildID, role.ID)
	if err != nil {
		log.Printf("couldn't save the note-taker role: %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't save that, try again in a moment")
	}

	return createHiddenMessageForCommandCaller(e, fmt.Sprintf("Got it, I'll treat %s as note-takers here. They can now start and stop captions.",
		discord.RoleMention(role.ID)))
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
	// Look up the bot's own member info (which roles it has) in disgo's cache.
	// ok is false if the cache doesn't have it. That doesn't mean the bot isn't in
	// the server; it means we can't check its permissions.
	botMember, memberCacheOk := e.Client().Caches.SelfMember(serverID)
	if !memberCacheOk {
		// Couldn't check: return false with an error
		return false, fmt.Errorf("could not determine if the bot was in the cache")
	}

	channel, channelCacheOk := e.Client().Caches.Channel(channelID)
	if !channelCacheOk {
		return false, fmt.Errorf("could not find that voice channel in the cache to determine join perms, looked for %s", channelID)
	}

	perms := e.Client().Caches.MemberPermissionsInChannel(channel, botMember)

	return perms.Has(discord.PermissionConnect, discord.PermissionViewChannel), nil
}

func createHiddenMessageForCommandCaller(e *handler.CommandEvent, message string) error {
	return e.CreateMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: message,
	})
}
