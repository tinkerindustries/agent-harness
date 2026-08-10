package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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
	got := e.timeoutFor(t.Context(), "Bash", mustJSON(t, bashArgs{TimeoutMS: 50}))
	if got != 50*time.Millisecond {
		t.Fatalf("expected the requested 50ms timeout, got %s", got)
	}
}

func TestExecutorTimeoutForCapsBashTimeoutAtMax(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.Timeouts.BashMax = time.Second
	got := e.timeoutFor(t.Context(), "Bash", mustJSON(t, bashArgs{TimeoutMS: 1000 * 60 * 60}))
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

// The cut lands mid-rune for two of every three offsets into a run of
// three-byte characters, and the result travels to the API through
// encoding/json, which silently substitutes U+FFFD for invalid UTF-8 rather
// than refusing to marshal it. Every cap in the package shares this
// function, so a Read of a UTF-8 source file is as exposed as a Bash dump.
func TestTruncateNeverSplitsARune(t *testing.T) {
	s := strings.Repeat("日", 40) // 120 bytes, no byte boundary shared with a rune boundary
	for n := 1; n < len(s); n++ {
		out, cut := truncate(s, n)
		if !cut {
			t.Fatalf("n=%d: expected a cut below the input length", n)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("n=%d: truncate produced invalid UTF-8: %q", n, out)
		}
	}
}

// A U+FFFD the caller meant to send decodes with size 3, so the rune-boundary
// backoff must not mistake it for the invalid encoding it uses as its signal
// and eat the tail one byte at a time.
func TestTruncateKeepsALiteralReplacementChar(t *testing.T) {
	s := "ab�" + strings.Repeat("c", 20)
	out, cut := truncate(s, 5)
	if !cut {
		t.Fatal("expected a cut")
	}
	if !strings.HasPrefix(out, "ab�") {
		t.Fatalf("expected the literal U+FFFD to survive, got %q", out)
	}
}

// The label reports what survived the rune-boundary backoff, not the
// requested cap, so the two numbers in it stay a true account of the cut.
func TestTruncateLabelReportsBytesKept(t *testing.T) {
	s := strings.Repeat("日", 10) // 30 bytes
	out, _ := truncate(s, 8)     // backs off to 6
	if !strings.Contains(out, "[truncated: 6 of 30 bytes shown]") {
		t.Fatalf("expected the label to name the kept byte count, got %q", out)
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
