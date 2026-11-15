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

func TestCaptionPrinter(t *testing.T) {
	logs := captureLog(t)
	print := captionPrinter(func(userID snowflake.ID) string { return "Sam" })

	print(42, "so I rang", false)
	print(42, "So I rang him.", true)

	for _, want := range []string{"Sam … so I rang\n", "Sam: So I rang him.\n"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("expected %q in the log, got:\n%s", want, logs)
		}
	}
}
