package main

import (
	"log"

	"github.com/disgoorg/snowflake/v2"
)

// terminalOutput logs finished captions to the terminal under each speaker's
// name. Captions still being spoken aren't logged: they change about twice a
// second per speaker and would flood the terminal.
type terminalOutput struct {
	nameFor nameFunc
}

func (o terminalOutput) caption(userID snowflake.ID, text string, final bool) {
	if final {
		log.Printf("%s: %s", o.nameFor(userID), text)
	}
}

func (o terminalOutput) notCaptioned(userID snowflake.ID) {
	log.Printf("%s isn't being captioned (no-captions role)", o.nameFor(userID))
}

// closed has nothing to finish: every line is logged as it arrives.
func (o terminalOutput) closed() {}
