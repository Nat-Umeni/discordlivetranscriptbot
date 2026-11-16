package captionbot

import (
	"log"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// OnVoiceJoin runs whenever anyone joins a voice channel. Someone joining a
// call that had emptied cancels the countdown to ending its captions.
func (b *Bot) OnVoiceJoin(e *events.GuildVoiceJoin) {
	b.checkEmpty(e.Client(), e.VoiceState.GuildID)
}

// OnVoiceLeave runs whenever anyone leaves a voice channel. When it's the bot
// and a session is still running, someone disconnected it by hand (or Discord
// dropped it). When it's someone else, the call may now be empty.
func (b *Bot) OnVoiceLeave(e *events.GuildVoiceLeave) {
	if e.VoiceState.UserID == e.Client().ID() {
		b.endAfterOutsideChange(e.Client(), e.VoiceState.GuildID, "disconnected from voice", disconnectedLine)
		return
	}
	b.checkEmpty(e.Client(), e.VoiceState.GuildID)
}

// OnVoiceMove runs whenever anyone is moved to a different voice channel. Our
// own code never moves the bot (/transcribe start refuses while a session is
// running, and joining with a fresh connection counts as a join, not a move),
// so if the bot moves while a session is running, someone else dragged it.
// Anyone else moving may have emptied the call, or come back to it.
func (b *Bot) OnVoiceMove(e *events.GuildVoiceMove) {
	if e.VoiceState.UserID == e.Client().ID() {
		b.endAfterOutsideChange(e.Client(), e.VoiceState.GuildID, "moved to another channel", movedLine)
		return
	}
	b.checkEmpty(e.Client(), e.VoiceState.GuildID)
}

// endAfterOutsideChange stops a server's session when the bot was disconnected
// or moved by someone else rather than by /transcribe stop or start. Without
// this the server would look busy, and /transcribe start would keep saying
// "already transcribing", until someone ran /transcribe stop. After a move, the
// bot would also carry on in a channel nobody asked it to caption.
func (b *Bot) endAfterOutsideChange(client *bot.Client, guildID snowflake.ID, whatHappened, closingLine string) {
	// No session means the change was ours: /transcribe stop or a failed join
	// already cleaned up, so there's nothing to do.
	if _, running := b.sessions.Get(guildID); !running {
		return
	}

	log.Printf("I was %s in server %s by someone else, ending that session", whatHappened, guildID)
	b.endSession(client, guildID, closingLine)
}
