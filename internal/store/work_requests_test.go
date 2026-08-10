package store

import (
	"context"
	"encoding/json"
	"errors"
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

// TestListWorkRequestsNewestFirst pins GET /api/requests's store layer: every
// row, newest first by received_at — the work-request analog of the sessions
// list's created_at order — with the sessionless running row a request whose
// worker died during workspace preparation leaves behind present and shaped
// like any other row.
func TestListWorkRequestsNewestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Oldest: a request that never ran — claimed an hour ago, no session
	// ever attached.
	if _, err := s.ClaimWorkRequest(ctx, "req-old-sessionless", 1, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("claim sessionless: %v", err)
	}
	// Middle: a request whose attempt actually started.
	requestWithSession(t, s, "req-mid", "sess-mid")
	// Newest: a request that ran to completion.
	requestWithSession(t, s, "req-new", "sess-new")
	if matched, err := s.FinishWorkRequest(ctx, "req-new", "sess-new", "ok", json.RawMessage(`{"status":"ok"}`), time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	rows, err := s.ListWorkRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %+v", len(rows), rows)
	}
	wantOrder := []string{"req-new", "req-mid", "req-old-sessionless"}
	for i, want := range wantOrder {
		if rows[i].RequestID != want {
			t.Fatalf("row %d = %q, want %q (newest first by received_at)", i, rows[i].RequestID, want)
		}
	}
	// The sessionless running row is present, the shape a request whose
	// worker died during preparation leaves behind.
	var sessionless *WorkRequest
	for i := range rows {
		if rows[i].RequestID == "req-old-sessionless" {
			sessionless = &rows[i]
		}
	}
	if sessionless == nil {
		t.Fatal("the sessionless running request must appear in the list")
	}
	if sessionless.Status != WorkRequestStatusRunning || sessionless.SessionID != "" || sessionless.Version != 1 {
		t.Fatalf("unexpected sessionless row: %+v", sessionless)
	}
}

// requestWithSession claims requestID, creates sessionID, appends one event
// (created at call time), and attaches the session — the row shape of a
// request whose attempt actually started. The event's timestamp is the
// liveness signal CloseWorkRequest and DeleteWorkRequest judge against the
// caller's `now`, so a test controls "live" vs "dead" purely through the now
// it passes.
func requestWithSession(t *testing.T, s *Store, requestID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ClaimWorkRequest(ctx, requestID, 1, time.Now().UTC()); err != nil {
		t.Fatalf("claim %s: %v", requestID, err)
	}
	mustCreateSession(t, s, sessionID)
	if _, err := s.AppendEvents(ctx, sessionID, []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, requestID, sessionID); err != nil {
		t.Fatalf("attach session: %v", err)
	}
}

// TestCloseWorkRequestClosesDeadRequest pins the success path (docs/DATA-API.md
// phase 3): a running request whose session is idle — or that has no session
// at all — can be closed into a terminal status, which sets finished_at and
// bumps the version. A session that never appended an event has nothing
// recent and passes the idle check the same way.
func TestCloseWorkRequestClosesDeadRequest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A request whose worker died mid-run: session attached, last event two
	// hours ago.
	requestWithSession(t, s, "req-dead", "sess-dead")
	// A request that never ran: claimed but no session was ever attached.
	if _, err := s.ClaimWorkRequest(ctx, "req-never-ran", 1, time.Now().UTC()); err != nil {
		t.Fatalf("claim req-never-ran: %v", err)
	}
	// A request whose attempt died before its first event: session exists,
	// no events at all.
	if _, err := s.ClaimWorkRequest(ctx, "req-no-events", 1, time.Now().UTC()); err != nil {
		t.Fatalf("claim req-no-events: %v", err)
	}
	mustCreateSession(t, s, "sess-no-events")
	if err := s.SetWorkRequestSession(ctx, "req-no-events", "sess-no-events"); err != nil {
		t.Fatalf("attach session: %v", err)
	}

	future := time.Now().UTC().Add(2 * time.Hour)
	for _, tc := range []struct {
		name      string
		requestID string
	}{
		{"with idle session", "req-dead"},
		{"with no session", "req-never-ran"},
		{"with eventless session", "req-no-events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := s.GetWorkRequest(ctx, tc.requestID)
			if err != nil {
				t.Fatal(err)
			}
			closed, err := s.CloseWorkRequest(ctx, tc.requestID, "cancelled", before.Version, future, 10*time.Minute)
			if err != nil {
				t.Fatalf("close: %v", err)
			}
			if closed.Status != "cancelled" || closed.FinishedAt == nil {
				t.Fatalf("expected cancelled with finished_at, got %+v", closed)
			}
			if closed.Version != before.Version+1 {
				t.Fatalf("version = %d, want %d (one mutation)", closed.Version, before.Version+1)
			}
			// A fresh read agrees.
			row, err := s.GetWorkRequest(ctx, tc.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != "cancelled" || row.FinishedAt == nil {
				t.Fatalf("store row not closed: %+v", row)
			}
		})
	}
}

