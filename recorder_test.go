package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/go-audio/wav"
	"gopkg.in/hraban/opus.v2"
)

// opusPackets makes n real Opus packets of a sine tone, 20 ms each, the same
// shape Discord sends (48 kHz stereo), so the recorder decodes genuine audio.
func opusPackets(t *testing.T, n int) [][]byte {
	t.Helper()
	encoder, err := opus.NewEncoder(48000, 2, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}

	packets := make([][]byte, 0, n)
	pcm := make([]int16, 960*2)
	buf := make([]byte, 4000)
	for frame := range n {
		for i := range 960 {
			sampleIndex := frame*960 + i
			value := int16(10000 * math.Sin(2*math.Pi*440*float64(sampleIndex)/48000))
			pcm[i*2], pcm[i*2+1] = value, value // same on left and right
		}
		size, err := encoder.Encode(pcm, buf)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:size]...))
	}
	return packets
}

// say feeds packets from one speaker, as disgo's receive loop would.
func say(t *testing.T, recorder *wavRecorder, userID snowflake.ID, packets [][]byte) {
	t.Helper()
	for _, data := range packets {
		if err := recorder.ReceiveOpusFrame(userID, &voice.Packet{Opus: data}); err != nil {
			t.Fatalf("ReceiveOpusFrame: %v", err)
		}
	}
}

// recordingFor finds the one WAV file for a user in the folder.
func recordingFor(t *testing.T, folder string, userID snowflake.ID) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(folder, userID.String()+"-*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one recording for %s, found %v", userID, matches)
	}
	return matches[0]
}

// readWAV checks the file is a playable 48 kHz 16-bit stereo WAV and returns
// its samples.
func readWAV(t *testing.T, path string) []int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	decoder := wav.NewDecoder(file)
	if !decoder.IsValidFile() {
		t.Fatalf("%s isn't a valid WAV file", path)
	}
	buffer, err := decoder.FullPCMBuffer()
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if decoder.SampleRate != 48000 || decoder.NumChans != 2 || decoder.BitDepth != 16 {
		t.Errorf("want 48000 Hz, 2 channels, 16-bit; got %d Hz, %d channels, %d-bit",
			decoder.SampleRate, decoder.NumChans, decoder.BitDepth)
	}
	return buffer.Data
}

func loudest(samples []int) int {
	peak := 0
	for _, s := range samples {
		peak = max(peak, s, -s)
	}
	return peak
}

func TestRecorderWritesAPlayableWAVPerSpeaker(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "recordings")
	recorder := newWavRecorder(folder)

	say(t, recorder, 1, opusPackets(t, 50)) // 1 second
	say(t, recorder, 2, opusPackets(t, 25)) // half a second
	recorder.Close()

	for _, test := range []struct {
		userID  snowflake.ID
		packets int
	}{{1, 50}, {2, 25}} {
		samples := readWAV(t, recordingFor(t, folder, test.userID))

		// every 20 ms packet decodes to 960 samples for each of 2 channels
		if want := test.packets * 960 * 2; len(samples) != want {
			t.Errorf("user %d: got %d samples, want %d (%d packets)", test.userID, len(samples), want, test.packets)
		}
		// the tone is at about a third of full volume; silence would be ~0
		if peak := loudest(samples); peak < 3000 {
			t.Errorf("user %d: recording looks silent (loudest sample %d)", test.userID, peak)
		}
	}
}

func TestRecorderSkipsUnknownSpeakersAndCreatesNothing(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "recordings")
	recorder := newWavRecorder(folder)

	say(t, recorder, 0, opusPackets(t, 10)) // user 0: Discord hasn't said who it is yet
	recorder.Close()

	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatalf("nothing should be created for unknown speakers or before anyone talks (stat: %v)", err)
	}
}

func TestRecorderCleanupUserFinishesOnlyThatSpeaker(t *testing.T) {
	folder := t.TempDir()
	recorder := newWavRecorder(folder)
	say(t, recorder, 1, opusPackets(t, 10))
	say(t, recorder, 2, opusPackets(t, 10))

	recorder.CleanupUser(1)

	// user 1's file is finished and playable straight away
	if samples := readWAV(t, recordingFor(t, folder, 1)); len(samples) != 10*960*2 {
		t.Errorf("user 1's finished file has %d samples, want %d", len(samples), 10*960*2)
	}
	if _, still := recorder.speakers[1]; still {
		t.Error("user 1 should be forgotten after leaving")
	}
	if _, still := recorder.speakers[2]; !still {
		t.Error("user 2 is still in the call and must keep recording")
	}

	say(t, recorder, 2, opusPackets(t, 5)) // user 2 carries on into the same file
	recorder.Close()
	if samples := readWAV(t, recordingFor(t, folder, 2)); len(samples) != 15*960*2 {
		t.Errorf("user 2's file has %d samples, want %d", len(samples), 15*960*2)
	}
}

