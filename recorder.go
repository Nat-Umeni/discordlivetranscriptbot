package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"gopkg.in/hraban/opus.v2"
)

// Fails to build if wavRecorder is missing any method disgo needs from a receiver.
var _ voice.OpusFrameReceiver = (*wavRecorder)(nil)

type speakerRecording struct {
	decoder *opus.Decoder
	file    *os.File
	wav     *wav.Encoder
}

type wavRecorder struct {
	mu       sync.Mutex
	folder   string
	speakers map[snowflake.ID]*speakerRecording
}

func newWavRecorder(folder string) *wavRecorder {
	return &wavRecorder{
		speakers: make(map[snowflake.ID]*speakerRecording),
		folder:   folder,
	}
}

func (wr *wavRecorder) startRecording(userID snowflake.ID) (*speakerRecording, error) {
	if err := os.MkdirAll(wr.folder, 0o755); err != nil {
		return nil, fmt.Errorf("Failed to create folder with correct perms %v", err)
	}

	// named path, not filepath: a variable called filepath would hide the
	// filepath package for the rest of this function
	path := filepath.Join(wr.folder, fmt.Sprintf("%s-%d.wav", userID, time.Now().Unix()))

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("Failed to create wav file with correct perms %v", err)
	}

	decoder, err := opus.NewDecoder(48000, 2)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("Failed to create decoder for the wav files %v", err)
	}

	// 48 kHz, 16-bit, 2 channels, and format 1 = plain uncompressed audio.
	encoder := wav.NewEncoder(file, 48000, 16, 2, 1)
	log.Printf("Created wav file at %s for user %s", path, userID)

	return &speakerRecording{
		decoder: decoder,
		file:    file,
		wav:     encoder,
	}, nil
}

func (sr speakerRecording) finish() error {
	// Close the encoder first: it goes back and writes the real length into the
	// WAV header. Without it the file won't play.
	wavErr := sr.wav.Close()

	// Always close the file too, even if the header write failed, so it isn't
	// left open. (The encoder's Close doesn't close the file itself.)
	fileErr := sr.file.Close()

	// errors.Join returns nil if both are nil, otherwise an error holding both
	return errors.Join(wavErr, fileErr)
}

func (wr *wavRecorder) ReceiveOpusFrame(userID snowflake.ID, packet *voice.Packet) error {
	if userID == 0 {
		// no name for the file just yet
		return nil
	}

	wr.mu.Lock()
	defer wr.mu.Unlock()

	// If we're not already recording for this user, then start a new one for them
	recording := wr.speakers[userID]
	if recording == nil {
		var err error
		recording, err = wr.startRecording(userID)
		if err != nil {
			return err
		}

		wr.speakers[userID] = recording
	}

	// decode the packets to get proper audio
	// Make room for 20 ms of audio: 960 samples per channel × 2 channels.
	pcm := make([]int16, 960*2)
	samplesPerChannel, err := recording.decoder.Decode(packet.Opus, pcm)
	if err != nil {
		return err
	}

	// convert so go-audio can read
	samples := pcm[:samplesPerChannel*2]
	data := make([]int, len(samples))
	for i, sample := range samples {
		data[i] = int(sample)
	}

	return recording.wav.Write(&audio.IntBuffer{Format: &audio.Format{NumChannels: 2, SampleRate: 48000}, Data: data,
		SourceBitDepth: 16})
}

// CleanupUser runs when someone leaves the voice channel: finish their file so it
// plays, and forget them. If they rejoin, ReceiveOpusFrame starts a new file.
func (wr *wavRecorder) CleanupUser(userID snowflake.ID) {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	recording := wr.speakers[userID]
	if recording == nil {
		// they left without saying anything, so there's no file to finish
		return
	}

	if err := recording.finish(); err != nil {
		log.Printf("couldn't finish the recording for %s: %v", userID, err)
	}
	delete(wr.speakers, userID)
}

// Close runs when the bot leaves voice (/transcribe stop, or being disconnected or
// moved): finish everyone's file. Without it every recording would be left without
// its real length in the header, and wouldn't play.
func (wr *wavRecorder) Close() {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	for userID, recording := range wr.speakers {
		if err := recording.finish(); err != nil {
			log.Printf("couldn't finish the recording for %s: %v", userID, err)
		}
	}

	// Forget them all, so finishing can't happen twice if Close is called again
	// (a second Close on an already-closed file would only log errors).
	clear(wr.speakers)

	log.Printf("recordings saved to %s", wr.folder)
}
