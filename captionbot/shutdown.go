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
//  1. end each session (closing line, notice marked ended, status cleared,
//     call left): each transcriber then finishes everyone's last sentence and
//     hands it to its poster
//  2. close any voice connection still open
//  3. give the posters a few seconds to get the last captions and closing
//     lines into the threads
//  4. only then close the client: once its connection to Discord is closed,
//     anything still trying to post would hang
func (b *Bot) ShutDown(client *bot.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for serverID := range b.sessions.All() {
		b.endSession(client, serverID, shutdownLine)
	}
	client.VoiceManager.Close(ctx)
	if !captions.WaitForPosters(&b.posters, 5*time.Second) {
		log.Printf("some captions were still being posted when I shut down")
	}
	client.Close(ctx)
}
