package queue

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// newTestQueue opens a fresh store in a temp dir and wraps it in a Queue.
// PollInterval is set long (an hour) so the ticker can never be what
// releases a Wait in the wake tests — only the nudge or the context may —
// and Lease/MaxDeliveries are set so the Msg-semantics tests do not depend
// on the zero-value fallbacks.
func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Queue{Store: st, Lease: 60 * time.Second, MaxDeliveries: 5, PollInterval: time.Hour}
}

// testRequest is the request the round-trip test enqueues.
func testRequest() Request {
	return Request{
		RequestID:      "req-1",
		Prompt:         "do the thing",
		Repos:          []Repo{{URL: "https://example.com/org/app.git"}},
		Model:          "deepseek-v4-flash",
		Effort:         "low",
		PermissionMode: "readonly",
		Deny:           []string{"git push"},
		MaxSubTurns:    10,
		DeadlineMS:     60000,
		JobType:        "implementation",
		ParentIsUser:   true,
	}
}

// TestQueueEnqueueClaimRoundTrip is the successor to
// TestPublishRequestLandsOnWorkStream: Enqueue marshals the request, the
// claim hands back the payload byte-identically, and ParseRequest recovers
// an equal Request — the wire shape is defined once, in this package, and
// survives the trip through the table.
func TestQueueEnqueueClaimRoundTrip(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	req := testRequest()
	if err := q.Enqueue(ctx, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(msgs))
	}

	want, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !reflect.DeepEqual(msgs[0].Data(), want) {
		t.Fatalf("payload did not round-trip byte-identically:\n got %q\nwant %q", msgs[0].Data(), want)
	}
	parsed, err := ParseRequest(msgs[0].Data())
	if err != nil {
		t.Fatalf("parse claimed payload: %v", err)
	}
	if !reflect.DeepEqual(parsed, req) {
		t.Fatalf("parsed request = %+v, want the enqueued request %+v", parsed, req)
	}
	if msgs[0].DeliveryCount() != 1 {
		t.Fatalf("delivery count = %d, want 1 on the first delivery", msgs[0].DeliveryCount())
	}
}

// TestQueueClaimEmpty checks the read gate: a claim on an empty queue
// returns nothing (and never takes the writer goroutine — the purpose of
// the WorkQueueClaimable gate, whose contract internal/store tests).
func TestQueueClaimEmpty(t *testing.T) {
	q := newTestQueue(t)
	msgs, err := q.Claim(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim on empty queue: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("empty queue claimed %d messages", len(msgs))
	}
}

