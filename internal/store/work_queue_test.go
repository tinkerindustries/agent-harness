package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"
)

// testNow is the fixed clock every test in this file passes as `now`, so the
// queue's time-dependent semantics are exercised by arithmetic on this value
// and nothing sleeps.
var testNow = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

const testLease = 60 * time.Second

func TestWorkQueueEnqueueClaimRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	payload := []byte(`{"request_id":"req-1","prompt":"do the thing","repos":[{"url":"https://example.com/org/app.git"}],"permission_mode":"readonly"}`)
	if err := s.EnqueueWork(ctx, "req-1", payload, testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(got))
	}
	qw := got[0]
	if qw.RequestID != "req-1" {
		t.Fatalf("request id = %q, want req-1", qw.RequestID)
	}
	if string(qw.Payload) != string(payload) {
		t.Fatalf("payload did not round-trip byte-identically:\n got %q\nwant %q", qw.Payload, payload)
	}
	if qw.DeliveryCount != 1 {
		t.Fatalf("delivery count = %d, want 1 on the first claim", qw.DeliveryCount)
	}
	if qw.ID != 1 {
		t.Fatalf("id = %d, want the surrogate key 1", qw.ID)
	}
}

// TestWorkQueueClaimRespectsVisibleAt is the Nak path: a Nak'd row is
// invisible until now passes the delay, and claimable again — with the
// delivery count incremented — once it has.
func TestWorkQueueClaimRespectsVisibleAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueWork(ctx, "req-1", []byte("p1"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("first claim returned %d rows, want 1", len(claimed))
	}
	id := claimed[0].ID

	const delay = 5 * time.Second
	if err := s.NakWork(ctx, id, delay, 5, testNow); err != nil {
		t.Fatalf("nak: %v", err)
	}

	// Before the delay elapses the row is invisible.
	if got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(4*time.Second)); err != nil {
		t.Fatalf("claim before delay: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("claim before delay returned %d rows, want 0", len(got))
	}

	// At exactly visible_at the row is claimable (visible_at_ms <= now).
	got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(delay))
	if err != nil {
		t.Fatalf("claim at delay expiry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim at delay expiry returned %d rows, want 1", len(got))
	}
	if got[0].ID != id || got[0].DeliveryCount != 2 {
		t.Fatalf("redelivered row = %+v, want id %d with delivery count 2", got[0], id)
	}
}

// TestWorkQueueLeaseExpiryRedelivers is the crash path at the row level:
// claim, do not ack, advance now past the lease, claim again — the same row
// comes back with delivery_count 2.
func TestWorkQueueLeaseExpiryRedelivers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueWork(ctx, "req-1", []byte("p1"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 || first[0].DeliveryCount != 1 {
		t.Fatalf("first claim = %+v, want one row with delivery count 1", first)
	}
	id := first[0].ID

	// Mid-lease the row is still owned.
	if got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(30*time.Second)); err != nil {
		t.Fatalf("claim mid-lease: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("claim mid-lease returned %d rows, want 0", len(got))
	}

	// At exactly lease expiry the row is claimable again (lease_expires_ms
	// <= now covers both "never leased" and "lease expired").
	second, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(testLease))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim returned %d rows, want 1", len(second))
	}
	if second[0].ID != id {
		t.Fatalf("redelivered id = %d, want the same row %d", second[0].ID, id)
	}
	if second[0].DeliveryCount != 2 {
		t.Fatalf("redelivered delivery count = %d, want 2", second[0].DeliveryCount)
	}
}

