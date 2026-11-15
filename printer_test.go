package main

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
	out := terminalOutput{nameFor: func(snowflake.ID) string { return "Sam" }}

	out.caption(42, "so I rang", false)
	out.caption(42, "So I rang him.", true)
	out.notCaptioned(43)
	out.closed()

	if strings.Contains(logs.String(), "so I rang\n") {
		t.Errorf("captions still being spoken shouldn't be logged, logged:\n%s", logs)
	}
	for _, want := range []string{"Sam: So I rang him.\n", "Sam isn't being captioned (no-captions role)\n"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, got:\n%s", want, logs)
		}
	}
}

func TestMultiOutputSendsEverythingToEachOutput(t *testing.T) {
	first, second := &captionLog{}, &captionLog{}
	out := multiOutput{first, second}

	out.caption(42, "so I rang", false)
	out.notCaptioned(43)
	out.closed()

	for i, log := range []*captionLog{first, second} {
		if lines := log.all(); len(lines) != 1 || lines[0] != (caption{42, "so I rang", false}) {
			t.Errorf("output %d: want the caption, got %v", i, lines)
		}
		if log.skipped(43) != 1 || log.timesClosed() != 1 {
			t.Errorf("output %d: want the opt-out report and closed passed on", i)
		}
	}
}
