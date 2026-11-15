package captionbot

import (
	"context"
	"log"
	"time"

	"github.com/disgoorg/disgo/bot"

	"livetranscribebot/captions"
)

// ShutDown ends every running session so nothing is left half-done. The order
// matters:
//  1. clear each call's status while the bot is still in it
//  2. leave every call: each transcriber then finishes everyone's last
//     sentence and hands it to its poster
//  3. give the posters a few seconds to get those into the threads
//  4. only then close the client: once its connection to Discord is closed,
//     anything still trying to post would hang
func (b *Bot) ShutDown(client *bot.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b.clearStatusesOnShutdown(client)
	client.VoiceManager.Close(ctx)
	if !captions.WaitForPosters(&b.posters, 5*time.Second) {
		log.Printf("some captions were still being posted when I shut down")
	}
	client.Close(ctx)
}

// clearStatusesOnShutdown takes the "live captions" status off every call
// being captioned, while the bot is still in them (setting a call's status
// needs the bot connected to it). Without this, shutting the bot down leaves
// "Live captions" showing on calls nobody is captioning.
func (b *Bot) clearStatusesOnShutdown(client *bot.Client) {
	for _, running := range b.sessions.All() {
		clearVoiceStatus(client, running.ChannelID)
	}
}
