package captions

import (
	"log"

	"github.com/disgoorg/snowflake/v2"
)

// TerminalOutput logs finished captions to the terminal under each speaker's
// name. Captions still being spoken aren't logged: they change about twice a
// second per speaker and would flood the terminal.
type TerminalOutput struct {
	NameFor NameFunc
}

func (o TerminalOutput) Caption(userID snowflake.ID, text string, final bool) {
	if final {
		log.Printf("%s: %s", o.NameFor(userID), text)
	}
}

func (o TerminalOutput) NotCaptioned(userID snowflake.ID) {
	log.Printf("%s isn't being captioned (no-captions role)", o.NameFor(userID))
}

// Closed has nothing to finish: every line is logged as it arrives.
func (o TerminalOutput) Closed() {}
