package captionbot

import (
	"context"
	"log"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// OnBotLeftVoice runs whenever anyone leaves a voice channel. When it's the bot and a
// session is still running, someone disconnected it by hand (or Discord dropped it).
func (b *Bot) OnBotLeftVoice(e *events.GuildVoiceLeave) {
	// GuildVoiceLeave fires for every member; only the bot leaving matters here.
	if e.VoiceState.UserID != e.Client().ID() {
		return
	}
	b.endSessionAfterOutsideVoiceChange(e.Client(), e.VoiceState.GuildID, "disconnected from voice")
}

// OnBotMovedVoice runs whenever anyone is moved to a different voice channel. Our own
// code never moves the bot (/transcribe start refuses while a session is running, and
// joining with a fresh connection counts as a join, not a move), so if the bot moves
// while a session is running, someone else dragged it.
func (b *Bot) OnBotMovedVoice(e *events.GuildVoiceMove) {
	// GuildVoiceMove fires for every member; only the bot moving matters here.
	if e.VoiceState.UserID != e.Client().ID() {
		return
	}
	b.endSessionAfterOutsideVoiceChange(e.Client(), e.VoiceState.GuildID, "moved to another channel")
}

// endSessionAfterOutsideVoiceChange stops a server's session when the bot was
// disconnected or moved by someone else rather than by /transcribe stop or start.
// Without this the server would look busy, and /transcribe start would keep saying
// "already transcribing", until someone ran /transcribe stop. After a move, the bot
// would also carry on in a channel nobody asked it to caption.
func (b *Bot) endSessionAfterOutsideVoiceChange(client *bot.Client, guildID snowflake.ID, whatHappened string) {
	// No session means the change was ours: /transcribe stop or a failed join
	// already cleaned up, so there's nothing to do.
	current, running := b.sessions.Get(guildID)
	if !running {
		return
	}

	log.Printf("I was %s in server %s by someone else, ending that session", whatHappened, guildID)

	// Try to take the "live captions" status off the call. The bot may already
	// be out of it, in which case Discord can refuse; that's only logged.
	clearVoiceStatus(client, current.ChannelID)

	// Close leaves voice (if a move left the bot connected somewhere) and removes
	// disgo's connection. disgo keeps a dropped connection registered, and
	// CreateConn would hand that dead one to the next /transcribe start.
	voiceConnection := client.VoiceManager.GetConn(guildID)
	if voiceConnection != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		voiceConnection.Close(ctx)
	}

	b.sessions.Remove(guildID)
}
