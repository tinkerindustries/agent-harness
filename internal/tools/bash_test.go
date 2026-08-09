package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBashRunsAndCapturesOutput(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo hello"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Fatalf("expected output to contain hello, got: %q", res.Content)
	}
}

func TestBashReportsNonZeroExit(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "exit 3"}))
	if !res.IsError {
		t.Fatal("expected a non-zero exit to be reported as an error")
	}
	if !strings.Contains(res.Content, "exit code 3") {
		t.Fatalf("expected the exit code in the result, got: %s", res.Content)
	}
}

// TestBashTimeoutFires exercises the timeout the way Executor.Execute
// applies it: as a context deadline wrapping the tool function, derived
// from Executor.timeoutFor.
func TestBashTimeoutFires(t *testing.T) {
	e, _ := newTestExecutor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := execBash(ctx, e, mustJSON(t, bashArgs{Command: "sleep 5"}))
	elapsed := time.Since(start)

	if !res.IsError {
		t.Fatal("expected the timed-out command to be reported as an error")
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Fatalf("expected a timeout label in the result, got: %s", res.Content)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("expected the command to be killed near the deadline, took %s", elapsed)
	}
}

func TestExecutorTimeoutForHonoursRequestedBashTimeout(t *testing.T) {
	e, _ := newTestExecutor(t)
	got := e.timeoutFor("Bash", mustJSON(t, bashArgs{TimeoutMS: 50}))
	if got != 50*time.Millisecond {
		t.Fatalf("expected the requested 50ms timeout, got %s", got)
	}
}

func TestExecutorTimeoutForCapsBashTimeoutAtMax(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.Timeouts.BashMax = time.Second
	got := e.timeoutFor("Bash", mustJSON(t, bashArgs{TimeoutMS: 1000 * 60 * 60}))
	if got != time.Second {
		t.Fatalf("expected the timeout capped at BashMax (1s), got %s", got)
	}
}

func TestOutputCapTruncatesAndLabels(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.OutputCap = 100
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "yes x | head -c 5000"}))
	if !res.Truncated {
		t.Fatal("expected Truncated to be set")
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("expected a truncation label in the content, got a %d-byte result", len(res.Content))
	}
	if len(res.Content) > 100+64 {
		t.Fatalf("expected output near the cap, got %d bytes", len(res.Content))
	}
}

// TestLiveStdoutWriterCoalescesByTime exercises the writer directly rather
// than through a real command, so the coalescing boundary can be forced
// deterministically instead of racing a wall-clock interval.
func TestLiveStdoutWriterCoalescesByTime(t *testing.T) {
	var chunks []string
	w := &liveStdoutWriter{sink: func(s string) { chunks = append(chunks, s) }}

	// A fresh writer's lastSent is the zero Time, so the very first write is
	// already past the interval and flushes immediately.
	w.Write([]byte("a"))
	if len(chunks) != 1 || chunks[0] != "a" {
		t.Fatalf("expected the first write to flush immediately, got %v", chunks)
	}

	w.Write([]byte("b"))
	if len(chunks) != 1 {
		t.Fatalf("expected \"b\" to buffer rather than flush within the interval, got %v", chunks)
	}

	w.lastSent = time.Time{} // simulate the interval having elapsed
	w.Write([]byte("c"))
	if len(chunks) != 2 || chunks[1] != "bc" {
		t.Fatalf("expected the buffered \"b\" and new \"c\" to flush together, got %v", chunks)
	}

	w.Write([]byte("d"))
	w.flush()
	if len(chunks) != 3 || chunks[2] != "d" {
		t.Fatalf("expected flush to forward the remainder \"d\", got %v", chunks)
	}
}

func TestLiveStdoutWriterNilSinkIsNoop(t *testing.T) {
	w := &liveStdoutWriter{}
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("expected a nil sink to still report a normal write, got n=%d err=%v", n, err)
	}
	w.flush() // must not panic
}

func TestBashForwardsLiveOutputThroughContextSink(t *testing.T) {
	e, _ := newTestExecutor(t)
	var mu sync.Mutex
	var chunks []string
	ctx := WithStdoutSink(t.Context(), func(s string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, s)
	})

	res := execBash(ctx, e, mustJSON(t, bashArgs{Command: "echo streamed"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) == 0 {
		t.Fatal("expected the sink to receive at least the final flush of the command's output")
	}
	if !strings.Contains(strings.Join(chunks, ""), "streamed") {
		t.Fatalf("expected forwarded output to contain the command's output, got %q", chunks)
	}
}

func TestBashWithoutStdoutSinkStillCapturesOutput(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo unwatched"}))
	if res.IsError || !strings.Contains(res.Content, "unwatched") {
		t.Fatalf("expected normal output capture with no sink attached, got: %+v", res)
	}
}
