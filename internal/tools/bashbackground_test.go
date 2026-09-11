package tools

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// pollBashOutput calls BashOutput with args repeatedly until until reports
// true against the result, or fails the test after a bound the slowest CI
// machine should still clear comfortably. Each call drains whatever is new
// since the one before it — the same way a real session's repeated polling
// would — so a caller that wants a filter applied to every read passes it
// on args rather than adding a second, unfiltered call afterward that would
// drain the output the filtered one was meant to see.
func pollBashOutput(t *testing.T, e *Executor, args bashOutputArgs, until func(Result) bool) Result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last Result
	for time.Now().Before(deadline) {
		last = execBashOutput(t.Context(), e, mustJSON(t, args))
		if until(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition never met for background shell %s: %s", args.BashID, last.Content)
	return last
}

// waitForBackgroundDone polls BashOutput until the shell reports anything
// other than "running".
func waitForBackgroundDone(t *testing.T, e *Executor, id string) Result {
	t.Helper()
	return pollBashOutput(t, e, bashOutputArgs{BashID: id}, func(r Result) bool {
		return !strings.HasPrefix(r.Content, "Status: running")
	})
}

func TestBashRunInBackgroundReturnsImmediately(t *testing.T) {
	e, _ := newTestExecutor(t)
	start := time.Now()
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 2", RunInBackground: true}))
	elapsed := time.Since(start)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Running in the background with id bash_1") {
		t.Fatalf("expected the minted id in the result, got: %s", res.Content)
	}
	if elapsed > time.Second {
		t.Fatalf("expected run_in_background to return immediately, took %s", elapsed)
	}
}

func TestBashOutputReturnsOnlyNewOutputSinceLastRead(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo first; sleep 0.2; echo second", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	first := pollBashOutput(t, e, bashOutputArgs{BashID: id}, func(r Result) bool {
		return strings.Contains(r.Content, "first")
	})
	if strings.Contains(first.Content, "second") {
		t.Fatalf("expected the first poll to not yet see \"second\", got: %s", first.Content)
	}

	second := waitForBackgroundDone(t, e, id)
	if strings.Contains(second.Content, "first") {
		t.Fatalf("expected the second poll to not repeat \"first\", got: %s", second.Content)
	}
	if !strings.Contains(second.Content, "second") {
		t.Fatalf("expected the second poll to see \"second\", got: %s", second.Content)
	}
	if !strings.Contains(second.Content, "completed (exit code 0)") {
		t.Fatalf("expected a completed status, got: %s", second.Content)
	}
}

func TestBashOutputWithNoNewOutputSaysSo(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo hi", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	waitForBackgroundDone(t, e, id)

	again := execBashOutput(t.Context(), e, mustJSON(t, bashOutputArgs{BashID: id}))
	if !strings.Contains(again.Content, "(no new output)") {
		t.Fatalf("expected no new output on a repeat poll, got: %s", again.Content)
	}
}

func TestBashOutputReportsNonZeroExitCode(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "exit 7", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	res := waitForBackgroundDone(t, e, id)
	if !strings.Contains(res.Content, "completed (exit code 7)") {
		t.Fatalf("expected exit code 7 reported, got: %s", res.Content)
	}
}

// TestBashOutputFilterKeepsOnlyMatchingLines spaces its three lines out with
// sleeps so more than one poll is likely needed, then accumulates every
// poll's content rather than trusting a single call to have caught
// everything — each poll only ever returns what is new since the one
// before it, filter included, so a test that only checked the last one
// could pass or fail on how the output happened to chunk.
func TestBashOutputFilterKeepsOnlyMatchingLines(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command:         "echo keep-me; sleep 0.05; echo drop-me; sleep 0.05; echo keep-me-too",
		RunInBackground: true,
	}))
	id := extractShellID(t, started.Content)

	var all strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := execBashOutput(t.Context(), e, mustJSON(t, bashOutputArgs{BashID: id, Filter: "keep"}))
		all.WriteString(res.Content)
		if !strings.HasPrefix(res.Content, "Status: running") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := all.String()
	if strings.Contains(got, "drop-me") {
		t.Fatalf("expected drop-me to be filtered out, got: %s", got)
	}
	if !strings.Contains(got, "keep-me") || !strings.Contains(got, "keep-me-too") {
		t.Fatalf("expected both keep-me lines to survive the filter, got: %s", got)
	}
}

