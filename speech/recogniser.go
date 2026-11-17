// Package speech turns people's voices into text: the speech model, and a
// transcriber per call that feeds each speaker's audio through it.
package speech

import (
	"fmt"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"livetranscribebot/speechmodel"
)

// DefaultModelDir is the speech model the bot uses: NVIDIA's Nemotron speech
// streaming 0.6b, 560 ms chunks, int8. On a casual recording it gave the best captions of
// five models tested, with punctuation and capitals built in, at ~4.9x real
// time on 4 threads. It runs entirely locally; nothing is sent anywhere.
// speechmodel downloads it there the first time the bot runs.
const DefaultModelDir = speechmodel.Dir

// NewRecogniser loads the speech model once. It's the heavy part (~650 MB on
// disk), so the bot shares one recogniser and gives each speaker their own
// stream.
func NewRecogniser(modelDir string) (*sherpa.OnlineRecognizer, error) {
	config := sherpa.OnlineRecognizerConfig{}

	// The audio format the model expects. Every sherpa-onnx model uses these.
	config.FeatConfig = sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80}

	// A streaming "transducer" model comes in three parts, each its own file.
	// The .int8 versions are compressed: smaller and faster, almost no accuracy loss.
	config.ModelConfig.Transducer.Encoder = filepath.Join(modelDir, "encoder.int8.onnx")
	config.ModelConfig.Transducer.Decoder = filepath.Join(modelDir, "decoder.int8.onnx")
	config.ModelConfig.Transducer.Joiner = filepath.Join(modelDir, "joiner.int8.onnx")

	// The model's dictionary of word pieces.
	config.ModelConfig.Tokens = filepath.Join(modelDir, "tokens.txt")

	// 4 was fastest when measured: 2 threads took 53.5 s for a 3-minute
	// recording, 4 took 37.2 s, and 6 slowed back down to 52.4 s (too many
	// threads fight each other and other programs for the CPU).
	config.ModelConfig.NumThreads = 4
	config.ModelConfig.Provider = "cpu"

	// The only method sherpa-onnx (v1.13.8) supports for streaming NeMo/Nemotron
	// models. It also means hotwords aren't available for this model: they need
	// "modified_beam_search", which only exists for NeMo's non-streaming mode.
	config.DecodingMethod = "greedy_search"

	// Sentence-end detection: a sentence ends after 1.2 s of silence following
	// speech (rule 2), after 2.4 s if nothing was recognised (rule 1), or after
	// a 20 s turn regardless (rule 3).
	config.EnableEndpoint = 1
	config.Rule1MinTrailingSilence = 2.4
	config.Rule2MinTrailingSilence = 1.2
	config.Rule3MinUtteranceLength = 20

	// NewOnlineRecognizer doesn't return an error: if loading fails it returns
	// nil and prints the reason itself. Without this check a wrong path would
	// only show up later as a confusing nil pointer crash.
	recogniser := sherpa.NewOnlineRecognizer(&config)
	if recogniser == nil {
		return nil, fmt.Errorf("couldn't load the speech model from %s", modelDir)
	}

	return recogniser, nil
}
