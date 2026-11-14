package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/handler/middleware"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
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
}

func main() {
	token := os.Getenv("BOT_TOKEN")

	r := handler.New()
	r.Use(middleware.Go)
	r.Route("/transcribe", func(r handler.Router) {
		r.Command("/start", onTranscribeStart)
	})

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

	// find the chat
	voiceState, ok := e.Client().Caches.VoiceState(*guildID, e.User().ID)
	if !ok {
		return createHiddenMessageForCommandCaller(e, "I can't see you in a chat, join one then run the command again so I know where to go")
	}

	// check the bot isn't already transcribing in this server. A bot can only be in
	// one voice channel per server, so joining again would move it out of the
	// current call and end that transcription. GetConn returns nil if the bot has
	// no voice connection here.
	existingConnection := e.Client().VoiceManager.GetConn(*guildID)
	if existingConnection != nil {
		// ChannelID is nil if the connection isn't in a channel right now (e.g. halfway
		// through leaving). Dereferencing nil would panic, and in a middleware.Go
		// goroutine that crashes the whole bot, so fall back to plain wording.
		currentChat := "a chat"
		if existingConnection.ChannelID() != nil {
			currentChat = discord.ChannelMention(*existingConnection.ChannelID())
		}
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm already transcribing in %s, use /transcribe stop first", currentChat))
	}

	canJoin, err := checkBotCanJoin(e, *guildID, *voiceState.ChannelID)
	if err != nil {
		log.Printf("Failed to check permissions to join that chat %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't check whether I'm allowed to join that chat, try again in a moment")
	}

	clickableChatName := discord.ChannelMention(*voiceState.ChannelID)

	if !canJoin {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I don't have permission to join %s, ask an admin to give me View Channel and Connect there", clickableChatName))
	}

	title := e.SlashCommandInteractionData().String("title")
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

	return nil
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