func TestKillBashStopsARunningShell(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 30", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	killed := execKillBash(t.Context(), e, mustJSON(t, killBashArgs{ShellID: id}))
	if killed.IsError || !strings.Contains(killed.Content, "killed") {
		t.Fatalf("expected a killed confirmation, got: %+v", killed)
	}

	res := waitForBackgroundDone(t, e, id)
	if !strings.Contains(res.Content, "Status: killed") {
		t.Fatalf("expected BashOutput to report killed, got: %s", res.Content)
	}
}

func TestKillBashOnAnAlreadyFinishedShellIsANoOp(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "true", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	waitForBackgroundDone(t, e, id)

	killed := execKillBash(t.Context(), e, mustJSON(t, killBashArgs{ShellID: id}))
	if killed.IsError {
		t.Fatalf("expected killing an already-finished shell to be a no-op, not an error: %s", killed.Content)
	}
	if !strings.Contains(killed.Content, "already finished") {
		t.Fatalf("expected the result to say it had already finished, got: %s", killed.Content)
	}
}

func TestBashOutputAndKillBashUnknownIdAreErrors(t *testing.T) {
	e, _ := newTestExecutor(t)
	if res := execBashOutput(t.Context(), e, mustJSON(t, bashOutputArgs{BashID: "bash_999"})); !res.IsError {
		t.Fatal("expected an unknown bash_id to be an error")
	}
	if res := execKillBash(t.Context(), e, mustJSON(t, killBashArgs{ShellID: "bash_999"})); !res.IsError {
		t.Fatal("expected an unknown shell_id to be an error")
	}
}

// TestExecutorCloseKillsRunningBackgroundShells pins the "never leave an
// agent behind" rule applied to a process rather than a session: a run that
// ends while a background shell is still alive must not leave it running.
func TestExecutorCloseKillsRunningBackgroundShells(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 30", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	e.Close()

	res := waitForBackgroundDone(t, e, id)
	if strings.HasPrefix(res.Content, "Status: running") {
		t.Fatalf("expected Close to have ended the shell, got: %s", res.Content)
	}
}

// TestBashOutputConcurrentPollsDoNotPanicOrDuplicate drives many concurrent
// BashOutput calls against one fast-writing shell. buf.String() and
// readOffset must be read together under bg.mu — reading the buffer
// outside the lock lets two callers snapshot it at different lengths while
// sharing one readOffset, and whichever locks second can slice its own,
// shorter snapshot from an offset the first already advanced past it,
// which panics with a slice bounds error.
func TestBashOutputConcurrentPollsDoNotPanicOrDuplicate(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command:         "for i in $(seq 1 200); do echo line-$i; done",
		RunInBackground: true,
	}))
	id := extractShellID(t, started.Content)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var combined strings.Builder
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				res := execBashOutput(t.Context(), e, mustJSON(t, bashOutputArgs{BashID: id}))
				mu.Lock()
				combined.WriteString(res.Content)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// The 20x20 concurrent calls above race Start(); the shell may not have
	// written anything by the time they are all done. waitForBackgroundDone
	// keeps polling past that point, and its own returned Result is where
	// the real content shows up — a further call after it, on a shell
	// already fully drained, would see nothing and prove nothing.
	done := waitForBackgroundDone(t, e, id)
	mu.Lock()
	combined.WriteString(done.Content)
	mu.Unlock()

	for _, want := range []string{"line-1\n", "line-100\n", "line-200"} {
		if !strings.Contains(combined.String(), want) {
			t.Errorf("expected %q somewhere across the concurrent polls, got a %d-byte combined result", want, combined.Len())
		}
	}
}

// extractShellID pulls "bash_N" out of Bash's own run_in_background result
// text, so a test reads the id the same way a model has to: from the
// result, not from internal state.
func extractShellID(t *testing.T, content string) string {
	t.Helper()
	const marker = "with id "
	i := strings.Index(content, marker)
	if i < 0 {
		t.Fatalf("expected %q in the background-start result, got: %s", marker, content)
	}
	rest := content[i+len(marker):]
	end := strings.IndexByte(rest, '.')
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}
