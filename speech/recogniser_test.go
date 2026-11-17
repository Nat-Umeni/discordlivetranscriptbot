package speech

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// The model isn't in git (~650 MB, gitignored), so these tests skip if it
// hasn't been downloaded. Download and unpack it into models/:
// https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-nemotron-speech-streaming-en-0.6b-560ms-int8-2026-04-25.tar.bz2
//
// Go runs these tests inside speech/, but models/ is at the project root (next
// to go.mod), so the path is built from there.
var testModelDir = filepath.Join(projectRoot(), DefaultModelDir)

// projectRoot is the folder holding go.mod, found by walking up from the
// folder the tests run in.
func projectRoot() string {
	dir, _ := os.Getwd()
	for dir != filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return dir
}

func needModel(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(testModelDir); err != nil {
		t.Skipf("speech model not downloaded (%s), skipping", testModelDir)
	}
}

// transcribe runs a whole WAV file through a fresh stream, the way the
// transcriber feeds live audio, and returns the recognised text.
func transcribe(t *testing.T, recogniser *sherpa.OnlineRecognizer, wavPath string) string {
	t.Helper()
	wave := sherpa.ReadWave(wavPath)
	if wave == nil {
		t.Fatalf("couldn't read %s", wavPath)
	}

	stream := sherpa.NewOnlineStream(recogniser)
	defer sherpa.DeleteOnlineStream(stream)

	stream.AcceptWaveform(wave.SampleRate, wave.Samples)

	// Half a second of silence after the speech. The model looks a little ahead
	// before committing to a word, so without this the last word can be cut off.
	stream.AcceptWaveform(wave.SampleRate, make([]float32, wave.SampleRate/2))
	stream.InputFinished()

	for recogniser.IsReady(stream) {
		recogniser.Decode(stream)
	}
	return recogniser.GetResult(stream).Text
}

func TestRecogniserTranscribesTheSampleRecordings(t *testing.T) {
	needModel(t)

	recogniser, err := NewRecogniser(testModelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer sherpa.DeleteOnlineRecognizer(recogniser)

	// Expected words from testdata/trans.txt. Checking key
	// words rather than the exact sentence leaves room for the odd small slip.
	tests := []struct {
		file string
		want []string
	}{
		{"0.wav", []string{"NIGHTFALL", "YELLOW LAMPS", "LIGHT UP"}},
		{"1.wav", []string{"DIRECT CONSEQUENCE", "LOVELY CHILD", "HEAVEN"}},
		// 8 kHz recording: proves sherpa resamples audio that isn't 16 kHz
		{"8k.wav", []string{"THESE THOUGHTS", "HOPE"}},
	}

	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			text := transcribe(t, recogniser, sampleWAV(test.file))
			t.Logf("heard: %s", text)

			for _, word := range test.want {
				if !strings.Contains(strings.ToUpper(text), word) {
					t.Errorf("expected %q in the transcript, got %q", word, text)
				}
			}

			// Nemotron writes normal sentence case. ALL CAPS would mean an
			// older model got loaded, and captions would need extra tidying.
			if text == strings.ToUpper(text) {
				t.Errorf("expected normal capitalisation, got all caps: %q", text)
			}
		})
	}
}

func TestRecogniserBadModelFolderIsAnError(t *testing.T) {
	recogniser, err := NewRecogniser(filepath.Join(t.TempDir(), "no-model-here"))
	if err == nil {
		sherpa.DeleteOnlineRecognizer(recogniser)
		t.Fatal("a missing model should return an error, not a recogniser")
	}
	if recogniser != nil {
		t.Error("should return a nil recogniser alongside the error")
	}
}
