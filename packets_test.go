package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// captureLog sends log output to a buffer for one test, so the test can check
// what the counter printed. TestMain discards logs otherwise.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
}

// hear feeds n packets from one user, as disgo's receive loop would.
func hear(t *testing.T, counter *packetCounter, userID snowflake.ID, n int) {
	t.Helper()
	for range n {
		if err := counter.ReceiveOpusFrame(userID, &voice.Packet{}); err != nil {
			t.Fatalf("ReceiveOpusFrame should never fail, got %v", err)
		}
	}
}

func TestPacketCounterCountsEachSpeakerSeparately(t *testing.T) {
	counter := newPacketCounter()

	hear(t, counter, 1, 3)
	hear(t, counter, 2, 5)

	if counter.counts[1] != 3 || counter.counts[2] != 5 {
		t.Fatalf("got counts %v, want user 1 → 3 and user 2 → 5", counter.counts)
	}
}

func TestPacketCounterLogsAboutOnceASecond(t *testing.T) {
	logs := captureLog(t)
	counter := newPacketCounter()

	hear(t, counter, 3000, 49)
	if logs.Len() != 0 {
		t.Fatalf("should stay quiet before 50 packets (1 s), logged:\n%s", logs)
	}

	hear(t, counter, 3000, 1)
	if !strings.Contains(logs.String(), "heard 50 packets (~1 s) from 3000") {
		t.Fatalf("should log at 50 packets, logged:\n%s", logs)
	}

	hear(t, counter, 3000, 50)
	if !strings.Contains(logs.String(), "heard 100 packets (~2 s) from 3000") {
		t.Fatalf("should log again at 100 packets, logged:\n%s", logs)
	}
	if lines := strings.Count(logs.String(), "heard"); lines != 2 {
		t.Fatalf("100 packets should give exactly 2 log lines, got %d:\n%s", lines, logs)
	}
}

// User 0 means Discord hasn't said whose stream it is yet; those packets
// still count, under 0, so the log shows how long the lookup took.
func TestPacketCounterCountsUnknownSpeakers(t *testing.T) {
	counter := newPacketCounter()

	hear(t, counter, 0, 4)

	if counter.counts[0] != 4 {
		t.Fatalf("got %d packets for user 0, want 4", counter.counts[0])
	}
}

func TestPacketCounterCleanupUserReportsAndForgetsThem(t *testing.T) {
	logs := captureLog(t)
	counter := newPacketCounter()
	hear(t, counter, 1, 100)
	hear(t, counter, 2, 10)

	counter.CleanupUser(1)

	if !strings.Contains(logs.String(), "1 left after 100 packets (~2 s)") {
		t.Errorf("should log their final count, logged:\n%s", logs)
	}
	if _, still := counter.counts[1]; still {
		t.Error("a user who left should be removed from the map")
	}
	if counter.counts[2] != 10 {
		t.Error("other users must be untouched")
	}
}

func TestPacketCounterCleanupUserNeverHeardIsHarmless(t *testing.T) {
	// someone who joined and left without talking: disgo still calls CleanupUser
	logs := captureLog(t)
	counter := newPacketCounter()

	counter.CleanupUser(7)

	if !strings.Contains(logs.String(), "7 left after 0 packets (~0 s)") {
		t.Errorf("should log a zero count, logged:\n%s", logs)
	}
}

func TestPacketCounterCloseReportsEveryoneStillThere(t *testing.T) {
	logs := captureLog(t)
	counter := newPacketCounter()
	hear(t, counter, 1, 150)
	hear(t, counter, 2, 50)
	logs.Reset() // only look at what Close prints

	counter.Close()

	for _, want := range []string{"1: 150 packets (~3s)", "2: 50 packets (~1s)", "audio stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("Close should log %q, logged:\n%s", want, logs)
		}
	}
}

func TestPacketCounterCloseWithNobodyHeard(t *testing.T) {
	logs := captureLog(t)
	counter := newPacketCounter()

	counter.Close()

	if !strings.Contains(logs.String(), "audio stopped") {
		t.Errorf("should still mark the end, logged:\n%s", logs)
	}
}

// disgo calls ReceiveOpusFrame from its read loop and CleanupUser from the
// voice gateway, at the same time. Run with -race: without the mutex this
// fails with a data race or "concurrent map writes".
func TestPacketCounterConcurrentUse(t *testing.T) {
	counter := newPacketCounter()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			hear(t, counter, snowflake.ID(i%3), 30)
		}()
		go func() {
			defer wg.Done()
			counter.CleanupUser(snowflake.ID(i % 3))
		}()
	}
	wg.Wait()
	counter.Close()
}