// TestCloseWorkRequestRefusesLiveRequest pins the precondition that matters
// (docs/DATA-API.md phase 3): a running request whose session's most recent
// event is newer than the idle threshold is being run by a live pool worker
// right now, and the close is a 409 whose message names when the session was
// last heard from. The row is untouched.
func TestCloseWorkRequestRefusesLiveRequest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	requestWithSession(t, s, "req-live", "sess-live")

	before, err := s.GetWorkRequest(ctx, "req-live")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CloseWorkRequest(ctx, "req-live", "cancelled", before.Version, time.Now().UTC(), 10*time.Minute)
	var active *ActiveRequestError
	if !errors.As(err, &active) {
		t.Fatalf("expected ActiveRequestError, got %v", err)
	}
	if active.RequestID != "req-live" || active.SessionID != "sess-live" {
		t.Fatalf("error must name the request and its session, got %+v", active)
	}
	if active.LastEventAt.IsZero() {
		t.Fatalf("error must name the last event's time, got %+v", active)
	}

	row, err := s.GetWorkRequest(ctx, "req-live")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != WorkRequestStatusRunning || row.FinishedAt != nil || row.Version != before.Version {
		t.Fatalf("a refused close must not touch the row, got %+v", row)
	}
}

// TestCloseWorkRequestRecloseIsVersionBump pins the idempotent re-close
// (docs/DATA-API.md phase 3, mirroring CloseSession): a request already
// terminal keeps the status it finished with, so a retried PATCH is a version
// bump and nothing else and a finished request cannot be relabelled.
func TestCloseWorkRequestRecloseIsVersionBump(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	requestWithSession(t, s, "req-finished", "sess-finished")
	result := json.RawMessage(`{"status":"ok","text":"done"}`)
	if matched, err := s.FinishWorkRequest(ctx, "req-finished", "sess-finished", "ok", result, time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	before, err := s.GetWorkRequest(ctx, "req-finished")
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "ok" {
		t.Fatalf("expected the row finished ok, got %+v", before)
	}
	reclosed, err := s.CloseWorkRequest(ctx, "req-finished", "cancelled", before.Version, time.Now().UTC(), 10*time.Minute)
	if err != nil {
		t.Fatalf("re-close: %v", err)
	}
	if reclosed.Status != "ok" {
		t.Fatalf("a re-close must keep the status the request finished with, got %q", reclosed.Status)
	}
	if reclosed.FinishedAt == nil || !reclosed.FinishedAt.Equal(*before.FinishedAt) {
		t.Fatalf("a re-close must keep finished_at, got %+v", reclosed.FinishedAt)
	}
	if reclosed.Version != before.Version+1 {
		t.Fatalf("version = %d, want %d", reclosed.Version, before.Version+1)
	}
}

// TestCloseWorkRequestGuards pins the remaining write preconditions: a stale
// If-Match version is a VersionConflictError, an unknown request is
// ErrNotFound, and "running" is not a terminal status the store accepts.
func TestCloseWorkRequestGuards(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	requestWithSession(t, s, "req-1", "sess-1")

	_, err := s.CloseWorkRequest(ctx, "req-1", "cancelled", 99, time.Now().UTC(), 10*time.Minute)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError, got %v", err)
	}
	if conflict.Resource != "work_request req-1" {
		t.Fatalf("conflict must name the resource, got %q", conflict.Resource)
	}

	if _, err := s.CloseWorkRequest(ctx, "does-not-exist", "cancelled", 1, time.Now().UTC(), 10*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if _, err := s.CloseWorkRequest(ctx, "req-1", WorkRequestStatusRunning, 1, time.Now().UTC(), 10*time.Minute); err == nil {
		t.Fatal("closing into running must be rejected")
	}
}

// TestDeleteWorkRequest pins DELETE /api/requests/{request_id}'s store layer:
// a terminal row is removed; a dead-but-running row (idle session, or no
// session at all) is removed directly; a row whose session is live is refused
// with ActiveRequestError; a stale version and an unknown id refuse.
func TestDeleteWorkRequest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Terminal: finished normally.
	requestWithSession(t, s, "req-terminal", "sess-terminal")
	if matched, err := s.FinishWorkRequest(ctx, "req-terminal", "sess-terminal", "ok", json.RawMessage(`{"status":"ok"}`), time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}
	// Dead: running with an idle session.
	requestWithSession(t, s, "req-dead", "sess-dead")
	// Never ran: running with no session.
	if _, err := s.ClaimWorkRequest(ctx, "req-never-ran", 1, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	future := time.Now().UTC().Add(2 * time.Hour)
	for _, tc := range []struct {
		name      string
		requestID string
	}{
		{"terminal", "req-terminal"},
		{"running with idle session", "req-dead"},
		{"running with no session", "req-never-ran"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := s.GetWorkRequest(ctx, tc.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteWorkRequest(ctx, tc.requestID, before.Version, future, 10*time.Minute); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := s.GetWorkRequest(ctx, tc.requestID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("row should be gone, got %v", err)
			}
		})
	}

	// Live: refused with ActiveRequestError naming the last event.
	requestWithSession(t, s, "req-live", "sess-live")
	before, err := s.GetWorkRequest(ctx, "req-live")
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteWorkRequest(ctx, "req-live", before.Version, time.Now().UTC(), 10*time.Minute)
	var active *ActiveRequestError
	if !errors.As(err, &active) {
		t.Fatalf("expected ActiveRequestError for a live request, got %v", err)
	}
	if _, err := s.GetWorkRequest(ctx, "req-live"); err != nil {
		t.Fatalf("a refused delete must leave the row, got %v", err)
	}

	// Stale version and unknown id.
	requestWithSession(t, s, "req-2", "sess-2")
	row, err := s.GetWorkRequest(ctx, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	var conflict *VersionConflictError
	if err := s.DeleteWorkRequest(ctx, "req-2", row.Version+1, future, 10*time.Minute); !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError for a stale delete, got %v", err)
	}
	if err := s.DeleteWorkRequest(ctx, "does-not-exist", 1, future, 10*time.Minute); !errors.Is(err, ErrNotFound) {
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
