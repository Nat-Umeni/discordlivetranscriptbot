package main

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	"gopkg.in/hraban/opus.v2"
)

// These tests send the model's own sample recordings through the transcriber
// as Discord-style Opus packets (one per 20 ms), the same path live audio
// takes, and check what comes out of onText. They skip if the model isn't
// downloaded.

// Loading the model takes a second or two, so every test shares one. It's
// never freed: the test program ends straight after.
var (
	sharedRecogniserOnce sync.Once
	sharedRecogniser     *sherpa.OnlineRecognizer
	sharedRecogniserErr  error
)

func testRecogniser(t *testing.T) *sherpa.OnlineRecognizer {
	t.Helper()
	needModel(t)
	sharedRecogniserOnce.Do(func() {
		sharedRecogniser, sharedRecogniserErr = newRecogniser(testModelDir)
	})
	if sharedRecogniserErr != nil {
		t.Fatal(sharedRecogniserErr)
	}
	return sharedRecogniser
}

// speechPackets turns a 16 kHz mono WAV into 20 ms Opus packets, like the ones
// Discord sends while someone talks.
func speechPackets(t *testing.T, wavPath string) [][]byte {
	t.Helper()
	wave := sherpa.ReadWave(wavPath)
	if wave == nil {
		t.Fatalf("couldn't read %s", wavPath)
	}
	if wave.SampleRate != 16000 {
		t.Fatalf("%s is %d Hz; speechPackets needs 16000", wavPath, wave.SampleRate)
	}
	return encodeOpus(t, wave.Samples)
}

// silencePackets is n packets of silence, like a mic left open between
// sentences.
func silencePackets(t *testing.T, n int) [][]byte {
	t.Helper()
	return encodeOpus(t, make([]float32, n*320))
}

func encodeOpus(t *testing.T, samples []float32) [][]byte {
	t.Helper()
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	buf := make([]byte, 4000)
	for start := 0; start+320 <= len(samples); start += 320 { // 320 samples = 20 ms
		size, err := encoder.EncodeFloat32(samples[start:start+320], buf)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:size]...))
	}
	return packets
}

func sampleWAV(name string) string {
	return filepath.Join(testModelDir, "test_wavs", name)
}

// caption is one call to onText.
type caption struct {
	userID snowflake.ID
	text   string
	final  bool
}

// captionLog records everything the transcriber passes to onText.
type captionLog struct {
	mu    sync.Mutex
	lines []caption
}

func (l *captionLog) record(userID snowflake.ID, text string, final bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, caption{userID, text, final})
}

func (l *captionLog) all() []caption {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]caption(nil), l.lines...)
}

func (l *captionLog) finals() []caption {
	var finals []caption
	for _, line := range l.all() {
		if line.final {
			finals = append(finals, line)
		}
	}
	return finals
}

// testTranscriber is a transcriber on a fake clock, so tests can say "1.3 s
// later" without waiting.
type testTranscriber struct {
	*transcriber
	captions *captionLog
	clock    time.Time
}

func newTestTranscriber(t *testing.T) *testTranscriber {
	t.Helper()
	captions := &captionLog{}
	tt := &testTranscriber{
		transcriber: newTranscriber(testRecogniser(t), captions.record),
		captions:    captions,
		clock:       time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
	}

	// The quiet timer reads t.now from its own goroutine, so swap it (and
	// later move the clock) only while holding the lock.
	tt.mu.Lock()
	tt.now = func() time.Time { return tt.clock }
	tt.mu.Unlock()

	// Stop the quiet timer and free the streams. Safe even if the test
	// already closed it.
	t.Cleanup(tt.Close)
	return tt
}

// speakerCount is how many streams the transcriber is holding.
func (tt *testTranscriber) speakerCount() int {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return len(tt.speakers)
}

func (tt *testTranscriber) hasStream(ssrc uint32) bool {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	_, found := tt.speakers[ssrc]
	return found
}

// captureLog sends log output to a buffer for one test, so the test can check
// what was printed. TestMain discards logs otherwise.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
}

// finalsFor is everything one user has had finished, joined up.
func finalsFor(captions *captionLog, userID snowflake.ID) (count int, text string) {
	for _, line := range captions.finals() {
		if line.userID == userID {
			count++
			text += line.text + " "
		}
	}
	return count, text
}

func (tt *testTranscriber) wait(d time.Duration) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.clock = tt.clock.Add(d)
}

