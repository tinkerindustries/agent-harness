package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// createCreatingSession creates a session row in the "creating" status, the
// shape Runner.Create leaves before the worker has prepared the workspace.
func createCreatingSession(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.CreateSession(context.Background(), Session{
		ID:             id,
		Model:          "deepseek-v4-flash",
		Effort:         "high",
		Workspace:      "/tmp/" + id,
		PermissionMode: "default",
		Status:         StatusCreating,
	})
	if err != nil {
		t.Fatalf("create creating session %s: %v", id, err)
	}
}

// TestPromoteSessionFromCreating pins the promotion Runner.Run performs on a
// pre-created row: status flips creating → running, the three columns that
// are only resolvable once the run starts are written, and version bumps.
func TestPromoteSessionFromCreating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createCreatingSession(t, s, "sess-1")

	if err := s.PromoteSession(ctx, "sess-1", "/tmp/sess-1-real", "the rendered prompt", []byte(`[{"name":"Bash"}]`)); err != nil {
		t.Fatalf("promote: %v", err)
	}
	sess, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusRunning {
		t.Fatalf("expected status running after promotion, got %q", sess.Status)
	}
	if sess.Workspace != "/tmp/sess-1-real" {
		t.Fatalf("expected the promoted workspace, got %q", sess.Workspace)
	}
	if sess.SystemPrompt != "the rendered prompt" {
		t.Fatalf("expected the promoted system prompt, got %q", sess.SystemPrompt)
	}
	if string(sess.ToolSchema) != `[{"name":"Bash"}]` {
		t.Fatalf("expected the promoted tool schema, got %q", sess.ToolSchema)
	}
	if sess.Version != 2 {
		t.Fatalf("expected version 2 after promotion, got %d", sess.Version)
	}
}

// TestPromoteSessionAlreadyRunningIsNoOp pins the idempotency a redelivery
// needs: promoting a row that is already running succeeds without touching
// it, so a retried Runner.Run cannot fail on its own row.
func TestPromoteSessionAlreadyRunningIsNoOp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1") // CreateSession defaults to StatusRunning
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PromoteSession(ctx, "sess-1", "/tmp/other", "another prompt", []byte(`[]`)); err != nil {
		t.Fatalf("promote an already-running row: %v", err)
	}
	after, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusRunning || after.Workspace != before.Workspace ||
		after.SystemPrompt != before.SystemPrompt || after.Version != before.Version {
		t.Fatalf("promoting an already-running row must be a no-op, got %+v", after)
	}
}

// TestPromoteSessionRefusesTerminal pins the fence against relabelling a row
// the run ended under: a stop during preparation marks the row cancelled, and
// a wedged goroutine that wakes and tries to promote must be refused, not
// resurrect the run.
func TestPromoteSessionRefusesTerminal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createCreatingSession(t, s, "cancelled")
	if err := s.CancelRunningSession(ctx, "cancelled", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var sfe *SessionFinishedError
	if err := s.PromoteSession(ctx, "cancelled", "/tmp/x", "p", []byte(`[]`)); !errors.As(err, &sfe) {
		t.Fatalf("expected SessionFinishedError promoting a cancelled row, got %v", err)
	}
	if sfe.Status != StatusCancelled {
		t.Fatalf("expected the error to name cancelled, got %q", sfe.Status)
	}

	mustCreateSession(t, s, "ok")
	finished := time.Now().UTC()
	if err := s.UpdateSessionStatus(ctx, "ok", StatusOK, &finished); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteSession(ctx, "ok", "/tmp/x", "p", []byte(`[]`)); !errors.As(err, &sfe) {
		t.Fatalf("expected SessionFinishedError promoting an ok row, got %v", err)
	}
}

