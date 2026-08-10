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
// bases its write on — no store or broker needed. session_id is the
// discriminator: once an attempt actually started, the request is
// single-use and never runs again, whatever its status.
func TestShouldClaim(t *testing.T) {
	cases := []struct {
		name         string
		found        bool
		status       string
		sessionID    string
		numDelivered uint64
		wantClaim    bool
	}{
		{"no row: fresh run", false, "", "", 1, true},
		{"terminal row: never re-run", true, "ok", "", 1, false},
		{"terminal row: never re-run regardless of delivery count", true, "failed", "", 5, false},
		{"running row, no session, first delivery: owned elsewhere", true, WorkRequestStatusRunning, "", 1, false},
		{"running row, no session, redelivered: died during preparation, claim", true, WorkRequestStatusRunning, "", 2, true},
		{"running row, no session, redelivered many times: still claim", true, WorkRequestStatusRunning, "", 9, true},
		{"running row with session, first delivery: spent, never re-run", true, WorkRequestStatusRunning, "sess-1", 1, false},
		{"running row with session, redelivered: spent, never re-run", true, WorkRequestStatusRunning, "sess-1", 9, false},
		{"terminal row with session: spent, never re-run", true, "ok", "sess-1", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldClaim(c.found, c.status, c.sessionID, c.numDelivered); got != c.wantClaim {
				t.Fatalf("shouldClaim(%v, %q, %q, %d) = %v, want %v", c.found, c.status, c.sessionID, c.numDelivered, got, c.wantClaim)
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
	if out.Refusal != RefusalOwnedElsewhere {
		t.Fatalf("expected RefusalOwnedElsewhere for a duplicate of a live attempt, got %q", out.Refusal)
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

// TestClaimWorkRequestSessionSetRefusedAtEveryStatus is the phase 2 exit
// criterion at the store layer: once a request carries a session id it is
// spent, and a second claim is refused at every status the row can hold —
// running (the abandoned-mid-run shape) and each terminal one (the
// finished-normally shape). The refusal is RefusalSpent for a running row
// and RefusalTerminal for a finished one, because a finished request's
// stored result is republished rather than re-failed.
func TestClaimWorkRequestSessionSetRefusedAtEveryStatus(t *testing.T) {
	for _, status := range []string{WorkRequestStatusRunning, "ok", "failed", "timeout", "denied", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()

			if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
				t.Fatalf("first claim: %v", err)
			}
			if err := s.SetWorkRequestSession(ctx, "req-1", "sess-abandoned"); err != nil {
				t.Fatalf("attach session: %v", err)
			}
			if status != WorkRequestStatusRunning {
				matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-abandoned", status,
					json.RawMessage(`{"status":"`+status+`"}`), time.Now())
				if err != nil {
					t.Fatalf("finish: %v", err)
				}
				if !matched {
					t.Fatal("expected the finish to match the attached session")
				}
			}

			out, err := s.ClaimWorkRequest(ctx, "req-1", 9, time.Now())
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if out.Claimed {
				t.Fatalf("a row with a session id must never be claimed again, got %+v", out)
			}
			if !out.Found || out.Existing.SessionID != "sess-abandoned" {
				t.Fatalf("expected the existing spent row back, got %+v", out)
			}
			want := RefusalSpent
			if status != WorkRequestStatusRunning {
				want = RefusalTerminal
			}
			if out.Refusal != want {
				t.Fatalf("Refusal = %q, want %q for status %q", out.Refusal, want, status)
			}
		})
	}
}

// TestClaimWorkRequestRedeliveryBeforeSessionStillClaims is the phase 2
// "died before its session existed" exit criterion: a request whose attempt
// died during workspace preparation has no session id, nothing happened
// that matters, and a redelivery may still claim it.
func TestClaimWorkRequestRedeliveryBeforeSessionStillClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The attempt died before SetWorkRequestSession ran: the row is running
	// and sessionless.

	out, err := s.ClaimWorkRequest(ctx, "req-1", 2, time.Now())
	if err != nil {
		t.Fatalf("redelivered claim: %v", err)
	}
	if !out.Claimed {
		t.Fatalf("expected the sessionless redelivery to be claimed, got %+v", out)
	}
	if out.Existing.SessionID != "" {
		t.Fatalf("expected no session on the pre-session row, got %+v", out.Existing)
	}

	row, err := s.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != WorkRequestStatusRunning || row.DeliveryCount != 2 {
		t.Fatalf("unexpected row after the redelivered claim: %+v", row)
	}
}

// TestClaimWorkRequestRepublishesTerminalRow pins the existing terminal-row
// behaviour: a redelivery of a finished request republishes its stored
// result and never runs again.
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
	if out.Refusal != RefusalTerminal {
		t.Fatalf("Refusal = %q, want %q", out.Refusal, RefusalTerminal)
	}
	if out.Existing.Status != "ok" || string(out.Existing.Result) != string(result) {
		t.Fatalf("expected the stored terminal result back, got %+v", out.Existing)
	}
}

// TestFinishWorkRequestFencedBySession covers the guard that keeps a
// terminal outcome attributable to the attempt that owns the row: a finish
// from a session the row does not point at must not match and must not
// change the row. The worker's spent path records its failure under the
// abandoned session id and the validation and exhausted-delivery paths
// record under no session at all, so this guard is what stops a write from
// the wrong session clobbering the row's recorded outcome.
func TestFinishWorkRequestFencedBySession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-owner"); err != nil {
		t.Fatalf("attach owning session: %v", err)
	}

	// A finish from a different session id must not match.
	matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-other", "ok", json.RawMessage(`{"stale":true}`), time.Now())
	if err != nil {
		t.Fatalf("stale finish: %v", err)
	}
	if matched {
		t.Fatal("a finish from a session the row does not point at must not match")
	}
	// Neither may a finish from no session at all.
	matched, err = s.FinishWorkRequest(ctx, "req-1", "", "ok", json.RawMessage(`{"stale":true}`), time.Now())
	if err != nil {
		t.Fatalf("sessionless finish: %v", err)
	}
	if matched {
		t.Fatal("a sessionless finish must not match a row that owns a session")
	}

	row, err := s.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != WorkRequestStatusRunning || row.SessionID != "sess-owner" {
		t.Fatalf("expected the row to remain owned by the original session, got %+v", row)
	}

	// The owning session's own finish still matches.
	matched, err = s.FinishWorkRequest(ctx, "req-1", "sess-owner", "ok", json.RawMessage(`{"status":"ok"}`), time.Now())
	if err != nil {
		t.Fatalf("owning finish: %v", err)
	}
	if !matched {
		t.Fatal("expected the owning session's finish to match")
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
