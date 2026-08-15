package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Source is the subset of *Queue the worker pool consumes: claim up to
// limit rows, wait for a nudge or the poll interval, and nudge back when a
// slot frees. Declared here and satisfied by *Queue, so the pool depends on
// the narrow interface rather than the concrete queue
// (docs/QUEUE-MIGRATION-PLAN.md §1.5).
type Source interface {
	Claim(ctx context.Context, limit int) ([]Msg, error)
	Wait(ctx context.Context)
	Wake()
}

// Lease and delivery-ceiling defaults, fixed by docs/DESIGN.md §4.10.
const (
	// LeaseDuration is how long a claim holds a row before it becomes
	// claimable again, 60s (docs/DESIGN.md §4.10); the pool's InProgress
	// heartbeat interval is sized well under it so a multi-minute run never
	// trips it.
	LeaseDuration = 60 * time.Second

	// DefaultMaxDeliveryAttempts is the delivery ceiling when a caller passes
	// zero. Production resolves worker.max_delivery_attempts from the
	// settings registry and passes it in.
	//
	// A ceiling has to exist. Once a request carries a session id it is
	// single-use and a redelivery fails it rather than re-runs it (§4.10), so
	// the ceiling's job is the requests that die *before* their session
	// exists — the only ones redelivery still claims. One of those that keeps
	// dying during preparation would otherwise be redelivered forever, each
	// attempt burning a pool slot; the ceiling caps that.
	DefaultMaxDeliveryAttempts = 5
)

// Queue is the store-backed work queue, the WORK stream's successor
// (docs/QUEUE-MIGRATION-PLAN.md §1.4). The pool claims rows and disposes of
// them through queue.Msg; producers enqueue through Enqueue. The wake
// channel lives here, not in the store: the store must not grow a
// notification concern, and the queue is the one place that knows both
// sides of the nudge.
type Queue struct {
	Store *store.Store

	// Lease is how long a claim holds a row before it becomes claimable
	// again (LeaseDuration, 60s). Zero means the default.
	Lease time.Duration

	// MaxDeliveries is the delivery ceiling: a Nak on the attempt that
	// reaches it discards the row, and a claim never returns a row at or
	// over it. Zero means DefaultMaxDeliveryAttempts.
	MaxDeliveries int

	// PollInterval is the backstop between claims when nothing nudges the
	// queue: a lease expiring or a Nak delay maturing with no enqueue or
	// slot release in between is only noticed by the next poll. Zero means
	// 1s. The nudge covers the latency-sensitive cases (Enqueue, slot
	// release), so a uniform PollInterval delay on run start is the symptom
	// of a broken nudge, not a tuning problem.
	PollInterval time.Duration

	wakeOnce sync.Once
	wake     chan struct{} // buffered 1
}

// Stats is what the queue health endpoint reports, the queue package's view
// of store.WorkQueueStats.
type Stats struct {
	Depth       int // claimable now: visible and unleased (or lease expired)
	Scheduled   int // visible_at > now — the Nak-delayed backlog
	InFlight    int // leased and not expired
	Redelivered int // delivery_count > 1
}

func (q *Queue) lease() time.Duration {
	if q.Lease > 0 {
		return q.Lease
	}
	return LeaseDuration
}

func (q *Queue) maxDeliveries() int {
	if q.MaxDeliveries > 0 {
		return q.MaxDeliveries
	}
	return DefaultMaxDeliveryAttempts
}

func (q *Queue) pollInterval() time.Duration {
	if q.PollInterval > 0 {
		return q.PollInterval
	}
	return time.Second
}

// wakeCh lazily creates the wake channel. Queue is built as a struct
// literal (cmd/harness/serve.go constructs it that way in phase 5), so the
// channel cannot be created in a constructor; the lazy init keeps a
// literally-built queue's nudge working.
func (q *Queue) wakeCh() chan struct{} {
	q.wakeOnce.Do(func() { q.wake = make(chan struct{}, 1) })
	return q.wake
}