func TestRecorderCleanupUserWhoNeverSpoke(t *testing.T) {
	recorder := newWavRecorder(t.TempDir())
	recorder.CleanupUser(7) // joined and left without talking: must not panic
}

func TestRecorderCloseTwiceIsHarmless(t *testing.T) {
	logs := captureLog(t)
	recorder := newWavRecorder(t.TempDir())
	say(t, recorder, 1, opusPackets(t, 5))

	recorder.Close()
	recorder.Close()

	if strings.Contains(logs.String(), "couldn't finish") {
		t.Errorf("a second Close shouldn't try to finish files again, logged:\n%s", logs)
	}
	if strings.Count(logs.String(), "recordings saved") != 2 {
		t.Errorf("each Close should log once, logged:\n%s", logs)
	}
}

func TestRecorderEmptyPacketIsAnError(t *testing.T) {
	recorder := newWavRecorder(t.TempDir())
	defer recorder.Close()

	if err := recorder.ReceiveOpusFrame(1, &voice.Packet{Opus: []byte{}}); err == nil {
		t.Fatal("an empty packet can't be decoded and should return an error for disgo to log")
	}
}

func TestRecorderFolderCantBeCreated(t *testing.T) {
	// a regular file where the folder's parent should be, so MkdirAll fails
	blocker := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := newWavRecorder(filepath.Join(blocker, "recordings"))

	err := recorder.ReceiveOpusFrame(1, &voice.Packet{Opus: opusPackets(t, 1)[0]})
	if err == nil {
		t.Fatal("expected an error when the recordings folder can't be made")
	}
	if len(recorder.speakers) != 0 {
		t.Error("a failed start shouldn't be remembered, so the next packet tries again")
	}
}

func TestRecorderFileCantBeCreated(t *testing.T) {
	folder := t.TempDir()
	if err := os.Chmod(folder, 0o555); err != nil { // folder exists but is read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o755) }) // so the temp dir can be removed

	recorder := newWavRecorder(folder)
	if err := recorder.ReceiveOpusFrame(1, &voice.Packet{Opus: opusPackets(t, 1)[0]}); err == nil {
		t.Fatal("expected an error when the WAV file can't be created")
	}
}

func TestRecorderLogsWhenAFileCantBeFinished(t *testing.T) {
	logs := captureLog(t)
	recorder := newWavRecorder(t.TempDir())
	say(t, recorder, 1, opusPackets(t, 2))
	say(t, recorder, 2, opusPackets(t, 2))

	// break both files underneath the recorder, so finishing them fails
	_ = recorder.speakers[1].file.Close()
	_ = recorder.speakers[2].file.Close()

	recorder.CleanupUser(1)
	if !strings.Contains(logs.String(), "couldn't finish the recording for 1") {
		t.Errorf("CleanupUser should log the failure, logged:\n%s", logs)
	}
	if _, still := recorder.speakers[1]; still {
		t.Error("user 1 should still be forgotten even though finishing failed")
	}

	recorder.Close()
	if !strings.Contains(logs.String(), "couldn't finish the recording for 2") {
		t.Errorf("Close should log the failure, logged:\n%s", logs)
	}
}

// disgo calls ReceiveOpusFrame from its read loop and CleanupUser from the voice
// gateway at the same time. Run with -race: without the mutex this fails.
func TestRecorderConcurrentUse(t *testing.T) {
	recorder := newWavRecorder(t.TempDir())
	packets := opusPackets(t, 5)

	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, data := range packets {
				_ = recorder.ReceiveOpusFrame(snowflake.ID(i%3+1), &voice.Packet{Opus: data})
			}
		}()
		go func() {
			defer wg.Done()
			recorder.CleanupUser(snowflake.ID(i%3 + 1))
		}()
	}
	wg.Wait()
	recorder.Close()
}
