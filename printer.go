package main

import (
	"log"

	"github.com/disgoorg/snowflake/v2"
)

// captionPrinter is milestone 1's captionFunc: captions go to the terminal
// under each speaker's name. Milestone 2 swaps it for one that posts in the
// Discord thread.
func captionPrinter(nameFor nameFunc) captionFunc {
	return func(userID snowflake.ID, text string, final bool) {
		name := nameFor(userID)
		if final {
			log.Printf("%s: %s", name, text)
			return
		}
		log.Printf("%s … %s", name, text)
	}
}
