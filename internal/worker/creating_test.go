package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/workspace"
)

// TestPoolPrepareWorkspaceFailureMarksSessionFailed drives the setup-failure
// path end to end: the row is created "creating" before preparation, the
// clone fails, and the run must mark that row failed (with an error event
// saying why) rather than leaving it stuck in "creating" — the state a dead
// worker used to leave, with nothing on the session page to say what
// happened.
func TestPoolPrepareWorkspaceFailureMarksSessionFailed(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "must never run", 0, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 1)
	h.pool.PrepareWorkspace = func(context.Context, string, string, []queue.Repo, []workspace.Attachment) (string, error) {
		return "", errors.New("clone refused: branch does not exist")
	}
	defer h.startPool(t)()

	requestID := uniqueID("req-setup-fail")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full"})

	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusFailed {
		t.Fatalf("expected a failed result, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "workspace_setup" {
		t.Fatalf("expected error code workspace_setup, got %+v", res.Error)
	}
	if res.SessionID == "" {
		t.Fatal("expected the result to carry the attempt's session id")
	}
	if hits.count() != 0 {
		t.Fatalf("expected no session to have run, got %d hits", hits.count())
	}

	// The session row is failed, not left stuck in "creating".
	sess, err := h.pool.Store.GetSession(context.Background(), res.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.StatusFailed {
		t.Fatalf("expected the row to be failed, got %q", sess.Status)
	}
	if sess.FinishedAt == nil {
		t.Fatal("expected the failed row to carry finished_at")
	}

	// The error event is what the session page shows for why the run never
	// started.
	events, err := h.pool.Store.GetEvents(context.Background(), res.SessionID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 1 || events[0].Kind != store.KindError {
		t.Fatalf("expected exactly one error event, got %+v", events)
	}
	var payload store.ErrorPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Message, "clone refused") {
		t.Fatalf("expected the error event to name the cause, got %q", payload.Message)
	}
}

// TestStopDuringPreparationMarksRowCancelled is the stop-during-a-clone
// shape the creating status exists for: the row is "creating" while
// preparation is wedged, and a stop must mark that row cancelled (the
// escalation's CancelRunningSession accepting creating) instead of the old
// "no session row to mark" branch leaving the row behind as creating.
func TestStopDuringPreparationMarksRowCancelled(t *testing.T) {
	runner := &ctxBlockingRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		runner.store = st
		return runner
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond

	// Preparation wedges on a gate the test controls; the run never reaches
	// Runner.Run while wedged, exactly like a clone that ignores ctx.
	gate := make(chan struct{})
	wedged := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	h.pool.PrepareWorkspace = func(ctx context.Context, root, sessionID string, _ []queue.Repo, _ []workspace.Attachment) (string, error) {
		close(wedged)
		<-gate
		return filepath.Join(root, sessionID), nil
	}
	stop := h.startPool(t)
	defer stop()
	defer release()

	requestID := uniqueID("req-prep-stop")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full"})

	// Wait until preparation is genuinely wedged, then confirm the row was
	// created as "creating" — the state a stop now has a row to mark.
	select {
	case <-wedged:
	case <-time.After(10 * time.Second):
		t.Fatal("preparation never wedged")
	}
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)
	if sess := waitForSessionStatus(t, h, sessionID, store.StatusCreating, 5*time.Second); sess.FinishedAt != nil {
		t.Fatal("expected a creating row with no finished_at before the stop")
	}

	if err := h.pool.Stop(sessionID, "the operator gave up on the clone"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The escalation marks the creating row cancelled within the grace
	// period, and the caller gets the cancelled result.
	sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 5*time.Second)
	if sess.FinishedAt == nil {
		t.Fatal("expected the cancelled session to carry finished_at")
	}
	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "cancelled" || res.Error.Message != "the operator gave up on the clone" {
		t.Fatalf("expected error {cancelled, the operator gave up on the clone}, got %+v", res.Error)
	}
	h.waitForNotRunning(t, sessionID, 2*time.Second)

	// Release the wedged preparation: the run wakes, finds the message
	// already answered, and drops its own result — the row stays cancelled
	// and no second result appears.
	release()
	time.Sleep(300 * time.Millisecond)
	if n := h.countFinalResults(t, requestID, 2*time.Second); n != 1 {
		t.Fatalf("expected exactly 1 final result, got %d", n)
	}
	if sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 2*time.Second); sess.Status != store.StatusCancelled {
		t.Fatalf("expected the row to stay cancelled, got %q", sess.Status)
	}
}
