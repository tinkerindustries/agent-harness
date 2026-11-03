package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// pollJobOutput calls job_output with args repeatedly until until reports
// true against the result, or fails the test after a bound the slowest CI
// machine should still clear comfortably. Each call drains whatever is new
// since the one before it — the same way a real session's repeated polling
// would — so a caller that wants a filter applied to every read passes it
// on args rather than adding a second, unfiltered call afterward that would
// drain the output the filtered one was meant to see.
func pollJobOutput(t *testing.T, e *Executor, args jobOutputArgs, until func(Result) bool) Result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last Result
	for time.Now().Before(deadline) {
		last = execJobOutput(t.Context(), e, mustJSON(t, args))
		if until(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition never met for background shell %s: %s", args.JobID, last.Content)
	return last
}

// drainBackgroundUntilDone polls job_output until the shell stops reporting
// "running" and returns every poll's content joined together.
//
// Joining is what makes an assertion about the shell's output
// deterministic. job_output consumes the output it returns, so a poll
// landing between a line being written and the process exiting carries that
// line away with it and the final poll says "(no new output)". A test that
// read only the last poll held when one poll happened to see the line and
// the exit together, and failed under load when two polls split them.
//
// waitForBackgroundDone is the one to use when the assertion is about the
// shell's final status rather than its output, because a joined content
// still has the earlier "Status: running" polls at the front of it.
func drainBackgroundUntilDone(t *testing.T, e *Executor, id string) Result {
	t.Helper()
	var seen strings.Builder
	res := pollJobOutput(t, e, jobOutputArgs{JobID: id}, func(r Result) bool {
		seen.WriteString(r.Content)
		seen.WriteString("\n")
		return !strings.HasPrefix(r.Content, "Status: running")
	})
	res.Content = seen.String()
	return res
}

// waitForBackgroundDone polls job_output until the shell reports anything
// other than "running", and returns that last poll.
func waitForBackgroundDone(t *testing.T, e *Executor, id string) Result {
	t.Helper()
	return pollJobOutput(t, e, jobOutputArgs{JobID: id}, func(r Result) bool {
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

func TestJobOutputReturnsOnlyNewOutputSinceLastRead(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo first; sleep 0.2; echo second", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	first := pollJobOutput(t, e, jobOutputArgs{JobID: id}, func(r Result) bool {
		return strings.Contains(r.Content, "first")
	})
	if strings.Contains(first.Content, "second") {
		t.Fatalf("expected the first poll to not yet see \"second\", got: %s", first.Content)
	}

	second := drainBackgroundUntilDone(t, e, id)
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

func TestJobOutputWithNoNewOutputSaysSo(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo hi", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	waitForBackgroundDone(t, e, id)

	again := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id}))
	if !strings.Contains(again.Content, "(no new output)") {
		t.Fatalf("expected no new output on a repeat poll, got: %s", again.Content)
	}
}

func TestJobOutputReportsNonZeroExitCode(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "exit 7", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	res := waitForBackgroundDone(t, e, id)
	if !strings.Contains(res.Content, "completed (exit code 7)") {
		t.Fatalf("expected exit code 7 reported, got: %s", res.Content)
	}
}

// TestJobOutputFilterKeepsOnlyMatchingLines spaces its three lines out with
// sleeps so more than one poll is likely needed, then accumulates every
// poll's content rather than trusting a single call to have caught
// everything — each poll only ever returns what is new since the one
// before it, filter included, so a test that only checked the last one
// could pass or fail on how the output happened to chunk.
func TestJobOutputFilterKeepsOnlyMatchingLines(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command:         "echo keep-me; sleep 0.05; echo drop-me; sleep 0.05; echo keep-me-too",
		RunInBackground: true,
	}))
	id := extractShellID(t, started.Content)

	var all strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id, Filter: "keep"}))
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

func TestJobKillStopsARunningShell(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 30", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	killed := execJobKill(t.Context(), e, mustJSON(t, jobKillArgs{JobID: id}))
	if killed.IsError || !strings.Contains(killed.Content, "killed") {
		t.Fatalf("expected a killed confirmation, got: %+v", killed)
	}

	res := waitForBackgroundDone(t, e, id)
	if !strings.Contains(res.Content, "Status: killed") {
		t.Fatalf("expected job_output to report killed, got: %s", res.Content)
	}
}

func TestJobKillOnAnAlreadyFinishedShellIsANoOp(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "true", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	waitForBackgroundDone(t, e, id)

	killed := execJobKill(t.Context(), e, mustJSON(t, jobKillArgs{JobID: id}))
	if killed.IsError {
		t.Fatalf("expected killing an already-finished shell to be a no-op, not an error: %s", killed.Content)
	}
	if !strings.Contains(killed.Content, "already finished") {
		t.Fatalf("expected the result to say it had already finished, got: %s", killed.Content)
	}
}

