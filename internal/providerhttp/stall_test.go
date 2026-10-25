package providerhttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLogs swaps logf for the length of a test and returns what it was
// given. These lines are the only record of a stall or a retry, so a test
// that reads them is what proves a parent will see one.
func captureLogs(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	prev := logf
	logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = prev })
	return &lines
}

// A gap of StallThreshold between two lines is logged once the second
// arrives, naming the gap and how far into the stream it came; gaps under
// the threshold, which is every line of an ordinary stream, log nothing.
// Fails if the line is dropped, logged early, or logged for a short gap.
func TestGapLogLogsOnlyTheLongGap(t *testing.T) {
	lines := captureLogs(t)
	clock := time.Unix(0, 0)
	g := newGapLog("anthropic", func() time.Time { return clock })

	clock = clock.Add(2 * time.Second)
	g.Line(10)
	clock = clock.Add(StallThreshold - time.Millisecond)
	g.Line(10)
	if len(*lines) != 0 {
		t.Fatalf("logged %q for a gap under the threshold", *lines)
	}

	clock = clock.Add(35 * time.Second)
	g.Line(10)
	if len(*lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(*lines), *lines)
	}
	got := (*lines)[0]
	for _, want := range []string{"anthropic:", "quiet for 35.0s", "after 2 lines (20 bytes)"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not contain %q", got, want)
		}
	}
}

// Every retry says why, so a run that sat through several of them leaves
// a trace rather than a silence. Fails if a retried 503 goes unlogged.
func TestDoLogsEachRetry(t *testing.T) {
	lines := captureLogs(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newTestTransport(srv.URL).Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if len(*lines) != 2 {
		t.Fatalf("got %d lines, want one per retry (2): %q", len(*lines), *lines)
	}
	if !strings.Contains((*lines)[0], "attempt 1 failed (status 503") {
		t.Errorf("first line %q does not name the attempt and its status", (*lines)[0])
	}
}