// say sends packets from one stream, 20 ms apart, as disgo would.
func (tt *testTranscriber) say(t *testing.T, ssrc uint32, userID snowflake.ID, packets [][]byte) {
	t.Helper()
	for _, data := range packets {
		tt.wait(20 * time.Millisecond)
		if err := tt.ReceiveOpusFrame(userID, &voice.Packet{SSRC: ssrc, Opus: data}); err != nil {
			t.Fatalf("ReceiveOpusFrame: %v", err)
		}
	}
}

// talker is one person in sayTogether.
type talker struct {
	ssrc    uint32
	userID  snowflake.ID
	packets [][]byte
}

// sayTogether has several people talking at once: their packets arrive
// interleaved, 20 ms apart, as they would from Discord.
func (tt *testTranscriber) sayTogether(t *testing.T, talkers ...talker) {
	t.Helper()
	longest := 0
	for _, person := range talkers {
		longest = max(longest, len(person.packets))
	}
	for i := range longest {
		tt.wait(20 * time.Millisecond)
		for _, person := range talkers {
			if i >= len(person.packets) {
				continue
			}
			packet := &voice.Packet{SSRC: person.ssrc, Opus: person.packets[i]}
			if err := tt.ReceiveOpusFrame(person.userID, packet); err != nil {
				t.Fatalf("ReceiveOpusFrame: %v", err)
			}
		}
	}
}

func containsAll(t *testing.T, text string, words ...string) {
	t.Helper()
	for _, word := range words {
		if !strings.Contains(strings.ToUpper(text), word) {
			t.Errorf("expected %q in %q", word, text)
		}
	}
}

func TestTranscriberPartialsGrowThenOneFinalAfterAPause(t *testing.T) {
	tt := newTestTranscriber(t)
	tt.say(t, 7, 42, speechPackets(t, sampleWAV("0.wav")))

	partials := tt.captions.all()
	if len(partials) < 3 {
		t.Fatalf("expected the caption to grow over several partials, got %v", partials)
	}
	for i, line := range partials {
		if line.final {
			t.Fatalf("nothing should be final while they're still talking, got %v", line)
		}
		if line.userID != 42 {
			t.Errorf("partial credited to %d, want 42", line.userID)
		}
		if line.text == "" {
			t.Error("an empty partial was passed on")
		}
		if i > 0 && line.text == partials[i-1].text {
			t.Errorf("the same partial was passed on twice in a row: %q", line.text)
		}
	}
	t.Logf("last partial: %s", partials[len(partials)-1].text)

	// 1.1 s of quiet: not long enough yet, they might just be taking a breath.
	tt.wait(1100 * time.Millisecond)
	tt.flushQuiet()
	if finals := tt.captions.finals(); len(finals) != 0 {
		t.Fatalf("a final came before the 1.2 s quiet limit: %v", finals)
	}

	// 1.3 s: the sentence is over.
	tt.wait(200 * time.Millisecond)
	tt.flushQuiet()
	finals := tt.captions.finals()
	if len(finals) != 1 {
		t.Fatalf("want exactly one final after the pause, got %v", finals)
	}
	if finals[0].userID != 42 {
		t.Errorf("final credited to %d, want 42", finals[0].userID)
	}
	t.Logf("final: %s", finals[0].text)
	containsAll(t, finals[0].text, "AFTER EARLY NIGHTFALL", "YELLOW LAMPS", "BROTHELS")

	// Still quiet: the same sentence mustn't be finished again.
	tt.wait(5 * time.Second)
	tt.flushQuiet()
	if finals := tt.captions.finals(); len(finals) != 1 {
		t.Fatalf("a quiet speaker got finished twice: %v", finals)
	}

	// Talking again starts a fresh sentence, not one glued onto the last.
	tt.say(t, 7, 42, speechPackets(t, sampleWAV("0.wav"))[:100]) // first 2 s
	tt.wait(1300 * time.Millisecond)
	tt.flushQuiet()
	finals = tt.captions.finals()
	if len(finals) != 2 {
		t.Fatalf("want a second final for the second sentence, got %v", finals)
	}
	second := finals[1].text
	t.Logf("second final: %s", second)
	containsAll(t, second, "AFTER EARLY")
	if strings.Contains(strings.ToUpper(second), "BROTHELS") {
		t.Errorf("the second sentence still has the first one in it: %q", second)
	}
}

