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
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagVoiceStates)),
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
		return createHiddenMessageForCommandCaller(e, "Run this command inside of a server.")
	}

	// find the chat
	voiceState, ok := e.Client().Caches.VoiceState(*guildID, e.User().ID)
	if !ok {
		return createHiddenMessageForCommandCaller(e, "It looks like you aren't in a chat, join one then run the command again")
	}

	clickableChatName := discord.ChannelMention(*voiceState.ChannelID)
	title := e.SlashCommandInteractionData().String("title")

	err := createHiddenMessageForCommandCaller(e, fmt.Sprintf("Joining %s and setting chat title to '%s'", clickableChatName, title))
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
			Content: "Discord took too long to answer, try again",
		})
		return err
	}

	return nil
}

func createHiddenMessageForCommandCaller(e *handler.CommandEvent, message string) error {
	return e.CreateMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: message,
	})
}
