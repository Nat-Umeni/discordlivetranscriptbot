package main

import (
	"log"
	"sync"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

var _ voice.OpusFrameReceiver = (*packetCounter)(nil)

type packetCounter struct {
	mu     sync.Mutex
	counts map[snowflake.ID]int
}

func newPacketCounter() *packetCounter {
	return &packetCounter{
		counts: make(map[snowflake.ID]int),
	}
}

func (pc *packetCounter) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.counts[userID]++

	count := pc.counts[userID]
	if count%50 == 0 {
		log.Printf("heard %d packets (~%d s) from %s", count, count/50, userID)
	}

	return nil
}

func (pc *packetCounter) CleanupUser(userID snowflake.ID) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	count := pc.counts[userID]
	log.Printf("%s left after %d packets (~%d s)", userID, count, count/50)

	delete(pc.counts, userID)
}

func (pc *packetCounter) Close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	for userID, count := range pc.counts {
		log.Printf("%s: %d packets (~%ds)", userID, count, count/50)
	}

	log.Printf("audio stopped")
}