func TestJobOutputAndJobKillUnknownIdAreErrors(t *testing.T) {
	e, _ := newTestExecutor(t)
	if res := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: "bash_999"})); !res.IsError {
		t.Fatal("expected an unknown job_id to be an error")
	}
	if res := execJobKill(t.Context(), e, mustJSON(t, jobKillArgs{JobID: "bash_999"})); !res.IsError {
		t.Fatal("expected an unknown job_id to be an error")
	}
}

// TestExecutorCloseKillsRunningBackgroundShells pins the "never leave an
// agent behind" rule applied to a process rather than a session: an Executor
// that owns its shells ends a run's shells with the run.
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

// TestSessionShellsSurviveExecutorClose is that rule's other half: a registry
// the Executor does not own is the session's, so an ordinary run ending
// leaves the shell for the next run's job_output.
func TestSessionShellsSurviveExecutorClose(t *testing.T) {
	exits := make(chan ShellExit, 1)
	registry := NewShells("sess-test", func(_ string, exit ShellExit) { exits <- exit })
	e, _ := newTestExecutor(t)
	e.Shells = registry

	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo alive; sleep 30", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	if id != "bash_1" {
		t.Fatalf("expected the first id to be bash_1, got %q", id)
	}

	e.Close()

	res := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id}))
	if !strings.HasPrefix(res.Content, "Status: running") {
		t.Fatalf("expected the session registry's shell to outlive the run, got: %s", res.Content)
	}

	registry.Close()
	res = waitForBackgroundDone(t, e, id)
	if !strings.Contains(res.Content, "Status: killed") {
		t.Fatalf("expected the registry's Close to kill the shell, got: %s", res.Content)
	}
	// A close is not a completion: the run or the process is going away, so
	// the exit is kept out of the notice path.
	select {
	case exit := <-exits:
		t.Fatalf("a Close must not report a completion, got %+v", exit)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestShellsReportsExitCode pins the payload a completion event is built
// from: the shell's own exit code, and the command that produced it.
func TestShellsReportsExitCode(t *testing.T) {
	exits := make(chan ShellExit, 1)
	registry := NewShells("sess-test", func(_ string, exit ShellExit) { exits <- exit })
	e, _ := newTestExecutor(t)
	e.Shells = registry

	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "exit 7", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	waitForBackgroundDone(t, e, id)

	select {
	case exit := <-exits:
		if exit.ID != id || exit.ExitCode != 7 || exit.Killed || exit.Wedged {
			t.Fatalf("expected exit 7 for %s, got %+v", id, exit)
		}
		if exit.Command != "exit 7" {
			t.Fatalf("expected the command in the exit, got %q", exit.Command)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registry never reported the shell's exit")
	}
}

// TestShellsReportTheSessionCurrentAtExit pins the compaction handover: the
// callback names the session the registry points at when the shell exits, not
// the one it was created under, because by then the run may have compacted
// into a successor row.
func TestShellsReportTheSessionCurrentAtExit(t *testing.T) {
	type report struct {
		session string
		exit    ShellExit
	}
	exits := make(chan report, 1)
	registry := NewShells("sess-original", func(sessionID string, exit ShellExit) {
		exits <- report{session: sessionID, exit: exit}
	})
	e, _ := newTestExecutor(t)
	e.Shells = registry

	started := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command:         "sleep 0.3; exit 5",
		RunInBackground: true,
	}))
	id := extractShellID(t, started.Content)

	registry.SetSessionID("sess-successor")
	waitForBackgroundDone(t, e, id)

	select {
	case got := <-exits:
		if got.session != "sess-successor" {
			t.Fatalf("the exit reported session %q, want the successor", got.session)
		}
		if got.exit.ID != id || got.exit.ExitCode != 5 {
			t.Fatalf("unexpected exit report: %+v", got.exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registry never reported the shell's exit")
	}
}

// TestJobOutputConcurrentPollsDoNotPanicOrDuplicate drives many concurrent
// job_output calls against one fast-writing shell. buf.String() and
// readOffset must be read together under bg.mu. Reading the buffer outside
// the lock lets two callers snapshot it at different lengths while sharing
// one readOffset, and whichever locks second can slice its own, shorter
// snapshot from an offset the first already advanced past it, which panics
// with a slice bounds error.
//
// The results cannot be joined to reconstruct the job's output: they arrive
// in completion order rather than read order, and a read can end mid-line.
// The total holds regardless of order. Every byte the shell wrote is returned
// by exactly one call, so the payload lengths must add up to the output's own
// length; a byte returned twice makes the total too large and a byte dropped
// makes it too small.
func TestJobOutputConcurrentPollsDoNotPanicOrDuplicate(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command:         "for i in $(seq 1 200); do echo line-$i; done",
		RunInBackground: true,
	}))
	id := extractShellID(t, started.Content)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var contents []string
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				res := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id}))
				mu.Lock()
				contents = append(contents, res.Content)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// The 20x20 concurrent calls above race Start(); the shell may not have
	// written anything by the time they are all done. The polls that follow
	// consume output too, and a poll that lands between a write and the exit
	// carries those bytes away, so every poll's result is kept and not just
	// the last one.
	pollJobOutput(t, e, jobOutputArgs{JobID: id}, func(r Result) bool {
		contents = append(contents, r.Content)
		return !strings.HasPrefix(r.Content, "Status: running")
	})

	var want strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&want, "line-%d\n", i)
	}

	var got int
	for _, content := range contents {
		_, payload, ok := strings.Cut(content, "\n\n")
		if !ok {
			t.Fatalf("a job_output result carried no payload: %q", content)
		}
		if payload == "(no new output)" {
			continue
		}
		got += len(payload)
	}
	if got != want.Len() {
		t.Fatalf("the polls returned %d bytes of output, want %d: a byte was lost or returned twice", got, want.Len())
	}
}