// Enqueue marshals req and inserts it into the queue, then wakes a waiting
// claim loop. It is the successor to PublishRequest and stays the one
// marshal-and-enqueue path every producer uses, so the wire shape is defined
// once, in the package that owns it.
func (q *Queue) Enqueue(ctx context.Context, req Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("queue: encode request: %w", err)
	}
	if err := q.Store.EnqueueWork(ctx, req.RequestID, data, time.Now().UTC()); err != nil {
		return fmt.Errorf("queue: enqueue request %s: %w", req.RequestID, err)
	}
	q.Wake()
	return nil
}

// Claim leases up to limit rows and returns them as queue.Msg values. It
// gates on a read-only existence check before taking the writer goroutine:
// without that gate an idle harness would run one write transaction per
// second forever and grow the WAL for nothing (docs/QUEUE-MIGRATION-PLAN.md
// §10).
func (q *Queue) Claim(ctx context.Context, limit int) ([]Msg, error) {
	if limit < 1 {
		return nil, nil
	}
	now := time.Now().UTC()
	claimable, err := q.Store.WorkQueueClaimable(ctx, now)
	if err != nil {
		return nil, err
	}
	if !claimable {
		return nil, nil
	}
	rows, err := q.Store.ClaimWork(ctx, limit, q.lease(), q.maxDeliveries(), now)
	if err != nil {
		return nil, err
	}
	msgs := make([]Msg, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, sqliteMsg{q: q, row: row})
	}
	return msgs, nil
}

// Wake nudges a blocked Wait. It is a non-blocking send onto a buffered
// channel, so a second Wake while one is already pending is a no-op rather
// than a block, and a Wake that races a Wait that is about to return is
// harmless — the buffered nudge is simply consumed by the next Wait.
func (q *Queue) Wake() {
	select {
	case q.wakeCh() <- struct{}{}:
	default:
	}
}

// Wait blocks until the queue is nudged (a row was enqueued or a slot was
// released), the poll interval elapses (a lease expired or a Nak delay
// matured with no nudge), or ctx is done.
func (q *Queue) Wait(ctx context.Context) {
	ticker := time.NewTicker(q.pollInterval())
	defer ticker.Stop()
	select {
	case <-q.wakeCh():
	case <-ticker.C:
	case <-ctx.Done():
	}
}

// Stats reports the queue's four counters, converted from the store's.
func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	st, err := q.Store.WorkQueueStats(ctx, time.Now().UTC())
	if err != nil {
		return Stats{}, err
	}
	return Stats{
		Depth:       st.Depth,
		Scheduled:   st.Scheduled,
		InFlight:    st.InFlight,
		Redelivered: st.Redelivered,
	}, nil
}

// sqliteMsg is queue.Msg over one claimed work_queue row. Disposal methods
// call the store with time.Now().UTC() — the queue is the layer that owns
// the clock; the store's explicit-now convention is what lets its tests run
// without sleeping.
type sqliteMsg struct {
	q   *Queue
	row store.QueuedWork
}

func (m sqliteMsg) Data() []byte {
	return m.row.Payload
}

func (m sqliteMsg) DeliveryCount() uint64 {
	return m.row.DeliveryCount
}

func (m sqliteMsg) Ack() error {
	return m.q.Store.AckWork(context.Background(), m.row.ID)
}

// Nak defers the row by delay, or — on the delivery that reaches the
// ceiling — discards it, exactly as the store's delivery ceiling discards a
// row that has been redelivered too often.
func (m sqliteMsg) Nak(delay time.Duration) error {
	return m.q.Store.NakWork(context.Background(), m.row.ID, delay, m.q.maxDeliveries(), time.Now().UTC())
}

// Term discards the row. A Term'd row is deleted, not dead-lettered — the
// work_requests row already carries the terminal result for every path that
// produces one (docs/QUEUE-MIGRATION-PLAN.md §11).
func (m sqliteMsg) Term() error {
	return m.q.Store.AckWork(context.Background(), m.row.ID)
}

func (m sqliteMsg) InProgress() error {
	return m.q.Store.HeartbeatWork(context.Background(), m.row.ID, m.q.lease(), time.Now().UTC())
}
