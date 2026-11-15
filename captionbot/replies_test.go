package captionbot

import (
	"testing"
)

func TestHiddenMessagesAreEphemeral(t *testing.T) {
	env := newTestEnv(t)
	event, sent := env.command(t, moderator, stopCommand())

	if err := createHiddenMessageForCommandCaller(event, "hello"); err != nil {
		t.Fatal(err)
	}
	onlyReply(t, sent, "hello")
}
