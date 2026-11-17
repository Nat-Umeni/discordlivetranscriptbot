package captionbot

import (
	"context"
	"log"
	"time"

	"github.com/disgoorg/disgo/bot"

	"livetranscribebot/captions"
)

// shutdownTimeout is how long shutting down waits for the last captions, and
// then the transcripts, to reach Discord.
const shutdownTimeout = 15 * time.Second

// Running reports whether captions are running in any server. An update
// waits until they aren't, so it never cuts a meeting's captions off.
func (b *Bot) Running() bool {
	return len(b.sessions.All()) > 0
}

// ShutDown ends every running session so nothing is left half-done. The order
// matters:
//  1. end each session (closing line, notice marked ended, status cleared,
//     call left): each transcriber then finishes everyone's last sentence and
//     hands it to its poster
//  2. close any voice connection still open
//  3. give the posters a few seconds to get the last captions and closing
//     lines into the threads, then the transcripts a few more to be posted
//  4. only then close the client: once its connection to Discord is closed,
//     anything still trying to post would hang
func (b *Bot) ShutDown(client *bot.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for serverID := range b.sessions.All() {
		b.endSession(client, serverID, shutdownLine)
	}
	client.VoiceManager.Close(ctx)
	if !captions.WaitForPosters(&b.posters, b.shutdownWait) {
		log.Printf("some captions were still being posted when I shut down")
	}
	// Transcripts are posted once their call's captions are in, so this comes
	// after the posters. (WaitForPosters works for any WaitGroup.)
	if !captions.WaitForPosters(&b.transcripts, b.shutdownWait) {
		log.Printf("some transcripts were still being posted when I shut down")
	}
	client.Close(ctx)
}
