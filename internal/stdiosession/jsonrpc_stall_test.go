package stdiosession

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// slowWriter stands in for a stdout pipe whose buffer is full because the
// parent stopped reading: every write blocks for delay before it lands.
type slowWriter struct{ delay time.Duration }

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

// A frame that sits on a blocked stdout is logged, so a parent that showed
// text late can tell the hold was its own and not the provider's. A frame
// that goes straight through logs nothing. Fails if the blocked write goes
// unlogged or a fast one is logged.
func TestWriteLogsAFrameTheParentDidNotRead(t *testing.T) {
	var lines []string
	prevLogf, prevThreshold := logf, writeStallThreshold
	logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	writeStallThreshold = 20 * time.Millisecond
	t.Cleanup(func() { logf, writeStallThreshold = prevLogf, prevThreshold })

	if err := NewConn(strings.NewReader(""), slowWriter{}, nil, nil).Notify("response.output_text.delta", map[string]string{"delta": "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("logged %q for a write that did not block", lines)
	}

	if err := NewConn(strings.NewReader(""), slowWriter{delay: 50 * time.Millisecond}, nil, nil).Notify("response.output_text.delta", map[string]string{"delta": "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "the parent was not reading") {
		t.Fatalf("got %q, want one line saying the parent was not reading", lines)
	}
}
