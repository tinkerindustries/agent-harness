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

// TestStopDuringPreparationSoftStopCancelsRowAndResult is the soft half of
// the stop-during-a-clone shape: preparation that answers a cancelled
// context — as a real clone does — fails with context.Canceled, and the run
// goroutine's own setup-failure path must report the stop as cancelled: a
// cancelled result carrying the operator's reason, and the "creating" row
// marked cancelled, not left dangling and not marked failed.
// (TestStopDuringPreparationMarksRowCancelled covers the wedged half, where
// preparation ignores ctx and the escalation force-finishes; this one covers
// the ordinary case where a stop lands mid-clone and the clone gives up.)
func TestStopDuringPreparationSoftStopCancelsRowAndResult(t *testing.T) {
	runner := &ctxBlockingRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		runner.store = st
		return runner
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond

	// Preparation blocks until the stop cancels its context, then reports
	// the cancellation — the way a real clone answers a cancelled ctx. It
	// must never reach Runner.Run.
	preparing := make(chan struct{})
	h.pool.PrepareWorkspace = func(ctx context.Context, root, sessionID string, _ []queue.Repo, _ []workspace.Attachment) (string, error) {
		close(preparing)
		<-ctx.Done()
		return "", ctx.Err()
	}
	defer h.startPool(t)()

	requestID := uniqueID("req-prep-soft-stop")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full"})

	select {
	case <-preparing:
	case <-time.After(10 * time.Second):
		t.Fatal("preparation never started")
	}
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)
	waitForSessionStatus(t, h, sessionID, store.StatusCreating, 5*time.Second)

	if err := h.pool.Stop(sessionID, "the operator stopped the clone"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The run's own setup-failure path answers: a cancelled result with the
	// operator's reason, not a workspace_setup failure.
	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "cancelled" || res.Error.Message != "the operator stopped the clone" {
		t.Fatalf("expected error {cancelled, the operator stopped the clone}, got %+v", res.Error)
	}

	// The creating session row is marked cancelled with finished_at — the
	// §4.10 promise that a stop during a clone marks the row instead of
	// finding no row to mark.
	sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 5*time.Second)
	if sess.FinishedAt == nil {
		t.Fatal("expected the cancelled session to carry finished_at")
	}

	// The work_requests row agrees with the result the caller reads back.
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != queue.StatusCancelled {
		t.Fatalf("expected the row to be recorded cancelled, got %q", row.Status)
	}
	h.waitForNotRunning(t, sessionID, 2*time.Second)
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
	// and no second finish lands (a second finish would bump the version).
	release()
	time.Sleep(300 * time.Millisecond)
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != queue.StatusCancelled {
		t.Fatalf("expected the row to stay cancelled, got %q", row.Status)
	}
	if sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 2*time.Second); sess.Status != store.StatusCancelled {
		t.Fatalf("expected the session to stay cancelled, got %q", sess.Status)
	}
}