// TestWorkQueueCrashThenRedeliverEndToEnd is the §4.10 pre-session-death
// path, end to end: claim the queue row, do not ack, expire the lease,
// re-claim it, and feed the re-claimed message's delivery count to
// ClaimWorkRequest against the sessionless running row the first claim
// created. The re-delivered attempt must claim the row, exactly as a
// redelivered message would — this is the semantic the surrogate-key
// decision exists to protect. The converse assertion is the same one
// TestClaimWorkRequestDuplicateWhileRunning makes: a second, independently
// published message (delivery count 1) must fall through to
// RefusalOwnedElsewhere instead of running twice.
func TestWorkQueueCrashThenRedeliverEndToEnd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueWork(ctx, "req-crash", []byte("p1"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// First delivery: the pool claims the queue row and the idempotency row
	// (creating a sessionless running row), then dies during workspace
	// preparation — before SetWorkRequestSession.
	first, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim returned %d rows, want 1", len(first))
	}
	out, err := s.ClaimWorkRequest(ctx, "req-crash", 1, testNow)
	if err != nil {
		t.Fatalf("claim idempotency row: %v", err)
	}
	if !out.Claimed || out.Found || out.Existing.Status != "" {
		t.Fatalf("first idempotency claim = %+v, want a fresh claim", out)
	}

	// A genuine race — a second, independently published message for the
	// same request_id, its own delivery counter back at 1 — must not run a
	// second session while the first attempt is still live.
	race, err := s.ClaimWorkRequest(ctx, "req-crash", 1, testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("race claim: %v", err)
	}
	if race.Claimed {
		t.Fatal("a duplicate first-delivery publish must not claim the row")
	}
	if race.Refusal != RefusalOwnedElsewhere {
		t.Fatalf("race refusal = %q, want %q", race.Refusal, RefusalOwnedElsewhere)
	}

	// The lease expires: the same queue row redelivers, its own delivery
	// counter now at 2.
	second, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(testLease))
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("re-claim = %+v, want the same row %d", second, first[0].ID)
	}
	if second[0].DeliveryCount != 2 {
		t.Fatalf("re-claimed delivery count = %d, want 2", second[0].DeliveryCount)
	}

	// The redelivered attempt claims the idempotency row: the delivery
	// count > 1 is exactly the "the process that held it died" signal.
	redelivered, err := s.ClaimWorkRequest(ctx, "req-crash", 2, testNow.Add(testLease))
	if err != nil {
		t.Fatalf("claim idempotency row after redelivery: %v", err)
	}
	if !redelivered.Claimed {
		t.Fatalf("redelivered claim = %+v, want Claimed", redelivered)
	}
	if !redelivered.Found || redelivered.Existing.Status != WorkRequestStatusRunning || redelivered.Existing.SessionID != "" {
		t.Fatalf("redelivered claim should have found the sessionless running row, got %+v", redelivered.Existing)
	}
}

// TestWorkQueueConcurrentClaimDisjoint is the flow-control property: N
// concurrent claims with limit 1 against M rows lease M distinct rows, never
// a duplicate, and the N-M extras come back empty-handed. Run with -race —
// the disjointness comes from the single writer goroutine, and the race
// detector is what proves the test itself is sound.
func TestWorkQueueConcurrentClaimDisjoint(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const m = 5
	for i := 0; i < m; i++ {
		if err := s.EnqueueWork(ctx, fmt.Sprintf("req-%d", i), []byte(fmt.Sprintf("payload-%d", i)), testNow); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	const n = 8 // n > m: three claimers come back empty-handed
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[int64]string{}
	empty := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := s.ClaimWork(ctx, 1, testLease, 5, testNow)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if len(rows) == 0 {
				empty++
				return
			}
			if len(rows) != 1 {
				t.Errorf("claim with limit 1 returned %d rows", len(rows))
				return
			}
			if prev, dup := claimed[rows[0].ID]; dup {
				t.Errorf("row %d claimed twice (request %s and %s)", rows[0].ID, prev, rows[0].RequestID)
				return
			}
			claimed[rows[0].ID] = rows[0].RequestID
		}()
	}
	wg.Wait()

	if len(claimed) != m {
		t.Fatalf("claimed %d distinct rows, want %d", len(claimed), m)
	}
	if empty != n-m {
		t.Fatalf("%d claimers came back empty-handed, want %d", empty, n-m)
	}
	for i := 0; i < m; i++ {
		want := fmt.Sprintf("req-%d", i)
		if got := claimed[int64(i+1)]; got != want {
			t.Fatalf("row %d holds request %q, want %q", i+1, got, want)
		}
	}
}

