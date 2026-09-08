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

// Sessions write bash whatever the shell is. Under busybox ash these come
// back as "syntax error: bad substitution" and the command never runs, so the
// tool resolves bash when the machine has one.
func TestBashRunsBashSyntax(t *testing.T) {
	if !strings.HasSuffix(shellPath(), "bash") {
		t.Skipf("no bash on this machine; the shell resolved to %s", shellPath())
	}
	e, _ := newTestExecutor(t)
	for _, tc := range []struct{ name, command, want string }{
		{"pipestatus", "true | false; echo ${PIPESTATUS[0]}", "0"},
		{"double bracket", `[[ ab == a* ]] && echo matched`, "matched"},
		{"array", "xs=(a b c); echo ${#xs[@]}", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: tc.command}))
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Errorf("expected %q in the output, got: %q", tc.want, res.Content)
			}
		})
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

// TestBashBackgroundedProcessDoesNotHang reproduces the production wedge
// (sess-23f440713ef783c1484eb3eb0be24969): a command that backgrounds a child
// holding stdout — `sleep 300 & echo started`, inheriting the captured pipe.
// WaitDelay must make the call return within the wait delay plus slack — not
// at the tool timeout, and not never — and the result must name the cause.
func TestBashBackgroundedProcessDoesNotHang(t *testing.T) {
	e, _ := newTestExecutor(t)
	const waitDelay = 300 * time.Millisecond
	e.Timeouts.BashWaitDelay = waitDelay

	start := time.Now()
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 300 & echo started"}))
	elapsed := time.Since(start)

	if !res.IsError {
		t.Fatalf("expected the wedged call to be reported as an error, got: %+v", res)
	}
	if !strings.Contains(res.Content, "left a process holding its output open") {
		t.Fatalf("expected the result to name the cause, got: %s", res.Content)
	}
	// What the command printed before it wedged survives, the way it does on
	// the timeout path: the explanation replaces nothing.
	if !strings.Contains(res.Content, "started") {
		t.Fatalf("expected the output written before the wedge to be kept, got: %s", res.Content)
	}
	if elapsed < waitDelay {
		t.Fatalf("expected the call to wait out the wait delay, took %s (< %s)", elapsed, waitDelay)
	}
	if elapsed > waitDelay+5*time.Second {
		t.Fatalf("expected the call to return within the wait delay plus slack, took %s", elapsed)
	}
}

// TestBashOrdinaryCommandUnchanged pins that WaitDelay does not alter the
// ordinary path: exit codes, truncation, and streamed stdout behave exactly as
// before even with a wait delay short enough to fire if a command dawdled.
func TestBashOrdinaryCommandUnchanged(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.Timeouts.BashWaitDelay = 200 * time.Millisecond

	// Exit codes are still reported.
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "exit 3"}))
	if !res.IsError || !strings.Contains(res.Content, "exit code 3") {
		t.Fatalf("expected the exit code in the result, got: %s", res.Content)
	}

	// Truncation is still labelled.
	e.OutputCap = 100
	res = execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "yes x | head -c 5000"}))
	if !res.Truncated || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("expected labelled truncation, got: %s", res.Content)
	}

	// Streamed stdout still reaches the sink.
	e.OutputCap = 0
	var mu sync.Mutex
	var chunks []string
	ctx := WithStdoutSink(t.Context(), func(s string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, s)
	})
	res = execBash(ctx, e, mustJSON(t, bashArgs{Command: "echo streamed"}))
	if res.IsError || !strings.Contains(res.Content, "streamed") {
		t.Fatalf("expected normal output capture, got: %+v", res)
	}
	mu.Lock()
	joined := strings.Join(chunks, "")
	mu.Unlock()
	if !strings.Contains(joined, "streamed") {
		t.Fatalf("expected streamed output to reach the sink, got %q", chunks)
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

// TestBashExtraEnvReachesTheCommand pins the hook the GitHub App
// credential rides on: a variable ExtraEnv returns is in the command's
// environment, and the process's own environment is still there underneath
// it (a Bash call inherits GIT_CONFIG_* and everything else
// internal/githubauth set).
func TestBashExtraEnvReachesTheCommand(t *testing.T) {
	e, _ := newTestExecutor(t)
	t.Setenv("HARNESS_TEST_AMBIENT", "ambient")
	e.ExtraEnv = func(ctx context.Context, workspace string) []string {
		return []string{"GH_TOKEN=ghs_from_extra_env"}
	}

	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo $GH_TOKEN $HARNESS_TEST_AMBIENT"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "ghs_from_extra_env") {
		t.Errorf("output = %q, want the ExtraEnv value", res.Content)
	}
	if !strings.Contains(res.Content, "ambient") {
		t.Errorf("output = %q, want the process's own environment as well", res.Content)
	}
}

