package speech

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	"gopkg.in/hraban/opus.v2"
)

// newDecoder makes a speaker's Opus decoder. It's a variable only so a test
// can make it fail: with the fixed settings used here it never does.
var newDecoder = opus.NewDecoder

const (
	quietLimit      = 1200 * time.Millisecond
	quietCheckEvery = 100 * time.Millisecond
	tailPadding     = 12800

	// How many packets (20 ms each, so 1 s) to hold per stream while Discord
	// hasn't said whose it is.
	holdLimit = 50
)

type speakerStream struct {
	userID    snowflake.ID
	decoder   *opus.Decoder
	stream    *sherpa.OnlineStream
	lastText  string
	lastHeard time.Time
	idle      bool
}

type Transcriber struct {
	mu         sync.Mutex
	recogniser *sherpa.OnlineRecognizer
	speakers   map[uint32]*speakerStream
	out        Output
	optedOut   OptOutFunc
	now        func() time.Time

	// Packets from streams Discord hasn't named yet, kept undecoded until we
	// know whether that person has opted out.
	held map[uint32][][]byte
	// Who the output has been told isn't being captioned, so it's told once,
	// not 50 times a second.
	skipReported map[snowflake.ID]bool

	done      chan struct{}
	closeOnce sync.Once
	closed    bool
}

// Compile-time check that transcriber has all three methods disgo needs, so
// it can be passed to SetOpusFrameReceiver.
var _ voice.OpusFrameReceiver = (*Transcriber)(nil)

func NewTranscriber(recogniser *sherpa.OnlineRecognizer, out Output, optedOut OptOutFunc) *Transcriber {
	t := &Transcriber{
		recogniser:   recogniser,
		out:          out,
		optedOut:     optedOut,
		speakers:     make(map[uint32]*speakerStream),
		held:         make(map[uint32][][]byte),
		skipReported: make(map[snowflake.ID]bool),
		now:          time.Now,
		done:         make(chan struct{}),
	}

	// Runs alongside everything else until Close, ending the sentences of
	// anyone who's gone quiet.
	go t.runQuietTimer()

	return t
}

// runQuietTimer checks for quiet speakers every quietCheckEvery. Discord sends
// no packets while someone is silent, so without this their last sentence
// would sit unfinished until they next spoke, maybe minutes later.
func (t *Transcriber) runQuietTimer() {
	ticker := time.NewTicker(quietCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C: // another 100 ms has passed
			t.flushQuiet()
		case <-t.done: // Close was called: stop, or this would run forever
			return
		}
	}
}

// flushQuiet finishes the sentence of everyone who's been quiet for at least
// quietLimit.
func (t *Transcriber) flushQuiet() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	for _, speaker := range t.speakers {
		// idle: already finished. Without this check the same silence would
		// be "finished" again every 100 ms.
		if !speaker.idle && now.Sub(speaker.lastHeard) >= quietLimit {
			t.finishSentence(speaker)
		}
	}
}

// finishSentence ends this speaker's current sentence and reports it as final.
// Called while t.mu is already locked, so it mustn't lock again.
func (t *Transcriber) finishSentence(speaker *speakerStream) {
	// The model works in 560 ms chunks and waits for a full one, so the last
	// half-second of words would never be processed. 0.8 s of silence pushes
	// them through.
	speaker.stream.AcceptWaveform(16000, make([]float32, tailPadding))
	for t.recogniser.IsReady(speaker.stream) {
		t.recogniser.Decode(speaker.stream)
	}

	text := t.recogniser.GetResult(speaker.stream).Text
	if text != "" {
		t.out.Caption(speaker.userID, text, true)
	}

	// Start a fresh sentence for when they next talk.
	t.recogniser.Reset(speaker.stream)
	speaker.lastText = ""
	speaker.idle = true
}

// report asks the model what this speaker has said so far and passes on any
// new text. Called while t.mu is already locked, so it mustn't lock again.
func (t *Transcriber) report(speaker *speakerStream) {
	// Catch up: run the model on every full chunk of audio that's waiting.
	for t.recogniser.IsReady(speaker.stream) {
		t.recogniser.Decode(speaker.stream)
	}

	// The whole sentence so far, e.g. "so I rang" then "so I rang him".
	text := t.recogniser.GetResult(speaker.stream).Text

	// Only pass it on if there's something to say and it's not a repeat.
	if text != "" && text != speaker.lastText {
		t.out.Caption(speaker.userID, text, false)
		speaker.lastText = text
	}

	// The model thinks the sentence has ended.
	if t.recogniser.IsEndpoint(speaker.stream) {
		if text != "" {
			t.out.Caption(speaker.userID, text, true)
		}
		// Start a fresh sentence: clear the model's text and ours.
		t.recogniser.Reset(speaker.stream)
		speaker.lastText = ""
	}
}