// TestWorkQueueClaimLimitAndOrder checks the claim's LIMIT and its
// (visible_at_ms, id) FIFO ordering: earlier-visible rows first, and among
// rows visible at the same instant, lower id first.
func TestWorkQueueClaimLimitAndOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Three rows visible at t0 (ids 1-3), two visible half a minute later
	// (ids 4-5) — before the first claim's lease expires, so the second
	// claim can see only the late rows. EnqueueWork stamps visible_at from
	// the explicit now, which is what lets the test stagger visibility
	// without sleeping.
	for i := 0; i < 3; i++ {
		if err := s.EnqueueWork(ctx, fmt.Sprintf("early-%d", i), []byte("p"), testNow); err != nil {
			t.Fatalf("enqueue early-%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.EnqueueWork(ctx, fmt.Sprintf("late-%d", i), []byte("p"), testNow.Add(30*time.Second)); err != nil {
			t.Fatalf("enqueue late-%d: %v", i, err)
		}
	}

	got, err := s.ClaimWork(ctx, 2, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("limit-2 claim = %+v, want ids 1,2 in id order", queuedIDs(got))
	}

	got, err = s.ClaimWork(ctx, 3, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("claim = %+v, want the remaining early row id 3", queuedIDs(got))
	}

	// The late rows are invisible until their visible_at arrives; the claim
	// at exactly that instant takes them in id order (the early rows are
	// still leased until t0+60s).
	got, err = s.ClaimWork(ctx, 5, testLease, 5, testNow.Add(30*time.Second))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 5 {
		t.Fatalf("claim = %+v, want ids 4,5 in id order", queuedIDs(got))
	}
}

