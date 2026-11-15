package speech

import (
	"testing"
)

func TestMultiOutputSendsEverythingToEachOutput(t *testing.T) {
	first, second := &captionLog{}, &captionLog{}
	out := MultiOutput{first, second}

	out.Caption(42, "so I rang", false)
	out.NotCaptioned(43)
	out.Closed()

	for i, recorded := range []*captionLog{first, second} {
		if lines := recorded.all(); len(lines) != 1 || lines[0] != (caption{42, "so I rang", false}) {
			t.Errorf("output %d: want the caption, got %v", i, lines)
		}
		if recorded.skipped(43) != 1 || recorded.timesClosed() != 1 {
			t.Errorf("output %d: want the opt-out report and closed passed on", i)
		}
	}
}