// ReceiveOpusFrame is called by disgo for every 20 ms packet of someone
// talking. Nobody who has opted out is ever decoded or captioned; everyone else's
// packet goes into their stream and any new text is reported.
func (t *Transcriber) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// After Close the streams are freed, so late packets are ignored.
	if t.closed {
		return nil
	}

	ssrc := packet.SSRC

	// 0 means Discord hasn't said whose stream this is yet, so there's no way
	// to tell whether they've opted out. Keep the packet without decoding it,
	// and catch up once we know who it is. Only the newest holdLimit packets
	// are kept, so a stream that never gets a name can't fill up memory.
	if userID == 0 {
		// A copy: disgo reuses the packet's memory for the next one.
		held := append(t.held[ssrc], slices.Clone(packet.Opus))
		if len(held) > holdLimit {
			held = held[len(held)-holdLimit:]
		}
		t.held[ssrc] = held
		return nil
	}

	held := t.held[ssrc]
	delete(t.held, ssrc)

	if t.optedOut(userID) {
		// Whatever was held for them is thrown away undecoded.
		t.skip(ssrc, userID)
		return nil
	}
	delete(t.skipReported, userID) // report again if they opt out again later

	// Catch up on what arrived before we knew who it was, then this packet. A
	// packet that can't be decoded is skipped; the rest still go in.
	var errs []error
	for _, data := range held {
		errs = append(errs, t.feed(ssrc, userID, data))
	}
	errs = append(errs, t.feed(ssrc, userID, packet.Opus))
	return errors.Join(errs...) // nil if they all worked; disgo logs any error
}

// skip stops captioning someone who has opted out. Called while t.mu is
// already locked.
func (t *Transcriber) skip(ssrc uint32, userID snowflake.ID) {
	// If they were mid-sentence when they got the role, what they'd already
	// said is finished as normal; nothing after it is captioned.
	if speaker, found := t.speakers[ssrc]; found {
		t.dropSpeaker(ssrc, speaker)
	}

	if !t.skipReported[userID] {
		t.out.NotCaptioned(userID)
		t.skipReported[userID] = true
	}
}

// feed decodes one packet into its speaker's stream and reports any new text.
// Called while t.mu is already locked.
func (t *Transcriber) feed(ssrc uint32, userID snowflake.ID, data []byte) error {
	// Find this stream's speaker, keyed by stream number (SSRC).
	speaker, found := t.speakers[ssrc]
	if !found {
		// Discord sends 48 kHz stereo; this makes libopus hand back 16 kHz
		// mono, which is what the model wants.
		decoder, err := newDecoder(16000, 1)
		if err != nil {
			return err
		}
		speaker = &speakerStream{
			userID:  userID,
			decoder: decoder,
			stream:  sherpa.NewOnlineStream(t.recogniser),
		}
		t.speakers[ssrc] = speaker
	}

	// 1920 samples = 120 ms, the longest packet Opus allows. Discord's 20 ms
	// packets use 320 of them; n says how many were filled.
	pcm := make([]float32, 1920)
	n, err := speaker.decoder.DecodeFloat32(data, pcm)
	if err != nil {
		return err
	}

	speaker.stream.AcceptWaveform(16000, pcm[:n])
	speaker.lastHeard = t.now()
	speaker.idle = false

	t.report(speaker)
	return nil
}

// CleanupUser is called by disgo when someone leaves the voice channel: their
// last sentence is finished and their stream freed.
func (t *Transcriber) CleanupUser(userID snowflake.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Every stream credited to them (usually one). Deleting from a map while
	// ranging over it is allowed in Go.
	for ssrc, speaker := range t.speakers {
		if speaker.userID == userID {
			t.dropSpeaker(ssrc, speaker)
		}
	}
}

// Close is called by disgo when the voice connection closes (/transcribe stop,
// or the bot being disconnected): everyone's last sentence is finished and all
// streams freed.
func (t *Transcriber) Close() {
	// Once only: closing done a second time would crash.
	t.closeOnce.Do(func() {
		close(t.done) // stops the quiet timer

		t.mu.Lock()
		for ssrc, speaker := range t.speakers {
			t.dropSpeaker(ssrc, speaker)
		}
		clear(t.held) // never decoded, so nothing to finish
		t.closed = true
		t.mu.Unlock()

		// Everyone's last sentence has gone out, so the output can finish up
		// (the captions thread sends whatever's left). Outside the lock: that
		// may take a moment, and nothing here needs protecting any more.
		t.out.Closed()
	})
}

// dropSpeaker finishes this speaker's sentence, frees their stream and forgets
// them. Called while t.mu is already locked.
func (t *Transcriber) dropSpeaker(ssrc uint32, speaker *speakerStream) {
	// idle means it's already been finished and wiped: nothing left to say.
	if !speaker.idle {
		t.finishSentence(speaker)
	}
	// The stream lives in C memory, which Go's garbage collector can't free.
	sherpa.DeleteOnlineStream(speaker.stream)
	delete(t.speakers, ssrc)
}
