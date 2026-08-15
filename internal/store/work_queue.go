package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueuedWork is one row of the work_queue table as it comes back from a
// claim: the surrogate id the lease and disposal operations key on, the
// request id the idempotency row keys on, the raw request payload, and the
// delivery count after this claim's increment (1 on the first delivery).
type QueuedWork struct {
	ID            int64
	RequestID     string
	Payload       []byte
	DeliveryCount uint64
}

// EnqueueWork inserts one row for requestID, visible immediately. One
// enqueue is one row with its own independent delivery counter — the
// property store.shouldClaim depends on (see the work_queue comment in
// schema.go) — so a second enqueue of the same request_id is a second,
// independent message, exactly as two publishes to the WORK stream were.
func (s *Store) EnqueueWork(ctx context.Context, requestID string, payload []byte, now time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO work_queue (request_id, payload, enqueued_at, visible_at_ms)
			VALUES (?, ?, ?, ?)`,
			requestID, string(payload), now.UTC().Format(time.RFC3339Nano), now.UnixMilli())
		return err
	})
}

// ClaimWork leases up to limit rows that are visible, unleased (or whose
// lease has expired), and under the delivery ceiling, and returns them with
// their incremented delivery counts. It is one statement in one transaction
// on the store's single writer goroutine, so two concurrent claims can never
// lease the same row — the same argument ClaimWorkRequest's doc comment
// makes for the idempotency row. Rows come back in (visible_at_ms, id)
// order, matching the WORK stream's FIFO redelivery.
//
// The statement must be issued through Query, not Exec: Exec runs the UPDATE
// and silently discards the RETURNING rows, so the claim would succeed and
// hand the caller nothing. lease_expires_ms = 0 on an unleased row means the
// single predicate "lease_expires_ms <= now" covers both "never leased" and
// "lease expired".
func (s *Store) ClaimWork(ctx context.Context, limit int, lease time.Duration, maxDeliveries int, now time.Time) ([]QueuedWork, error) {
	if limit < 1 {
		return nil, nil
	}
	nowMS := now.UnixMilli()
	var out []QueuedWork
	err := s.submit(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`
			UPDATE work_queue
			   SET lease_expires_ms = ?,
			       delivery_count   = delivery_count + 1
			 WHERE id IN (SELECT id FROM work_queue
			               WHERE visible_at_ms    <= ?
			                 AND lease_expires_ms <= ?
			                 AND delivery_count   <  ?
			               ORDER BY visible_at_ms ASC, id ASC
			               LIMIT ?)
			RETURNING id, request_id, payload, delivery_count`,
			nowMS+lease.Milliseconds(), nowMS, nowMS, maxDeliveries, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var qw QueuedWork
			var payload string
			if err := rows.Scan(&qw.ID, &qw.RequestID, &payload, &qw.DeliveryCount); err != nil {
				return err
			}
			qw.Payload = []byte(payload)
			out = append(out, qw)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HeartbeatWork extends id's lease to now + lease, the InProgress of the
// work queue. The UPDATE is unconditional: a lease on a row that is no
// longer claimable is harmless (the row is gone or already re-leased, and
// the next claim of it starts from whatever state it is in).
func (s *Store) HeartbeatWork(ctx context.Context, id int64, lease time.Duration, now time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_queue SET lease_expires_ms = ? WHERE id = ?`, now.UnixMilli()+lease.Milliseconds(), id)
		return err
	})
}

// AckWork deletes id's row — the ack of the work queue. A Term is the same
// delete; the two stay separate methods on queue.Msg for readability, and a
// future dead-letter table would make them differ.
func (s *Store) AckWork(ctx context.Context, id int64) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM work_queue WHERE id = ?`, id)
		return err
	})
}

// NakWork defers id's row to now + delay, releasing its lease, or deletes it
// when this delivery has reached the ceiling — delivery_count was already
// incremented at claim time, so "at the ceiling" means the count this
// delivery is running under is >= maxDeliveries. That is the delivery
// ceiling docs/DESIGN.md §4.10 asks for: at the ceiling the row stops being
// redelivered and is discarded, the queue's replacement for MaxDeliver. The
// delivery_count < :max clause in ClaimWork is a second guard on the same
// rule.
func (s *Store) NakWork(ctx context.Context, id int64, delay time.Duration, maxDeliveries int, now time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM work_queue WHERE id = ? AND delivery_count >= ?`, id, maxDeliveries); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE work_queue SET visible_at_ms = ?, lease_expires_ms = 0 WHERE id = ?`, now.UnixMilli()+delay.Milliseconds(), id)
		return err
	})
}

// WorkQueueStats is what the queue health endpoint reports: how many rows
// are claimable now, how many are Nak-delayed into the future, how many are
// leased to a live worker, and how many have been redelivered at least once.
type WorkQueueStats struct {
	Depth       int
	Scheduled   int
	InFlight    int
	Redelivered int
}

// WorkQueueClaimable reports whether any row would satisfy the claim's
// visibility and lease predicates at now. It is a read-only existence check
// on the read pool — deliberately not a submit — so an idle harness that
// polls it every second never takes the writer goroutine and never grows the
// WAL for nothing; the real claim, with the delivery-ceiling guard, runs
// only when this says there is something to take. It does not check the
// ceiling, so it may answer true for a queue whose only claimable rows are
// exhausted — the claim then returns nothing, which is correct, just one
// wasted writer turn.
func (s *Store) WorkQueueClaimable(ctx context.Context, now time.Time) (bool, error) {
	var ok bool
	err := s.readDB.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM work_queue
			 WHERE visible_at_ms    <= ?
			   AND lease_expires_ms <= ?)`,
		now.UnixMilli(), now.UnixMilli()).Scan(&ok)
	return ok, err
}

// WorkQueueStats counts the four states a queue row can be in at now, one
// conditional pass over the table: Depth is claimable now (visible, and
// never leased or lease expired — the claim's first two predicates),
// Scheduled is the Nak-delayed backlog, InFlight is leased to a live worker,
// and Redelivered counts rows whose delivery_count is past the first. A row
// can count in more than one bucket (a redelivered row that is also
// scheduled, say); the four are separate facts, not a partition.
func (s *Store) WorkQueueStats(ctx context.Context, now time.Time) (WorkQueueStats, error) {
	nowMS := now.UnixMilli()
	var st WorkQueueStats
	err := s.readDB.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN visible_at_ms    <= ? AND lease_expires_ms <= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN visible_at_ms    >  ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN lease_expires_ms >  ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN delivery_count   >  1 THEN 1 ELSE 0 END), 0)
		FROM work_queue`,
		nowMS, nowMS, nowMS, nowMS).Scan(&st.Depth, &st.Scheduled, &st.InFlight, &st.Redelivered)
	if err != nil {
		return WorkQueueStats{}, fmt.Errorf("store: work queue stats: %w", err)
	}
	return st, nil
}
