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
	// Claimed reports whether this call put the row into (fresh) or kept it
	// in (taken over) the running state for the caller to run a session
	// against. When false and Found, Existing.Status is authoritative — a
	// terminal row republishes, a running one is still owned elsewhere.
	Claimed bool
}

// shouldClaim is the idempotency decision docs/DESIGN.md §4.10 requires: a
// row absent gets a fresh run; a terminal row never runs again; a running
// row is taken over only when numDelivered shows JetStream redelivered
// this exact message, meaning whatever process last held it is gone.
// numDelivered from a second, independently published message for the same
// request_id starts back at 1, so a genuine race between two live attempts
// never satisfies this and falls through to "still owned elsewhere"
// instead of running twice.
func shouldClaim(found bool, status string, numDelivered uint64) bool {
	if !found {
		return true
	}
	if status != WorkRequestStatusRunning {
		return false
	}
	return numDelivered > 1
}

// ClaimWorkRequest decides and, if the decision is to run, performs the
// claim in the same transaction on the store's single writer goroutine — so
// two goroutines racing to claim the same request_id can never both
// succeed, without needing a database-level compare-and-swap. numDelivered
// is the JetStream message's redelivery count, the only signal that
// distinguishes an abandoned attempt from a live one (see shouldClaim).
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

		if !shouldClaim(out.Found, out.Existing.Status, numDelivered) {
			out.Claimed = false
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
// was actually making it, ready for the next delivery to link against as
// the abandoned session (docs/DESIGN.md §4.10).
func (s *Store) SetWorkRequestSession(ctx context.Context, requestID, sessionID string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_requests SET session_id = ? WHERE request_id = ?`, sessionID, requestID)
		return err
	})
}

// FinishWorkRequest records requestID's terminal outcome, guarded by
// sessionID still owning the row. The guard matters when a message is
// wrongly believed abandoned — a heartbeat that failed to land before
// AckWait expired while the original attempt was still genuinely running —
// and gets taken over while the original attempt is still alive: without
// it, the original attempt's eventual, correct result could overwrite the
// takeover's. matched is false when the row had already moved on, in which
// case the caller has nothing left to publish.
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
