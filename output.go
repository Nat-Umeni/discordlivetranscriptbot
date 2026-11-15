package main

import "github.com/disgoorg/snowflake/v2"

// captionOutput is where a transcriber's text goes: the terminal, the captions
// thread, or a test's record.
// caption and notCaptioned are called while the transcriber holds its lock, so
// they must return straight away: anything slow (like posting to Discord)
// happens elsewhere, or everyone's audio would wait for it.
type captionOutput interface {
	// caption passes on what someone has said so far in their current
	// sentence. final is true once the sentence has ended; their next words
	// start a new one.
	caption(userID snowflake.ID, text string, final bool)

	// notCaptioned says someone with the no-captions role started talking.
	notCaptioned(userID snowflake.ID)

	// closed says the transcriber has finished: everyone's last sentence has
	// been passed on and nothing more will come.
	closed()
}

// multiOutput sends everything to several outputs, e.g. the terminal and the
// captions thread.
type multiOutput []captionOutput

func (m multiOutput) caption(userID snowflake.ID, text string, final bool) {
	for _, out := range m {
		out.caption(userID, text, final)
	}
}

func (m multiOutput) notCaptioned(userID snowflake.ID) {
	for _, out := range m {
		out.notCaptioned(userID)
	}
}

func (m multiOutput) closed() {
	for _, out := range m {
		out.closed()
	}
}
