package captionbot

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
	"livetranscribebot/transcript"
)

const (
	// How long a finished call's transcript waits for its last captions and
	// closing line to be posted, so they're in it.
	posterTimeout = 30 * time.Second

	// Discord hands back at most this many messages at a time.
	messagesPerPage = 100

	transcriptHeading = "📄 Transcript"
)

// transcriptWhenPosted posts a call's transcript once its poster has sent the
// last captions and the closing line, so they're all in it. It runs in the
// background: ending a call doesn't wait for it, but shutdown does.
func (b *Bot) transcriptWhenPosted(client *bot.Client, call *liveCall) {
	b.transcripts.Add(1)
	go func() {
		defer b.transcripts.Done()
		select {
		case <-call.poster.Done():
		case <-time.After(b.posterWait):
			log.Printf("the captions in thread %s never finished posting, so I didn't post its transcript", call.threadID)
			return
		}
		if err := b.postTranscript(client, call.threadID, call.threadName, transcriptHeading); err != nil {
			log.Printf("couldn't post the transcript in thread %s: %v", call.threadID, err)
		}
	}()
}

// postTranscript reads a captions thread back, builds its transcript and posts
// it in the thread as .txt and .md files.
func (b *Bot) postTranscript(client *bot.Client, threadID snowflake.ID, threadName, heading string) error {
	messages, err := fetchThread(client, threadID)
	if err != nil {
		return fmt.Errorf("reading the thread: %w", err)
	}

	lines := transcript.Lines(messages, client.ID())
	text := transcript.Text(threadName, lines, b.timezone)
	markdown := transcript.Markdown(threadName, lines, b.timezone)
	_, err = client.Rest.CreateMessage(threadID, discord.MessageCreate{
		Content:         heading,
		AllowedMentions: captions.NoPings,
		Files: []*discord.File{
			discord.NewFile(transcript.FileName(threadName, "txt"), "", strings.NewReader(text)),
			discord.NewFile(transcript.FileName(threadName, "md"), "", strings.NewReader(markdown)),
		},
	})
	if err != nil {
		return fmt.Errorf("posting it: %w", err)
	}
	return nil
}

// fetchThread reads every message in a thread, oldest first. Discord hands
// them back 100 at a time, so it asks for the next 100 after the newest one
// so far until there are no more.
func fetchThread(client *bot.Client, threadID snowflake.ID) ([]discord.Message, error) {
	var messages []discord.Message
	// Every message in a thread comes after the thread itself, so start there.
	after := threadID
	for {
		page, err := client.Rest.GetMessages(threadID, 0, 0, after, messagesPerPage)
		if err != nil {
			return nil, err
		}
		messages = append(messages, page...)
		for _, message := range page {
			after = max(after, message.ID)
		}
		if len(page) < messagesPerPage {
			break
		}
	}

	// Discord doesn't promise an order, so put them oldest first (IDs grow
	// over time).
	slices.SortFunc(messages, func(a, b discord.Message) int { return cmp.Compare(a.ID, b.ID) })
	return messages, nil
}

// onTranscribeExport rebuilds a captions thread's transcript, e.g. after
// note-takers have fixed captions since it was posted. It's run inside the
// thread.
func (b *Bot) onTranscribeExport(e *handler.CommandEvent) error {
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}
	if !b.canManageTranscription(e, *guildID) {
		return createHiddenMessageForCommandCaller(e, "Only note-takers and moderators can rebuild transcripts.")
	}

	// Answer straight away with a hidden "thinking…": looking the thread up and
	// reading a long thread back can take longer than the 3 seconds Discord
	// allows for a first answer.
	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}
	answer := func(text string) error {
		_, err := e.UpdateInteractionResponse(discord.MessageUpdate{Content: &text})
		return err
	}

	// It has to be one of this bot's captions threads: a thread the bot
	// started in the captions channel. The thread is looked up from Discord
	// rather than read from the command, because the channel details sent
	// with a command may not say who started it.
	notACaptionsThread := "Run /transcribe export inside a captions thread, and I'll rebuild that thread's transcript."
	if e.Channel().MessageChannel == nil {
		return answer(notACaptionsThread)
	}
	channel, err := e.Client().Rest.GetChannel(e.Channel().ID())
	if err != nil {
		log.Printf("couldn't look up channel %s for /transcribe export: %v", e.Channel().ID(), err)
		return answer("I couldn't look this thread up, try again in a moment.")
	}
	captionsChannelID, _ := b.settings.CaptionsChannel(*guildID)
	thread, isThread := channel.(discord.GuildThread)
	if !isThread || thread.OwnerID != e.Client().ID() || thread.ParentID() == nil || *thread.ParentID() != captionsChannelID {
		return answer(notACaptionsThread)
	}

	heading := fmt.Sprintf("📄 Updated transcript (rebuilt by %s)", captions.EscapeMarkdown(e.Member().EffectiveName()))
	if err := b.postTranscript(e.Client(), thread.ID(), thread.Name(), heading); err != nil {
		log.Printf("couldn't rebuild the transcript in thread %s: %v", thread.ID(), err)
		return answer("I couldn't rebuild the transcript, try again in a moment.")
	}
	return answer("Posted the updated transcript in this thread.")
}
