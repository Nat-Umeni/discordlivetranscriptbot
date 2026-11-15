package captions

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// captureLog sends log output to a buffer for one test, so the test can check
// what was printed. TestMain discards logs otherwise.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
}

func TestTerminalOutputLogsOnlyFinishedCaptions(t *testing.T) {
	logs := captureLog(t)
	out := TerminalOutput{NameFor: func(snowflake.ID) string { return "Sam" }}

	out.Caption(42, "so I rang", false)
	out.Caption(42, "So I rang him.", true)
	out.NotCaptioned(43)
	out.Closed()

	if strings.Contains(logs.String(), "so I rang\n") {
		t.Errorf("captions still being spoken shouldn't be logged, logged:\n%s", logs)
	}
	for _, want := range []string{"Sam: So I rang him.\n", "Sam isn't being captioned (no-captions role)\n"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, got:\n%s", want, logs)
		}
	}
}
