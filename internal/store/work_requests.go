package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// WorkRequestStatusRunning is the status a work_requests row holds from the
// moment a worker claims it until it reaches one of the terminal statuses
// internal/queue.Result defines (ok, failed, denied, timeout, cancelled).
// The store treats every other status as terminal without needing to know
// the queue package's vocabulary.
const WorkRequestStatusRunning = "running"

// WorkRequest is one row of the idempotency table docs/DESIGN.md §4.10
// describes: "Idempotency is a row, not a convention." request_id is the
// primary key a redelivered or duplicated work request is checked against.
type WorkRequest struct {
	RequestID     string
	SessionID     string
	Status        string
	Result        json.RawMessage
	ReceivedAt    time.Time
	FinishedAt    *time.Time
	DeliveryCount int
}

// ClaimOutcome is what ClaimWorkRequest found and did, in one atomic step.
type ClaimOutcome struct {
	// Existing is the row as it was before this call, valid when Found.
	Existing WorkRequest
	Found    bool
	// Claimed reports whether this call put the row into the running state
	// for the caller to run a session against. When false and Found, Refusal
	// says why: a terminal row republishes its stored result, a spent one
	// (session id set) never runs again, and a contended one is still owned
	// by a live attempt elsewhere.
	Claimed bool
	// Refusal is why the row was not claimed, set when Claimed is false and
	// Found is true. It is the discriminator the worker branches on — the
	// three reasons take three different paths — so it is named on the
	// outcome rather than left for the caller to re-derive from the row.
	Refusal ClaimRefusal
}

// ClaimRefusal names why ClaimWorkRequest refused to claim a found row.
type ClaimRefusal string

const (
	// RefusalTerminal: the row holds a terminal status. Its stored result is
	// republished and the message acked without running anything.
	RefusalTerminal ClaimRefusal = "terminal"
	// RefusalSpent: the row already carries a session id. A work request is
	// single-use once an attempt actually started — the id is attached as
	// soon as the session row exists, before the run does anything
	// side-effecting — and an agent run is not idempotent, so it must never
	// run again, whatever its status. The worker closes the abandoned
	// session, publishes a failed result, and terminates the message.
	RefusalSpent ClaimRefusal = "spent"
	// RefusalOwnedElsewhere: the row is running and sessionless, so this
	// message is a duplicate publish racing a live attempt that has not
	// attached its session yet. The message waits for the attempt's
	// resolution rather than running a second session.
	RefusalOwnedElsewhere ClaimRefusal = "owned_elsewhere"
)

// shouldClaim is the idempotency decision docs/DESIGN.md §4.10 requires: a
// row absent gets a fresh run; a found row is refused when it carries a
// session id — a work request is single-use once an attempt actually
// started, and an agent run is not idempotent — and when it is terminal,
// which republishes its stored result instead of running. A running,
// sessionless row is claimed only when numDelivered shows JetStream
// redelivered this exact message, meaning the process that held it died
// during workspace preparation and nothing happened that matters.
// numDelivered from a second, independently published message for the same
// request_id starts back at 1, so a genuine race between two live attempts
// never satisfies this and falls through to "still owned elsewhere" instead
// of running twice.
func shouldClaim(found bool, status, sessionID string, numDelivered uint64) bool {
	if !found {
		return true
	}
	if sessionID != "" {
		return false
	}
	if status != WorkRequestStatusRunning {
		return false
	}
	return numDelivered > 1
}

// refusalFor names why a found row was refused. A terminal row wins over a
// spent one: a request that finished carries both a terminal status and the
// session that ran it, and its stored result is republished, not re-failed.
func refusalFor(row WorkRequest) ClaimRefusal {
	switch {
	case row.Status != WorkRequestStatusRunning:
		return RefusalTerminal
	case row.SessionID != "":
		return RefusalSpent
	default:
		return RefusalOwnedElsewhere
	}
}

