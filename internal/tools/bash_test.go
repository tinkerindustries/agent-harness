package tools

import (
	"context"
	"strings"
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
