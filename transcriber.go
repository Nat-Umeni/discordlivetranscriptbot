package main

import (
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	"gopkg.in/hraban/opus.v2"
)

const (
	quietLimit      = 1200 * time.Millisecond
	quietCheckEvery = 100 * time.Millisecond
	tailPadding     = 12800
)

// captionFunc is what the transcriber does with text: print it (printer.go),
// record it (tests) or, in milestone 2, post it in Discord.
type captionFunc func(userID snowflake.ID, text string, final bool)

type speakerStream struct {
	userID    snowflake.ID
	decoder   *opus.Decoder
	stream    *sherpa.OnlineStream
	lastText  string
	lastHeard time.Time
	idle      bool
}

type transcriber struct {
	mu         sync.Mutex
	recogniser *sherpa.OnlineRecognizer
	speakers   map[uint32]*speakerStream
	onText     captionFunc
	now        func() time.Time
	done       chan struct{}
	closeOnce  sync.Once
	closed     bool
}

// Compile-time check that transcriber has all three methods disgo needs, so
// it can be passed to SetOpusFrameReceiver.
var _ voice.OpusFrameReceiver = (*transcriber)(nil)

func newTranscriber(recogniser *sherpa.OnlineRecognizer, onText captionFunc) *transcriber {
	t := &transcriber{
		recogniser: recogniser,
		onText:     onText,
		speakers:   make(map[uint32]*speakerStream),
		now:        time.Now,
		done:       make(chan struct{}),
	}

	// Runs alongside everything else until Close, ending the sentences of
	// anyone who's gone quiet.
	go t.runQuietTimer()

	return t
}

// runQuietTimer checks for quiet speakers every quietCheckEvery. Discord sends
// no packets while someone is silent, so without this their last sentence
// would sit unfinished until they next spoke, maybe minutes later.
func (t *transcriber) runQuietTimer() {
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
func (t *transcriber) flushQuiet() {
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
func (t *transcriber) finishSentence(speaker *speakerStream) {
	// The model works in 560 ms chunks and waits for a full one, so the last
	// half-second of words would never be processed. 0.8 s of silence pushes
	// them through.
	speaker.stream.AcceptWaveform(16000, make([]float32, tailPadding))
	for t.recogniser.IsReady(speaker.stream) {
		t.recogniser.Decode(speaker.stream)
	}

	text := t.recogniser.GetResult(speaker.stream).Text
	if text != "" {
		t.onText(speaker.userID, text, true)
	}

	// Start a fresh sentence for when they next talk.
	t.recogniser.Reset(speaker.stream)
	speaker.lastText = ""
	speaker.idle = true
}

// report asks the model what this speaker has said so far and passes on any
// new text. Called while t.mu is already locked, so it mustn't lock again.
func (t *transcriber) report(speaker *speakerStream) {
	// Catch up: run the model on every full chunk of audio that's waiting.
	for t.recogniser.IsReady(speaker.stream) {
		t.recogniser.Decode(speaker.stream)
	}

	// The whole sentence so far, e.g. "so I rang" then "so I rang him".
	text := t.recogniser.GetResult(speaker.stream).Text

	// Only pass it on if there's something to say and it's not a repeat.
	if text != "" && text != speaker.lastText {
		t.onText(speaker.userID, text, false)
		speaker.lastText = text
	}

	// The model thinks the sentence has ended.
	if t.recogniser.IsEndpoint(speaker.stream) {
		if text != "" {
			t.onText(speaker.userID, text, true)
		}
		// Start a fresh sentence: clear the model's text and ours.
		t.recogniser.Reset(speaker.stream)
		speaker.lastText = ""
	}
}

// ReceiveOpusFrame is called by disgo for every 20 ms packet of someone
// talking. It feeds the packet into that speaker's stream and reports any new
// text.
func (t *transcriber) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// After Close the streams are freed, so late packets are ignored.
	if t.closed {
		return nil
	}

	// Find this stream's speaker, keyed by stream number (SSRC) so their
	// first words count even before Discord says who they are.
	speaker, found := t.speakers[packet.SSRC]
	if !found {
		// Discord sends 48 kHz stereo; this makes libopus hand back 16 kHz
		// mono, which is what the model wants.
		decoder, err := opus.NewDecoder(16000, 1)
		if err != nil {
			return err
		}
		speaker = &speakerStream{
			decoder: decoder,
			stream:  sherpa.NewOnlineStream(t.recogniser),
		}
		t.speakers[packet.SSRC] = speaker
	}

	// 0 means Discord hasn't said who this stream belongs to yet. Fill it in
	// as soon as it has.
	if userID != 0 {
		speaker.userID = userID
	}

	// 1920 samples = 120 ms, the longest packet Opus allows. Discord's 20 ms
	// packets use 320 of them; n says how many were filled.
	pcm := make([]float32, 1920)
	n, err := speaker.decoder.DecodeFloat32(packet.Opus, pcm)
	if err != nil {
		return err // disgo logs it
	}

	speaker.stream.AcceptWaveform(16000, pcm[:n])
	speaker.lastHeard = t.now()
	speaker.idle = false

	t.report(speaker)
	return nil
}

// CleanupUser is called by disgo when someone leaves the voice channel: their
// last sentence is finished and their stream freed.
func (t *transcriber) CleanupUser(userID snowflake.ID) {
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
func (t *transcriber) Close() {
	// Once only: closing done a second time would crash.
	t.closeOnce.Do(func() {
		close(t.done) // stops the quiet timer

		t.mu.Lock()
		defer t.mu.Unlock()
		for ssrc, speaker := range t.speakers {
			t.dropSpeaker(ssrc, speaker)
		}
		t.closed = true
	})
}

// dropSpeaker finishes this speaker's sentence, frees their stream and forgets
// them. Called while t.mu is already locked.
func (t *transcriber) dropSpeaker(ssrc uint32, speaker *speakerStream) {
	// idle means it's already been finished and wiped: nothing left to say.
	if !speaker.idle {
		t.finishSentence(speaker)
	}
	// The stream lives in C memory, which Go's garbage collector can't free.
	sherpa.DeleteOnlineStream(speaker.stream)
	delete(t.speakers, ssrc)
}
