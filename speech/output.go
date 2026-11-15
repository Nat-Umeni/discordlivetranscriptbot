package speech

import (
	"github.com/disgoorg/snowflake/v2"
)

// Output is where a transcriber's text goes: the terminal, the captions
// thread, or a test's record.
// Caption and NotCaptioned are called while the transcriber holds its lock, so
// they must return straight away: anything slow (like posting to Discord)
// happens elsewhere, or everyone's audio would wait for it.
type Output interface {
	// Caption passes on what someone has said so far in their current
	// sentence. final is true once the sentence has ended; their next words
	// start a new one.
	Caption(userID snowflake.ID, text string, final bool)

	// NotCaptioned says someone with the no-captions role started talking.
	NotCaptioned(userID snowflake.ID)

	// Closed says the transcriber has finished: everyone's last sentence has
	// been passed on and nothing more will come.
	Closed()
}

// MultiOutput sends everything to several outputs, e.g. the terminal and the
// captions thread.
type MultiOutput []Output

func (m MultiOutput) Caption(userID snowflake.ID, text string, final bool) {
	for _, out := range m {
		out.Caption(userID, text, final)
	}
}

func (m MultiOutput) NotCaptioned(userID snowflake.ID) {
	for _, out := range m {
		out.NotCaptioned(userID)
	}
}

func (m MultiOutput) Closed() {
	for _, out := range m {
		out.Closed()
	}
}

// OptOutFunc says whether someone must not be captioned (e.g. they have the
// server's no-captions role).
type OptOutFunc func(userID snowflake.ID) bool
