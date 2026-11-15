package captionbot

import (
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
)

func createHiddenMessageForCommandCaller(e *handler.CommandEvent, message string) error {
	return e.CreateMessage(discord.MessageCreate{
		Flags:   discord.MessageFlagEphemeral,
		Content: message,
	})
}
