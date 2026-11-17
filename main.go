package main

import (
	"bufio"
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
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"

	"livetranscribebot/captionbot"
	"livetranscribebot/settings"
	"livetranscribebot/speech"
	"livetranscribebot/speechmodel"
	"livetranscribebot/startup"
	"livetranscribebot/updater"
)

// version is set by the release build (-ldflags "-X main.version=v1.2.3"). A
// build from the code is "dev", which never updates itself.
var version = "dev"

// updateCheckEvery is how often a running bot looks for a new release.
const updateCheckEvery = 24 * time.Hour

// tokenHint is added to errors connecting to Discord, the usual cause of
// which is a wrong or reset token.
const tokenHint = "If Discord says the token is wrong (401 Unauthorized, or 4004 Authentication failed), delete the .env file next to this program and run it again to paste a new one."

// main wires the pieces together: updates, the token, each server's settings,
// the speech model, the Discord side (captionbot), and the connection to
// Discord. It runs until Ctrl+C, then shuts down cleanly.
func main() {
	fmt.Printf("Hopamine Live Captions %s\n", version)

	// Found before anything else: an update moves the running program aside,
	// and restarting needs the path it was started from.
	executable, err := os.Executable()
	if err != nil {
		fatal("couldn't find where this program is: %v", err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		fatal("couldn't find the current folder: %v", err)
	}
	// settings.json, .env and the speech model are all found in this folder.
	// HOW-TO-RUN.txt is always in the download, next to the program.
	appDir := startup.AppDir(executable, workingDir, "HOW-TO-RUN.txt")
	if err := os.Chdir(appDir); err != nil {
		fatal("couldn't open the program's folder %s: %v", appDir, err)
	}

	// Release builds only: a build from the code mustn't update itself, or
	// tidy *.old files out of the project.
	if updater.IsRelease(version) {
		updater.CleanupOld(appDir)
		if _, updated := updater.Update(version, appDir); updated {
			restart(executable)
		}
	}

	token, err := startup.Token(appDir, os.Stdin, os.Stdout)
	if err != nil {
		fatal("couldn't get the bot token: %v", err)
	}

	// The first run downloads the speech model (it isn't in the download).
	if err := speechmodel.Ensure(appDir, os.Stdout); err != nil {
		fatal("%v\nStart the bot again to try again (if it couldn't connect, check the internet connection first). It carries on where it stopped.", err)
	}

	serverSettings, err := settings.Load("settings.json")
	if err != nil {
		fatal("couldn't load settings: %v", err)
	}

	// The model is deliberately never freed. On shutdown disgo closes each
	// transcriber from its own goroutine, *after* the client has closed, and a
	// transcriber finishing someone's last sentence would crash if the model
	// had been freed underneath it. The OS frees it when the bot exits.
	recogniser, err := speech.NewRecogniser(speech.DefaultModelDir)
	if err != nil {
		fatal("couldn't load the speech model: %v", err)
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
		// Joins, leaves and moves: someone else disconnecting or moving the bot
		// ends its session, and a call left empty for a minute ends too.
		bot.WithEventListenerFunc(captionBot.OnVoiceJoin),
		bot.WithEventListenerFunc(captionBot.OnVoiceLeave),
		bot.WithEventListenerFunc(captionBot.OnVoiceMove),
		// Messages typed in a captioned call's chat are copied into its thread.
		bot.WithEventListenerFunc(captionBot.OnMessageCreate),
		bot.WithEventListenerFunc(captionBot.OnMessageUpdate),
	)
	if err != nil {
		fatal("couldn't set up the Discord connection: %v\n%s", err, tokenHint)
	}

	err = handler.SyncCommands(client, captionbot.Commands, nil)
	if err != nil {
		fatal("couldn't register the bot's commands with Discord: %v\n%s", err, tokenHint)
	}

	err = client.OpenGateway(context.Background())
	if err != nil {
		fatal("couldn't connect to Discord: %v\n%s", err, tokenHint)
	}

	fmt.Println("Bot is online. Keep this window open while captions are needed; Ctrl+C stops it.")

	updateInstalled := make(chan struct{})
	go updateWhenIdle(captionBot, appDir, updateInstalled)

	// Wait for Ctrl+C, a stop from the system (SIGTERM), or the terminal
	// window being closed (SIGHUP).
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	select {
	case <-sc:
		captionBot.ShutDown(client)
	case <-updateInstalled:
		log.Printf("restarting into the new version")
		captionBot.ShutDown(client)
		restart(executable)
	}
}

// updateWhenIdle checks for a new release once a day. Once one is installed,
// it waits until no captions are running, so a meeting's captions are never
// cut off, then tells main to restart into it.
func updateWhenIdle(captionBot *captionbot.Bot, appDir string, installed chan<- struct{}) {
	if !updater.IsRelease(version) {
		return
	}
	for {
		time.Sleep(updateCheckEvery)
		if _, updated := updater.Update(version, appDir); !updated {
			continue
		}
		for captionBot.Running() {
			time.Sleep(time.Minute)
		}
		close(installed)
		return
	}
}

// fatal logs what went wrong and waits for Enter before exiting, so a window
// opened by double-clicking the program doesn't close before it can be read.
func fatal(format string, args ...any) {
	log.Printf(format, args...)
	fmt.Println("Press Enter to close.")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(1)
}
