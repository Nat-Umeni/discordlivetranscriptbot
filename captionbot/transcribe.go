package captionbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"

	"livetranscribebot/sessions"
	"livetranscribebot/speech"
)

func (b *Bot) onTranscribeStart(e *handler.CommandEvent) error {
	// check where the command was called
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	// only note-takers and moderators can start captions
	if !b.canManageTranscription(e, *guildID) {
		message := "Only note-takers and moderators can start captions here."
		if _, roleIsSet := b.settings.NoteTakerRole(*guildID); !roleIsSet {
			message += " An admin can choose the note-taker role with /captions-setup."
		}
		return createHiddenMessageForCommandCaller(e, message)
	}

	// Captions need somewhere to go, and people need a way to opt out, before
	// it's worth joining.
	captionsChannelID, captionsChannelSet := b.settings.CaptionsChannel(*guildID)
	noCaptionsRoleID, noCaptionsRoleSet := b.settings.NoCaptionsRole(*guildID)
	var missing []string
	if !captionsChannelSet {
		missing = append(missing, "a captions channel (captions-channel)")
	}
	if !noCaptionsRoleSet {
		missing = append(missing, "a role for people who don't want to be captioned (no-captions-role)")
	}
	if len(missing) > 0 {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("Before I can caption here, an admin needs to pick %s with /captions-setup.",
			strings.Join(missing, " and ")))
	}

	// find the chat
	voiceState, ok := e.Client().Caches.VoiceState(*guildID, e.User().ID)
	if !ok {
		return createHiddenMessageForCommandCaller(e, "I can't see you in a chat, join one then run the command again so I know where to go")
	}

	clickableChatName := discord.ChannelMention(*voiceState.ChannelID)
	title := e.SlashCommandInteractionData().String("title")

	// Claim this server for a new session. A bot can only be in one voice channel
	// per server, so if a session is already running, joining would move the bot
	// out of that call and end it. start checks and records in one locked step,
	// so two people running this at the same moment can't both get through.
	existingSession, claimed := b.sessions.Start(*guildID, sessions.Session{
		StartedBy: e.User().ID,
		ChannelID: *voiceState.ChannelID,
		Title:     title,
	})

	if !claimed {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm already transcribing '%s' in %s, use /transcribe stop first",
			existingSession.Title, discord.ChannelMention(existingSession.ChannelID)))
	}

	// From here on, any return before the bot has joined must give the claim back,
	// or this server would look busy until the bot restarts. A deferred function
	// runs when onTranscribeStart returns, whichever return that is, so it covers
	// every early return below in one place.
	joined := false
	defer func() {
		if !joined {
			b.sessions.Remove(*guildID)
		}
	}()

	canJoin, err := checkBotCanJoin(e, *guildID, *voiceState.ChannelID)
	if err != nil {
		log.Printf("Failed to check permissions to join that chat: %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't check whether I'm allowed to join that chat, try again in a moment")
	}

	if !canJoin {
		return createHiddenMessageForCommandCaller(e, fmt.Sprintf("I don't have permission to join %s, ask an admin to give me View Channel, Connect, Send Messages and Set Voice Channel Status there", clickableChatName))
	}

	// checked again here, not just in /captions-setup: the channel's
	// permissions may have changed since
	canPost, err := checkBotCanPostCaptions(e, *guildID, captionsChannelID)
	if err != nil {
		log.Printf("Failed to check permissions in the captions channel: %v", err)
		return createHiddenMessageForCommandCaller(e, "I couldn't check whether I can post in the captions channel. If it's been deleted, pick another with /captions-setup captions-channel.")
	}

	if !canPost {
		return createHiddenMessageForCommandCaller(e, missingCaptionsPermissionsMessage(captionsChannelID))
	}

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

	// Joined, so now tell everyone, before listening to anything: a notice in
	// the call's chat, the captions thread and the call's status. Doing this
	// after joining means a failed join never leaves announcements behind.
	thread, noticeID, err := announceCaptions(e.Client(), captionsCall{
		title:             title,
		voiceChannelID:    *voiceState.ChannelID,
		captionsChannelID: captionsChannelID,
		noCaptionsRoleID:  noCaptionsRoleID,
		startedBy:         e.User().ID,
	})
	if err != nil {
		// people can't be told, so captions mustn't run: leave again
		voiceConnection.Close(context.Background())
		log.Printf("couldn't announce the captions: %v", err)

		_, err = e.CreateFollowupMessage(discord.MessageCreate{
			Flags: discord.MessageFlagEphemeral,
			Content: fmt.Sprintf("I couldn't post the captions notice in %s's chat and the thread in %s, so I've left again. Try again in a moment.",
				clickableChatName, discord.ChannelMention(captionsChannelID)),
		})
		return err
	}

	// Tell the caller where captions are. If this fails, captions are still
	// running and the thread is in the channel, so log it and carry on.
	_, err = e.CreateFollowupMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: fmt.Sprintf("Captions are live in %s", thread.Mention()),
	})
	if err != nil {
		log.Printf("couldn't send the captions thread link: %v", err)
	}

	// Start listening: live captions for each speaker, posted and growing in the
	// thread, with finished ones also logged to the terminal. Anyone with the
	// no-captions role is skipped.
	output, poster := b.captionOutputs(e.Client(), *guildID, thread.ID())
	voiceConnection.SetOpusFrameReceiver(speech.NewTranscriber(b.recogniser, output,
		b.optedOutOfCaptions(e.Client().Caches, *guildID)))

	// Remember what's needed to end the call tidily later (endSession).
	b.addCall(*guildID, &liveCall{
		poster:         poster,
		voiceChannelID: *voiceState.ChannelID,
		threadID:       thread.ID(),
		noticeID:       noticeID,
	})

	// joined, so keep the claim: the session runs until it's ended
	joined = true
	return nil
}

func (b *Bot) onTranscribeStop(e *handler.CommandEvent) error {
	guildID := e.GuildID()
	if guildID == nil {
		return createHiddenMessageForCommandCaller(e, "I only work inside a server, run this command in one.")
	}

	current, running := b.sessions.Get(*guildID)
	if !running {
		return createHiddenMessageForCommandCaller(e, "I'm not transcribing in this server right now.")
	}

	// The person who started it can always stop it, so a session can't get stuck
	// waiting for a moderator. Anyone else needs to be a note-taker or moderator,
	// so a random member can't cut the captions off mid-meeting.
	if e.User().ID != current.StartedBy && !b.canManageTranscription(e, *guildID) {
		return createHiddenMessageForCommandCaller(e, "Only the person who started these captions, note-takers or moderators can stop them.")
	}

	// Reply first: leaving can take a moment, and Discord gives 3 seconds to answer.
	err := createHiddenMessageForCommandCaller(e, fmt.Sprintf("I'm leaving %s and stopping the captions for '%s'",
		discord.ChannelMention(current.ChannelID), current.Title))
	if err != nil {
		return err
	}

	b.endSession(e.Client(), *guildID, stoppedByLine(e.Member().EffectiveName()))
	return nil
}