// TestWorkQueueDeliveryCeiling checks both halves of the ceiling rule: a Nak
// on the attempt that reaches MaxDeliveries deletes the row, and a claim
// never returns a row at or over the ceiling.
func TestWorkQueueDeliveryCeiling(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const max = 2
	const delay = 5 * time.Second

	enqueue := func(requestID string) {
		t.Helper()
		if err := s.EnqueueWork(ctx, requestID, []byte("p"), testNow); err != nil {
			t.Fatalf("enqueue %s: %v", requestID, err)
		}
	}
	claimOne := func(at time.Time, wantCount uint64) int64 {
		t.Helper()
		got, err := s.ClaimWork(ctx, 10, testLease, max, at)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("claim returned %d rows, want 1", len(got))
		}
		if got[0].DeliveryCount != wantCount {
			t.Fatalf("delivery count = %d, want %d", got[0].DeliveryCount, wantCount)
		}
		return got[0].ID
	}

	// A Nak on the attempt that reaches the ceiling deletes the row.
	enqueue("req-ceiling")
	id := claimOne(testNow, 1)
	if err := s.NakWork(ctx, id, delay, max, testNow); err != nil {
		t.Fatalf("nak after delivery 1: %v", err)
	}
	claimOne(testNow.Add(delay), 2)
	if err := s.NakWork(ctx, id, delay, max, testNow.Add(delay)); err != nil {
		t.Fatalf("nak after delivery 2: %v", err)
	}
	got, err := s.ClaimWork(ctx, 10, testLease, max, testNow.Add(2*delay))
	if err != nil {
		t.Fatalf("claim after ceiling: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("claim after ceiling-exhausting Nak returned %d rows, want 0 (the row was deleted)", len(got))
	}

	// A claim never returns a row at or over the ceiling, even when the
	// ceiling was reached without a Nak — the row just sits unclaimable.
	enqueue("req-at-ceiling")
	id = claimOne(testNow, 1)
	if err := s.NakWork(ctx, id, delay, max, testNow); err != nil {
		t.Fatalf("nak: %v", err)
	}
	claimOne(testNow.Add(delay), 2) // now at the ceiling, still leased
	if got, err := s.ClaimWork(ctx, 10, testLease, max, testNow.Add(2*delay)); err != nil {
		t.Fatalf("claim: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("claim returned a row at the ceiling: %+v", got)
	}
}

// TestWorkQueueAckDeletes and its siblings cover the four ways of disposing
// of a claimed row. Ack deletes; a Term is the same delete (exercised at the
// queue.Msg layer, sqlite_test.go); InProgress extends the lease so a claim
// at the original expiry finds nothing.
func TestWorkQueueAckDeletes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueWork(ctx, "req-1", []byte("p"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(got))
	}
	if err := s.AckWork(ctx, got[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow); err != nil {
		t.Fatalf("claim after ack: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("claim after ack returned %d rows, want 0", len(got))
	}
}

func TestWorkQueueInProgressExtendsLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueWork(ctx, "req-1", []byte("p"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(got))
	}
	id := got[0].ID

	// Heartbeat halfway through the original lease, extending it by a full
	// lease from the heartbeat time.
	if err := s.HeartbeatWork(ctx, id, testLease, testNow.Add(30*time.Second)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(61*time.Second)); err != nil {
		t.Fatalf("claim at original expiry: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("claim at original expiry returned %d rows, want 0 (the heartbeat extended the lease)", len(got))
	}
	// At the extended expiry the row redelivers.
	if got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow.Add(90*time.Second)); err != nil {
		t.Fatalf("claim at extended expiry: %v", err)
	} else if len(got) != 1 || got[0].ID != id || got[0].DeliveryCount != 2 {
		t.Fatalf("claim at extended expiry = %+v, want row %d at delivery 2", got, id)
	}
}

// insertWorkRow hand-builds one work_queue row with arbitrary visibility,
// lease, and delivery count, so the stats test does not have to reach each
// state through the public methods. It goes through the writer goroutine
// like every other write.
func insertWorkRow(t *testing.T, s *Store, id int64, requestID string, visibleAt, leaseExpires time.Time, deliveryCount int) {
	t.Helper()
	err := s.submit(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO work_queue (id, request_id, payload, enqueued_at, visible_at_ms, lease_expires_ms, delivery_count)
			VALUES (?, ?, 'p', ?, ?, ?, ?)`,
			id, requestID, testNow.Format(time.RFC3339Nano), visibleAt.UnixMilli(), leaseExpires.UnixMilli(), deliveryCount)
		return err
	})
	if err != nil {
		t.Fatalf("insert row %d: %v", id, err)
	}
}

func TestWorkQueueStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Hand-built rows covering every state, with overlaps:
	//   A: visible now, never leased, first delivery        -> Depth
	//   B: visible later, never leased                      -> Scheduled
	//   C: visible now, leased past now, first delivery     -> InFlight
	//   D: visible now, leased past now, redelivered        -> InFlight, Redelivered
	//   E: visible now, lease expired, redelivered          -> Depth, Redelivered
	//   F: visible later, leased past now, redelivered      -> Scheduled, InFlight, Redelivered
	insertWorkRow(t, s, 1, "req-a", testNow, testNow, 1)
	insertWorkRow(t, s, 2, "req-b", testNow.Add(10*time.Second), testNow, 1)
	insertWorkRow(t, s, 3, "req-c", testNow, testNow.Add(60*time.Second), 1)
	insertWorkRow(t, s, 4, "req-d", testNow, testNow.Add(60*time.Second), 3)
	insertWorkRow(t, s, 5, "req-e", testNow, testNow.Add(-time.Second), 2)
	insertWorkRow(t, s, 6, "req-f", testNow.Add(10*time.Second), testNow.Add(60*time.Second), 2)

	st, err := s.WorkQueueStats(ctx, testNow)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	want := WorkQueueStats{Depth: 2, Scheduled: 2, InFlight: 3, Redelivered: 3}
	if st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}

	// An empty queue counts zero in every bucket.
	if err := s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM work_queue`)
		return err
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	st, err = s.WorkQueueStats(ctx, testNow)
	if err != nil {
		t.Fatalf("stats after clear: %v", err)
	}
	if st != (WorkQueueStats{}) {
		t.Fatalf("stats on an empty queue = %+v, want all zeros", st)
	}
}

// TestWorkQueueClaimable is the read-only gate: true only when a row
// satisfies the claim's visibility and lease predicates, without ever
// leasing anything.
func TestWorkQueueClaimable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if ok, err := s.WorkQueueClaimable(ctx, testNow); err != nil {
		t.Fatalf("claimable on empty queue: %v", err)
	} else if ok {
		t.Fatal("empty queue reported claimable")
	}

	if err := s.EnqueueWork(ctx, "req-1", []byte("p"), testNow); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if ok, err := s.WorkQueueClaimable(ctx, testNow); err != nil {
		t.Fatalf("claimable: %v", err)
	} else if !ok {
		t.Fatal("queued row not reported claimable")
	}

	// A leased row is not claimable until its lease expires.
	got, err := s.ClaimWork(ctx, 10, testLease, 5, testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(got))
	}
	if ok, err := s.WorkQueueClaimable(ctx, testNow.Add(30*time.Second)); err != nil {
		t.Fatalf("claimable mid-lease: %v", err)
	} else if ok {
		t.Fatal("leased row reported claimable")
	}
	if ok, err := s.WorkQueueClaimable(ctx, testNow.Add(testLease)); err != nil {
		t.Fatalf("claimable at lease expiry: %v", err)
	} else if !ok {
		t.Fatal("row with an expired lease not reported claimable")
	}
}

// queuedIDs is a test helper: the ids of a claim result, for readable
// failure messages. (ids is taken by sessions_page_test.go for []Session.)
func queuedIDs(rows []QueuedWork) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