// ClaimWorkRequest decides and, if the decision is to run, performs the
// claim in the same transaction on the store's single writer goroutine — so
// two goroutines racing to claim the same request_id can never both
// succeed, without needing a database-level compare-and-swap. numDelivered
// is the JetStream message's redelivery count, the only signal that
// distinguishes an attempt that died during workspace preparation from a
// live one (see shouldClaim).
func (s *Store) ClaimWorkRequest(ctx context.Context, requestID string, numDelivered uint64, now time.Time) (ClaimOutcome, error) {
	var out ClaimOutcome
	deliveryCount := int(numDelivered)
	if deliveryCount < 1 {
		deliveryCount = 1
	}

	err := s.submit(ctx, func(tx *sql.Tx) error {
		row, err := selectWorkRequestTx(tx, requestID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out.Found = false
		case err != nil:
			return err
		default:
			out.Found = true
			out.Existing = row
		}

		if !shouldClaim(out.Found, out.Existing.Status, out.Existing.SessionID, numDelivered) {
			out.Claimed = false
			out.Refusal = refusalFor(out.Existing)
			return nil
		}
		out.Claimed = true

		if !out.Found {
			_, err := tx.Exec(`
				INSERT INTO work_requests (request_id, session_id, status, result, received_at, finished_at, delivery_count)
				VALUES (?, NULL, ?, NULL, ?, NULL, ?)`,
				requestID, WorkRequestStatusRunning, now.UTC().Format(time.RFC3339Nano), deliveryCount)
			return err
		}
		_, err = tx.Exec(`
			UPDATE work_requests SET status = ?, result = NULL, finished_at = NULL, delivery_count = ?
			WHERE request_id = ?`,
			WorkRequestStatusRunning, deliveryCount, requestID)
		return err
	})
	if err != nil {
		return ClaimOutcome{}, err
	}
	return out, nil
}

// SetWorkRequestSession attaches sessionID to requestID once the session
// row exists. Called as soon as the id is known rather than after the run
// finishes, so a crash mid-run leaves the row pointing at the attempt that
// was actually making it — which is what makes a work request single-use: a
// redelivery sees the session id and refuses to run again (docs/DESIGN.md
// §4.10).
func (s *Store) SetWorkRequestSession(ctx context.Context, requestID, sessionID string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_requests SET session_id = ? WHERE request_id = ?`, sessionID, requestID)
		return err
	})
}

// FinishWorkRequest records requestID's terminal outcome, guarded by
// sessionID still owning the row. The guard keeps a finish from a session
// the row no longer points at — an empty id from the exhausted-delivery or
// validation path, or a stale id from an attempt superseded by a later
// write — from overwriting the outcome the row now records. matched is
// false when the row had already moved on, in which case the caller has
// nothing left to publish.
func (s *Store) FinishWorkRequest(ctx context.Context, requestID, sessionID, status string, result json.RawMessage, finishedAt time.Time) (bool, error) {
	var matched bool
	err := s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE work_requests SET status = ?, result = ?, finished_at = ?
			WHERE request_id = ? AND session_id = ?`,
			status, string(result), finishedAt.UTC().Format(time.RFC3339Nano), requestID, sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		matched = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// GetWorkRequest reads one work_requests row by id, for republishing a
// terminal request's stored result.
func (s *Store) GetWorkRequest(ctx context.Context, requestID string) (WorkRequest, error) {
	row := s.readDB.QueryRowContext(ctx,
		`SELECT request_id, session_id, status, result, received_at, finished_at, delivery_count
		 FROM work_requests WHERE request_id = ?`, requestID)
	wr, err := scanWorkRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkRequest{}, ErrNotFound
	}
	if err != nil {
		return WorkRequest{}, err
	}
	return wr, nil
}

func selectWorkRequestTx(tx *sql.Tx, requestID string) (WorkRequest, error) {
	row := tx.QueryRow(`
		SELECT request_id, session_id, status, result, received_at, finished_at, delivery_count
		FROM work_requests WHERE request_id = ?`, requestID)
	return scanWorkRequest(row)
}

func scanWorkRequest(row interface {
	Scan(dest ...any) error
}) (WorkRequest, error) {
	var wr WorkRequest
	var sessionID, result, finishedAt sql.NullString
	var receivedAt string
	if err := row.Scan(&wr.RequestID, &sessionID, &wr.Status, &result, &receivedAt, &finishedAt, &wr.DeliveryCount); err != nil {
		return WorkRequest{}, err
	}
	wr.SessionID = sessionID.String
	if result.Valid {
		wr.Result = json.RawMessage(result.String)
	}
	t, err := time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return WorkRequest{}, err
	}
	wr.ReceivedAt = t
	if finishedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAt.String)
		if err != nil {
			return WorkRequest{}, err
		}
		wr.FinishedAt = &t
	}
	return wr, nil
}