// TestJobListReportsEveryJob pins job_list's row for a job that has exited
// and one still running, each with the command it was started with.
func TestJobListReportsEveryJob(t *testing.T) {
	e, _ := newTestExecutor(t)
	fast := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo done", RunInBackground: true}))
	fastID := extractShellID(t, fast.Content)
	slow := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 30", RunInBackground: true}))
	slowID := extractShellID(t, slow.Content)
	t.Cleanup(func() { e.CloseAll() })

	waitForBackgroundDone(t, e, fastID)

	res := execJobList(t.Context(), e, mustJSON(t, struct{}{}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{
		fastID + ": exited (exit code 0)",
		slowID + ": running",
		"command: echo done",
		"command: sleep 30",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("job_list is missing %q:\n%s", want, res.Content)
		}
	}
}

// TestJobListWithoutJobsSaysSo pins the empty answer: a session that never
// started a job reads a sentence rather than an empty result.
func TestJobListWithoutJobsSaysSo(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execJobList(t.Context(), e, mustJSON(t, struct{}{}))
	if res.IsError || !strings.Contains(res.Content, "No background jobs") {
		t.Fatalf("expected an empty-job sentence, got: %+v", res)
	}
}

// TestJobOutputWaitBlocksUntilTheJobFinishes pins wait's purpose: the call
// returns once the job has exited, carrying the output and the exit status,
// without the caller polling.
func TestJobOutputWaitBlocksUntilTheJobFinishes(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "sleep 0.2; echo finished", RunInBackground: true}))
	id := extractShellID(t, started.Content)

	begin := time.Now()
	res := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id, Wait: true}))
	if elapsed := time.Since(begin); elapsed < 150*time.Millisecond {
		t.Fatalf("wait returned after %s, before the job could have finished", elapsed)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "completed (exit code 0)") || !strings.Contains(res.Content, "finished") {
		t.Fatalf("expected the finished job's status and output, got: %s", res.Content)
	}
}

// TestJobOutputWaitTimesOutAndLeavesTheJobRunning pins the other half: a wait
// bounded by the call's own timeout returns what the job has written and does
// not end the job.
func TestJobOutputWaitTimesOutAndLeavesTheJobRunning(t *testing.T) {
	e, _ := newTestExecutor(t)
	started := execBash(t.Context(), e, mustJSON(t, bashArgs{Command: "echo started; sleep 30", RunInBackground: true}))
	id := extractShellID(t, started.Content)
	t.Cleanup(func() { e.CloseAll() })

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	res := execJobOutput(ctx, e, mustJSON(t, jobOutputArgs{JobID: id, Wait: true}))
	if res.IsError {
		t.Fatalf("a timed-out wait is not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "still running") {
		t.Fatalf("expected the timed-out wait to say the job is still running, got: %s", res.Content)
	}

	after := execJobOutput(t.Context(), e, mustJSON(t, jobOutputArgs{JobID: id}))
	if !strings.HasPrefix(after.Content, "Status: running") {
		t.Fatalf("expected the job to survive a timed-out wait, got: %s", after.Content)
	}
}

// extractShellID pulls "bash_N" out of bash's own run_in_background result
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