func TestTranscriberQuietTimerRunsByItself(t *testing.T) {
	tt := newTestTranscriber(t)
	tt.say(t, 7, 42, speechPackets(t, sampleWAV("0.wav"))[:100]) // first 2 s

	tt.wait(1300 * time.Millisecond)
	// No flushQuiet call here: the timer goroutine checks every 100 ms.
	deadline := time.Now().Add(3 * time.Second)
	for len(tt.captions.finals()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the quiet timer never finished the sentence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	containsAll(t, tt.captions.finals()[0].text, "AFTER EARLY")
}

func TestTranscriberKeepsSpeakersApart(t *testing.T) {
	tt := newTestTranscriber(t)
	tt.sayTogether(t,
		talker{1, 101, speechPackets(t, sampleWAV("0.wav"))},
		talker{2, 102, speechPackets(t, sampleWAV("1.wav"))[:300]}, // first 6 s
	)
	tt.wait(1300 * time.Millisecond)
	tt.flushQuiet()

	said := map[snowflake.ID]string{}
	for _, line := range tt.captions.finals() {
		said[line.userID] += line.text + " "
	}
	t.Logf("101: %s", said[101])
	t.Logf("102: %s", said[102])

	containsAll(t, said[101], "NIGHTFALL", "YELLOW LAMPS")
	containsAll(t, said[102], "DIRECT CONSEQUENCE")
	if strings.Contains(strings.ToUpper(said[101]), "CONSEQUENCE") ||
		strings.Contains(strings.ToUpper(said[102]), "NIGHTFALL") {
		t.Error("one speaker's words ended up in the other's caption")
	}
	if len(said) != 2 {
		t.Errorf("want finals for exactly 2 speakers, got %v", said)
	}
}

func TestTranscriberCreditsTheRealUserOnceDiscordSaysWhoItIs(t *testing.T) {
	tt := newTestTranscriber(t)
	packets := speechPackets(t, sampleWAV("0.wav"))[:150] // first 3 s

	// The first half second arrives before Discord has said whose stream it
	// is (user 0), then the real ID turns up.
	tt.say(t, 7, 0, packets[:25])
	tt.say(t, 7, 42, packets[25:])
	tt.wait(1300 * time.Millisecond)
	tt.flushQuiet()

	finals := tt.captions.finals()
	if len(finals) != 1 {
		t.Fatalf("want one final, got %v", finals)
	}
	if finals[0].userID != 42 {
		t.Errorf("final credited to %d, want 42", finals[0].userID)
	}
	// The opening words came in as user 0 and must still be there.
	containsAll(t, finals[0].text, "AFTER EARLY NIGHTFALL")
}

// Someone with an open mic keeps sending packets through their pauses, so the
// quiet timer never fires; the model hears the silence and ends the sentence
// itself.
func TestTranscriberOpenMicPauseEndsTheSentence(t *testing.T) {
	tt := newTestTranscriber(t)
	tt.say(t, 7, 42, speechPackets(t, sampleWAV("0.wav")))
	tt.say(t, 7, 42, silencePackets(t, 150)) // 3 s of silence

	finals := tt.captions.finals()
	if len(finals) != 1 {
		t.Fatalf("want one final from the model's own pause detection, got %v", finals)
	}
	containsAll(t, finals[0].text, "NIGHTFALL", "BROTHELS")

	// The stream was wiped, so the quiet timer has nothing left to finish.
	tt.wait(1300 * time.Millisecond)
	tt.flushQuiet()
	if finals := tt.captions.finals(); len(finals) != 1 {
		t.Errorf("an empty sentence was passed on as a final: %v", finals)
	}
}

func TestTranscriberBadPacketIsAnError(t *testing.T) {
	tt := newTestTranscriber(t)
	if err := tt.ReceiveOpusFrame(42, &voice.Packet{SSRC: 7, Opus: []byte{}}); err == nil {
		t.Fatal("an empty packet can't be decoded and should return an error for disgo to log")
	}
	if lines := tt.captions.all(); len(lines) != 0 {
		t.Errorf("nothing should be passed on, got %v", lines)
	}
}

func TestTranscriberCleanupUserFinishesOnlyThatSpeaker(t *testing.T) {
	tt := newTestTranscriber(t)
	tt.sayTogether(t,
		talker{1, 101, speechPackets(t, sampleWAV("0.wav"))[:150]}, // first 3 s
		talker{2, 102, speechPackets(t, sampleWAV("1.wav"))[:150]},
	)

	// 101 leaves the call mid-sentence: what they'd said so far is finished.
	tt.CleanupUser(101)
	count, said := finalsFor(tt.captions, 101)
	if count != 1 {
		t.Fatalf("want one final for the speaker who left, got %d", count)
	}
	t.Logf("101: %s", said)
	containsAll(t, said, "AFTER EARLY")
	if tt.hasStream(1) {
		t.Error("the leaver's stream should be freed and forgotten")
	}

	// 102 is still talking and mustn't be cut off.
	if count, _ := finalsFor(tt.captions, 102); count != 0 {
		t.Error("102 is still talking but got finished too")
	}
	if !tt.hasStream(2) {
		t.Fatal("102's stream should be kept")
	}

	// 102 carries on as normal: their sentence ends at the pause.
	tt.wait(1300 * time.Millisecond)
	tt.flushQuiet()
	count, said = finalsFor(tt.captions, 102)
	if count != 1 {
		t.Fatalf("want one final for 102 after their pause, got %d", count)
	}
	t.Logf("102: %s", said)
	containsAll(t, said, "DIRECT")

	// 102 leaves after their sentence already finished: nothing more to say.
	tt.CleanupUser(102)
	if count, _ := finalsFor(tt.captions, 102); count != 1 {
		t.Errorf("an already-finished sentence was passed on again, %d finals", count)
	}
	if n := tt.speakerCount(); n != 0 {
		t.Errorf("everyone left, but %d streams are still held", n)
	}
}

func TestTranscriberCloseFinishesEveryoneAndIgnoresLatePackets(t *testing.T) {
	tt := newTestTranscriber(t)
	sam := speechPackets(t, sampleWAV("0.wav"))[:150] // first 3 s
	tt.sayTogether(t,
		talker{1, 101, sam},
		talker{2, 102, speechPackets(t, sampleWAV("1.wav"))[:150]},
	)

	// /transcribe stop while both are mid-sentence.
	tt.Close()
	for _, test := range []struct {
		userID snowflake.ID
		word   string
	}{{101, "AFTER EARLY"}, {102, "DIRECT"}} {
		count, said := finalsFor(tt.captions, test.userID)
		if count != 1 {
			t.Errorf("%d: want one final on Close, got %d", test.userID, count)
		}
		containsAll(t, said, test.word)
	}
	if n := tt.speakerCount(); n != 0 {
		t.Errorf("Close should free every stream, %d left", n)
	}

	// A packet still in flight when the call ended is ignored.
	before := len(tt.captions.all())
	if err := tt.ReceiveOpusFrame(101, &voice.Packet{SSRC: 1, Opus: sam[0]}); err != nil {
		t.Errorf("a late packet isn't an error, got %v", err)
	}
	if tt.speakerCount() != 0 {
		t.Error("a late packet made a new stream after Close")
	}

	tt.Close() // a second Close (disgo can call it twice) must not panic
	if after := len(tt.captions.all()); after != before {
		t.Errorf("nothing should be passed on after Close, got %d more", after-before)
	}
}

// No model needed: nobody talks, so no stream is ever made. Runs even
// without the model downloaded.
func TestTranscriberWithNobodyTalking(t *testing.T) {
	captions := &captionLog{}
	tr := newTranscriber(nil, captions.record)

	tr.CleanupUser(7) // joined and left without saying anything
	tr.flushQuiet()
	tr.Close()
	tr.Close()

	if lines := captions.all(); len(lines) != 0 {
		t.Errorf("nobody spoke, so nothing should be passed on, got %v", lines)
	}
}

// disgo calls ReceiveOpusFrame from its read loop and CleanupUser from the
// voice gateway, while the quiet timer runs. Run with -race: without the lock
// this fails.
func TestTranscriberConcurrentUse(t *testing.T) {
	tt := newTestTranscriber(t)
	packets := speechPackets(t, sampleWAV("0.wav"))[:50] // 1 s

	var wg sync.WaitGroup
	for i := range 6 {
		ssrc := uint32(i%3 + 1)
		userID := snowflake.ID(100 + i%3)
		wg.Add(3)
		go func() {
			defer wg.Done()
			for _, data := range packets {
				tt.wait(20 * time.Millisecond)
				_ = tt.ReceiveOpusFrame(userID, &voice.Packet{SSRC: ssrc, Opus: data})
			}
		}()
		go func() {
			defer wg.Done()
			tt.CleanupUser(userID)
		}()
		go func() {
			defer wg.Done()
			tt.wait(1300 * time.Millisecond)
			tt.flushQuiet()
		}()
	}
	wg.Wait()

	tt.Close()
	if n := tt.speakerCount(); n != 0 {
		t.Errorf("Close should free every stream, %d left", n)
	}
}

func TestPrintCaption(t *testing.T) {
	logs := captureLog(t)

	printCaption(42, "so I rang", false)
	printCaption(42, "So I rang him.", true)

	for _, want := range []string{"42 … so I rang\n", "42: So I rang him.\n"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, got:\n%s", want, logs)
		}
	}
}