// TestQueueMsgAckAndTermDelete drives the two disposal methods that delete
// the row: after an Ack the message is gone, and a Term is the same delete.
func TestQueueMsgAckAndTermDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("ack", func(t *testing.T) {
		q := newTestQueue(t)
		if err := q.Enqueue(ctx, testRequest()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		msgs, err := q.Claim(ctx, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := msgs[0].Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
		again, err := q.Claim(ctx, 10)
		if err != nil {
			t.Fatalf("claim after ack: %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("claim after ack returned %d messages, want 0", len(again))
		}
	})

	t.Run("term", func(t *testing.T) {
		q := newTestQueue(t)
		if err := q.Enqueue(ctx, testRequest()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		msgs, err := q.Claim(ctx, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := msgs[0].Term(); err != nil {
			t.Fatalf("term: %v", err)
		}
		again, err := q.Claim(ctx, 10)
		if err != nil {
			t.Fatalf("claim after term: %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("claim after term returned %d messages, want 0", len(again))
		}
	})
}

// TestQueueMsgNakDeferAndCeiling drives Nak through the Msg seam: it defers
// the row past the delay, and the Nak on the attempt that reaches the
// ceiling deletes it. The delay is tiny so the deferral matures in
// milliseconds of real time rather than the production 5s.
func TestQueueMsgNakDeferAndCeiling(t *testing.T) {
	q := newTestQueue(t)
	q.MaxDeliveries = 2
	ctx := context.Background()

	if err := q.Enqueue(ctx, testRequest()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	const delay = 100 * time.Millisecond

	first, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 || first[0].DeliveryCount() != 1 {
		t.Fatalf("first claim = %d messages, first delivery count %d; want 1 and 1", len(first), first[0].DeliveryCount())
	}
	if err := first[0].Nak(delay); err != nil {
		t.Fatalf("first nak: %v", err)
	}

	// The row is invisible until the delay elapses, then claimable again
	// with delivery count 2.
	second := waitForClaim(t, q, 10)
	if len(second) != 1 || second[0].DeliveryCount() != 2 {
		t.Fatalf("redelivery after Nak delay = %d messages, delivery count %d; want 1 and 2", len(second), second[0].DeliveryCount())
	}

	// The Nak on the attempt at the ceiling deletes the row: once the
	// deferral matures there is nothing left to claim.
	if err := second[0].Nak(delay); err != nil {
		t.Fatalf("second nak: %v", err)
	}
	time.Sleep(2 * delay)
	again, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim after ceiling: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("claim after ceiling-exhausting Nak returned %d messages, want 0", len(again))
	}
}

// TestQueueMsgInProgressExtendsLease checks the heartbeat through the Msg
// seam: after InProgress the row's lease runs from the heartbeat's clock,
// so an immediate re-claim finds nothing — the row is still owned.
func TestQueueMsgInProgressExtendsLease(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, testRequest()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	msgs, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(msgs))
	}
	if err := msgs[0].InProgress(); err != nil {
		t.Fatalf("in-progress: %v", err)
	}
	again, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim after heartbeat: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("claim after heartbeat returned %d messages, want 0 (the lease was extended)", len(again))
	}
}

// TestQueueZeroDefaultsFallBack builds a Queue with every tunable left at
// zero and checks that it still delivers: the lease and poll interval fall
// back to their defaults, and the delivery ceiling to
// DefaultMaxDeliveryAttempts — a zero MaxDeliveries must not produce a
// queue that never delivers or never discards.
func TestQueueZeroDefaultsFallBack(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	q := &Queue{Store: st}
	ctx := context.Background()

	if q.lease() != LeaseDuration {
		t.Fatalf("zero Lease fell back to %v, want LeaseDuration %v", q.lease(), LeaseDuration)
	}
	if q.maxDeliveries() != DefaultMaxDeliveryAttempts {
		t.Fatalf("zero MaxDeliveries fell back to %d, want %d", q.maxDeliveries(), DefaultMaxDeliveryAttempts)
	}
	if q.pollInterval() != time.Second {
		t.Fatalf("zero PollInterval fell back to %v, want 1s", q.pollInterval())
	}

	if err := q.Enqueue(ctx, testRequest()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	msgs, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("defaults queue claimed %d messages, want 1", len(msgs))
	}
	if msgs[0].DeliveryCount() != 1 {
		t.Fatalf("delivery count = %d, want 1", msgs[0].DeliveryCount())
	}
	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.InFlight != 1 || stats.Depth != 0 {
		t.Fatalf("stats after one claim = %+v, want 1 in flight and nothing claimable", stats)
	}
}

// TestQueueWakeReleasesWait is the nudge: a Wake releases a Wait that is
// blocked with nothing else to return on. The wake channel is buffered, so
// the send lands whether or not Wait has started selecting. PollInterval is
// an hour in these tests, so the ticker cannot be what releases the Wait —
// only the nudge (or the context) may.
func TestQueueWakeReleasesWait(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		q.Wait(ctx)
		close(done)
	}()
	// Let the goroutine block on Wait before nudging, so the test is about
	// waking a blocked Wait rather than racing an unstarted one.
	time.Sleep(50 * time.Millisecond)

	q.Wake()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Wake")
	}
}

// TestQueueWakeBufferFullDoesNotBlock checks the non-blocking send: with one
// nudge already pending and nobody consuming, a second Wake returns
// immediately instead of blocking, and the pending nudge releases the next
// Wait.
func TestQueueWakeBufferFullDoesNotBlock(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	q.Wake() // fills the buffered channel

	// A second Wake with the buffer full must not block. Run it in a
	// goroutine so a regression hangs the test rather than the suite.
	woke := make(chan struct{})
	go func() {
		q.Wake()
		close(woke)
	}()
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("second Wake blocked on a full buffer")
	}

	// The first nudge is still pending and releases the next Wait.
	done := make(chan struct{})
	go func() {
		q.Wait(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return on the pending nudge")
	}
}

// TestQueueWaitReturnsOnContextDone checks the third Wait exit: a cancelled
// context releases it even with no nudge and no enqueue.
func TestQueueWaitReturnsOnContextDone(t *testing.T) {
	q := newTestQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		q.Wait(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return on a cancelled context")
	}
}

// TestQueueEnqueueWakesWait is the producer's nudge end to end: a Wait
// blocked on an empty queue returns once Enqueue lands.
func TestQueueEnqueueWakesWait(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		q.Wait(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)

	if err := q.Enqueue(ctx, testRequest()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Enqueue woke it")
	}

	msgs, err := q.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(msgs))
	}
}

// TestQueueStats exercises Stats through the queue layer: a claimed message
// moves the row from Depth to InFlight.
func TestQueueStats(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, testRequest()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Depth != 1 || stats.Scheduled != 0 || stats.InFlight != 0 || stats.Redelivered != 0 {
		t.Fatalf("stats after enqueue = %+v, want depth 1", stats)
	}

	if _, err := q.Claim(ctx, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stats, err = q.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Depth != 0 || stats.InFlight != 1 {
		t.Fatalf("stats after claim = %+v, want 1 in flight", stats)
	}
}

// waitForClaim polls Claim until it returns at least one message, failing
// the test if the queue stays empty past the deadline. The queue layer runs
// on the real clock (unlike the store, whose methods take an explicit now),
// so a Nak-deferred row needs real time to mature; the polling loop is that
// wait, bounded.
func waitForClaim(t *testing.T, q *Queue, limit int) []Msg {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := q.Claim(context.Background(), limit)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(msgs) > 0 {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatal("claim stayed empty past the deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
