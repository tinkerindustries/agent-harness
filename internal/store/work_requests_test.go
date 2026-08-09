package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestShouldClaim is the idempotency decision table docs/DESIGN.md §4.10
// describes, exercised directly against the pure function ClaimWorkRequest
// bases its write on — no store or broker needed.
func TestShouldClaim(t *testing.T) {
	cases := []struct {
		name         string
		found        bool
		status       string
		numDelivered uint64
		wantClaim    bool
	}{
		{"no row: fresh run", false, "", 1, true},
		{"terminal row: never re-run", true, "ok", 1, false},
		{"terminal row: never re-run regardless of delivery count", true, "failed", 5, false},
		{"running row, first delivery: owned elsewhere", true, WorkRequestStatusRunning, 1, false},
		{"running row, redelivered: abandoned, take over", true, WorkRequestStatusRunning, 2, true},
		{"running row, redelivered many times: still take over", true, WorkRequestStatusRunning, 9, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldClaim(c.found, c.status, c.numDelivered); got != c.wantClaim {
				t.Fatalf("shouldClaim(%v, %q, %d) = %v, want %v", c.found, c.status, c.numDelivered, got, c.wantClaim)
			}
		})
	}
}

func TestClaimWorkRequestFresh(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	out, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !out.Claimed || out.Found {
		t.Fatalf("expected a fresh claim, got %+v", out)
	}

	row, err := s.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != WorkRequestStatusRunning || row.DeliveryCount != 1 {
		t.Fatalf("unexpected row after fresh claim: %+v", row)
	}
}

// TestClaimWorkRequestDuplicateWhileRunning is the "publish the same
// request_id twice" exit criterion at the store layer: a second claim
// attempt for a row that is genuinely still running (delivery count 1, the
// first time this particular message has been seen) must not be allowed to
// run a second session.
func TestClaimWorkRequestDuplicateWhileRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	out, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if out.Claimed {
		t.Fatal("second claim while the first is still running must not succeed")
	}
	if !out.Found || out.Existing.Status != WorkRequestStatusRunning {
		t.Fatalf("expected to observe the still-running row, got %+v", out)
	}
}

// TestClaimWorkRequestConcurrentDuplicates fires many concurrent claims for
// one request_id, all reporting first delivery, the shape a flood of
// duplicate publishes produces. Exactly one may win. Run with -race.
func TestClaimWorkRequestConcurrentDuplicates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.ClaimWorkRequest(ctx, "req-race", 1, time.Now())
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if out.Claimed {
				mu.Lock()
				claims++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("expected exactly 1 winning claim among %d racing duplicates, got %d", n, claims)
	}
}

// TestClaimWorkRequestTakeoverAfterAbandonment is the "kill mid-run and
// restart" exit criterion at the store layer: a row left running by a dead
// process, redelivered (numDelivered > 1), is taken over.
func TestClaimWorkRequestTakeoverAfterAbandonment(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-abandoned"); err != nil {
		t.Fatalf("attach abandoned session: %v", err)
	}

	out, err := s.ClaimWorkRequest(ctx, "req-1", 2, time.Now())
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if !out.Claimed {
		t.Fatal("expected the redelivered row to be taken over")
	}
	if out.Existing.SessionID != "sess-abandoned" {
		t.Fatalf("expected to see the abandoned session id for linking, got %q", out.Existing.SessionID)
	}

	row, err := s.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != WorkRequestStatusRunning || row.DeliveryCount != 2 {
		t.Fatalf("unexpected row after takeover: %+v", row)
	}
}

func TestClaimWorkRequestRepublishesTerminalRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	result := json.RawMessage(`{"status":"ok"}`)
	matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-1", "ok", result, time.Now())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !matched {
		t.Fatal("expected FinishWorkRequest to match the owning session")
	}

	out, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now())
	if err != nil {
		t.Fatalf("claim after finish: %v", err)
	}
	if out.Claimed {
		t.Fatal("a terminal row must never be claimed again")
	}
	if out.Existing.Status != "ok" || string(out.Existing.Result) != string(result) {
		t.Fatalf("expected the stored terminal result back, got %+v", out.Existing)
	}
}

// TestFinishWorkRequestFencedByStaleSession covers the race a false
// redelivery (a heartbeat that failed to land before AckWait, not an
// actually dead process) can cause: a newer attempt takes over the row
// while the original is still genuinely running. The original's eventual,
// correct FinishWorkRequest call must not be allowed to clobber the
// takeover's result.
func TestFinishWorkRequestFencedByStaleSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-original"); err != nil {
		t.Fatalf("attach original session: %v", err)
	}

	// A redelivery believes the original abandoned and takes over.
	if _, err := s.ClaimWorkRequest(ctx, "req-1", 2, time.Now()); err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-newer"); err != nil {
		t.Fatalf("attach newer session: %v", err)
	}

	// The original attempt was alive after all and finishes late.
	matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-original", "ok", json.RawMessage(`{"stale":true}`), time.Now())
	if err != nil {
		t.Fatalf("stale finish: %v", err)
	}
	if matched {
		t.Fatal("a finish from a session the row no longer points at must not match")
	}

	row, err := s.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != WorkRequestStatusRunning || row.SessionID != "sess-newer" {
		t.Fatalf("expected the row to remain owned by the newer attempt, got %+v", row)
	}
}

func TestGetWorkRequestNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetWorkRequest(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAcquireWorkspaceLeaseWaitSucceedsAfterRelease proves the wait side of
// wait-or-fail: a contended lease is retried until the holder releases it,
// rather than failing on first contention.
func TestAcquireWorkspaceLeaseWaitSucceedsAfterRelease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.AcquireWorkspaceLeaseWait(ctx, "/tmp/ws", "sess-2", 10*time.Millisecond)
	}()

	time.Sleep(50 * time.Millisecond)
	if err := s.ReleaseWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the waiter to acquire after release, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned")
	}
}

// TestAcquireWorkspaceLeaseWaitGivesUpAtDeadline proves the fail side: a
// short-lived context bounds how long the wait lasts, which is what makes
// wait-or-fail a per-request choice driven by the request's own deadline
// rather than a separate flag.
func TestAcquireWorkspaceLeaseWaitGivesUpAtDeadline(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	err := s.AcquireWorkspaceLeaseWait(waitCtx, "/tmp/ws", "sess-2", 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to give up once its deadline passed")
	}
}
