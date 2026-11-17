// Command getmodel downloads the speech model into ./models, as the bot does
// on its first run. The tests and the build need it before the bot has run:
//
//	go run ./build/getmodel
package main

import (
	"log"
	"os"

	"livetranscribebot/speechmodel"
)

func main() {
	if err := speechmodel.Ensure(".", os.Stdout); err != nil {
		log.Fatal(err)
	}
	log.Printf("the speech model is in %s", speechmodel.Dir)
}
