package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	_ "github.com/joho/godotenv/autoload"

	"livetranscribebot/captionbot"
	"livetranscribebot/settings"
	"livetranscribebot/speech"
)

// main wires the pieces together: each server's settings, the speech model,
// the Discord side (captionbot), and the connection to Discord. It runs until
// Ctrl+C, then shuts down cleanly.
func main() {
	token := os.Getenv("BOT_TOKEN")

	serverSettings, err := settings.Load("settings.json")
	if err != nil {
		log.Fatalf("couldn't load settings: %v", err)
	}

	// The model is deliberately never freed. On shutdown disgo closes each
	// transcriber from its own goroutine, *after* the client has closed, and a
	// transcriber finishing someone's last sentence would crash if the model
	// had been freed underneath it. The OS frees it when the bot exits.
	recogniser, err := speech.NewRecogniser(speech.DefaultModelDir)
	if err != nil {
		log.Fatalf("failed to setup the new voice recogniser %v", err)
	}

	captionBot := captionbot.New(serverSettings, recogniser)

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
		bot.WithEventListeners(captionBot.Router()),
		// stop the session if someone else disconnects or moves the bot
		bot.WithEventListenerFunc(captionBot.OnBotLeftVoice),
		bot.WithEventListenerFunc(captionBot.OnBotMovedVoice),
	)
	if err != nil {
		log.Fatal(err)
	}

	err = handler.SyncCommands(client, captionbot.Commands, nil)
	if err != nil {
		log.Fatal(err)
	}

	err = client.OpenGateway(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Bot is online")

	// Wait for Ctrl+C, a stop from the system (SIGTERM), or the terminal
	// window being closed (SIGHUP).
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	<-sc

	captionBot.ShutDown(client)
}