// TestCancelRunningSessionAcceptsCreating is the store half of a stop during
// a clone: the row exists as "creating" (Runner.Create ran, prepareWorkspace
// is still going), and the escalation's CancelRunningSession must mark it
// cancelled instead of finding nothing to mark.
func TestCancelRunningSessionAcceptsCreating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createCreatingSession(t, s, "sess-1")

	now := time.Now().UTC()
	if err := s.CancelRunningSession(ctx, "sess-1", now); err != nil {
		t.Fatalf("cancel a creating session: %v", err)
	}
	sess, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %q", sess.Status)
	}
	if sess.FinishedAt == nil || !sess.FinishedAt.Equal(now) {
		t.Fatalf("expected finished_at = the cancel time, got %v", sess.FinishedAt)
	}
}

// TestCloseSessionAcceptsCreating pins the operator-facing repair path for a
// row a dead worker left mid-clone: a "creating" row with no events passes
// the idle check and takes the new terminal status, so the operations screen
// can close it exactly as it closes an abandoned running session.
func TestCloseSessionAcceptsCreating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createCreatingSession(t, s, "abandoned-mid-clone")

	now := time.Now().UTC()
	closed, err := s.CloseSession(ctx, "abandoned-mid-clone", StatusFailed, 1, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("close a creating session: %v", err)
	}
	if closed.Status != StatusFailed || closed.FinishedAt == nil {
		t.Fatalf("expected failed with finished_at, got %+v", closed)
	}
	if closed.Version != 2 {
		t.Fatalf("expected version 2 after the close, got %d", closed.Version)
	}
}

// TestDeleteSessionRefusesCreating pins the delete fence for the preparation
// window: a worker is cloning into that directory, so deleting the row out
// from under it would race the run, exactly as deleting a running row would.
func TestDeleteSessionRefusesCreating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createCreatingSession(t, s, "sess-1")

	var running *SessionRunningError
	if err := s.DeleteSession(ctx, "sess-1", 1); !errors.As(err, &running) {
		t.Fatalf("expected SessionRunningError deleting a creating session, got %v", err)
	}
	if _, err := s.GetSession(ctx, "sess-1"); err != nil {
		t.Fatalf("session should still exist after a refused delete: %v", err)
	}
}

// TestListSessionsPagePlacesCreatingUnderRunning pins the status filter
// widening: a "creating" row is live, so ?status=running must include it and
// ?status=finished must not — otherwise a preparing session would vanish
// from the in-flight list and appear in the finished table.
func TestListSessionsPagePlacesCreatingUnderRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "run-1")
	createCreatingSession(t, s, "creating-1")
	mustCreateSession(t, s, "done-1")
	finishSession(t, s, "done-1")

	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Status: "running", Limit: 20})
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 live rows (running + creating), got total %d", total)
	}
	got := map[string]bool{}
	for _, sess := range page {
		got[sess.ID] = true
		if !IsLive(sess.Status) {
			t.Fatalf("expected only live rows under the running filter, got %q", sess.Status)
		}
	}
	if !got["run-1"] || !got["creating-1"] {
		t.Fatalf("expected run-1 and creating-1 under running, got %+v", page)
	}

	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Status: "finished", Limit: 20})
	if err != nil {
		t.Fatalf("list finished: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 finished row, got total %d", total)
	}
	for _, sess := range page {
		if IsLive(sess.Status) {
			t.Fatalf("expected no live rows under finished, got %q", sess.Status)
		}
	}
}

// TestIsLive pins the helper every store branch and SQL predicate builds
// off: running and creating are live, everything else is not.
func TestIsLive(t *testing.T) {
	for _, status := range []string{StatusRunning, StatusCreating} {
		if !IsLive(status) {
			t.Fatalf("IsLive(%q) = false, want true", status)
		}
	}
	for _, status := range []string{StatusOK, StatusFailed, StatusTimeout, StatusMaxTurns, StatusCancelled, StatusCompacted, "", "denied"} {
		if IsLive(status) {
			t.Fatalf("IsLive(%q) = true, want false", status)
		}
	}
}