// TestBashEnvFilterStripsAVariableBeforeInheritance pins the hosted-mode
// credential boundary: a hosted Gemini session must not let its Bash calls
// see GEMINI_API_KEY, which this process itself was handed only so its own
// API client could reach Google (docs/STDIO-PROTOCOL.md, "Trust
// boundaries"). EnvFilter runs even with no ExtraEnv configured, unlike a
// nil-Env inherit which would leak the whole environment unfiltered.
func TestBashEnvFilterStripsAVariableBeforeInheritance(t *testing.T) {
	e, _ := newTestExecutor(t)
	t.Setenv("GEMINI_API_KEY", "leaked-key")
	t.Setenv("HARNESS_TEST_AMBIENT", "ambient")
	e.EnvFilter = func(base []string) []string {
		out := make([]string, 0, len(base))
		for _, kv := range base {
			if strings.HasPrefix(kv, "GEMINI_API_KEY=") {
				continue
			}
			out = append(out, kv)
		}
		return out
	}

	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo [$GEMINI_API_KEY] $HARNESS_TEST_AMBIENT"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "leaked-key") {
		t.Errorf("output = %q, the filtered key reached the command", res.Content)
	}
	if !strings.Contains(res.Content, "[]") {
		t.Errorf("output = %q, want GEMINI_API_KEY to expand empty", res.Content)
	}
	if !strings.Contains(res.Content, "ambient") {
		t.Errorf("output = %q, want the rest of the environment still inherited", res.Content)
	}
}

// TestBashEnvFilterComposesWithExtraEnv pins that the two hooks stack:
// EnvFilter narrows the base environment and ExtraEnv still adds to what is
// left, the same order a hosted session's Bash call and a GitHub App token
// would combine in if both were ever configured together.
func TestBashEnvFilterComposesWithExtraEnv(t *testing.T) {
	e, _ := newTestExecutor(t)
	t.Setenv("GEMINI_API_KEY", "leaked-key")
	e.EnvFilter = func(base []string) []string {
		out := make([]string, 0, len(base))
		for _, kv := range base {
			if !strings.HasPrefix(kv, "GEMINI_API_KEY=") {
				out = append(out, kv)
			}
		}
		return out
	}
	e.ExtraEnv = func(ctx context.Context, workspace string) []string {
		return []string{"GH_TOKEN=ghs_from_extra_env"}
	}

	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo [$GEMINI_API_KEY] $GH_TOKEN"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "leaked-key") {
		t.Errorf("output = %q, the filtered key reached the command", res.Content)
	}
	if !strings.Contains(res.Content, "ghs_from_extra_env") {
		t.Errorf("output = %q, want the ExtraEnv value on top of the filtered base", res.Content)
	}
}

// TestBashExtraEnvIsGivenTheWorkspace pins that the closure is told which
// session it is resolving for: the token it returns is chosen from the
// clones in that workspace (cmd/harness/github.go).
func TestBashExtraEnvIsGivenTheWorkspace(t *testing.T) {
	e, dir := newTestExecutor(t)
	var got string
	e.ExtraEnv = func(ctx context.Context, workspace string) []string {
		got = workspace
		return nil
	}

	execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "true"}))
	if got != e.Workspace {
		t.Errorf("ExtraEnv was given %q, want the executor's workspace %q (from %q)", got, e.Workspace, dir)
	}
}
