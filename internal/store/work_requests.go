package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// Version is the row's optimistic-concurrency counter (docs/DATA-API.md
// "Optimistic concurrency"): 1 at creation, incremented by 1 on every
// successful mutation. The HTTP surface returns it in the work-request
// representation and requires it echoed back in If-Match on a mutating
// write, so a write based on a stale read fails with a version conflict
// instead of racing whoever changed the row first.
type WorkRequest struct {
	RequestID     string
	SessionID     string
	Status        string
	Result        json.RawMessage
	ReceivedAt    time.Time
	FinishedAt    *time.Time
	DeliveryCount int
	Version       int
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
				INSERT INTO work_requests (request_id, session_id, status, result, received_at, finished_at, delivery_count, version)
				VALUES (?, NULL, ?, NULL, ?, NULL, ?, 1)`,
				requestID, WorkRequestStatusRunning, now.UTC().Format(time.RFC3339Nano), deliveryCount)
			return err
		}
		_, err = tx.Exec(`
			UPDATE work_requests SET status = ?, result = NULL, finished_at = NULL, delivery_count = ?, version = version + 1
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
// §4.10). It bumps the row's version like every other mutation, so an
// operator read made before the attach goes stale and a later write against
// it 412s.
func (s *Store) SetWorkRequestSession(ctx context.Context, requestID, sessionID string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_requests SET session_id = ?, version = version + 1 WHERE request_id = ?`, sessionID, requestID)
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
			UPDATE work_requests SET status = ?, result = ?, finished_at = ?, version = version + 1
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
		`SELECT `+workRequestColumns+`
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

// ListWorkRequests returns every work_requests row, newest first by
// received_at — the work-request analog of ListSessions' created_at order,
// so the newest claim leads the list. The collection is how an operator
// finds a request whose worker died before its session existed: it has no
// session to be discovered through, only this row.
func (s *Store) ListWorkRequests(ctx context.Context) ([]WorkRequest, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+workRequestColumns+` FROM work_requests ORDER BY received_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkRequest
	for rows.Next() {
		wr, err := scanWorkRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

func selectWorkRequestTx(tx *sql.Tx, requestID string) (WorkRequest, error) {
	row := tx.QueryRow(`
		SELECT `+workRequestColumns+`
		FROM work_requests WHERE request_id = ?`, requestID)
	return scanWorkRequest(row)
}

// workRequestColumns is the column list every work_requests read uses, so a
// column added for one query cannot silently miss another.
const workRequestColumns = `request_id, session_id, status, result, received_at, finished_at, delivery_count, version`

func scanWorkRequest(row interface {
	Scan(dest ...any) error
}) (WorkRequest, error) {
	var wr WorkRequest
	var sessionID, result, finishedAt sql.NullString
	var receivedAt string
	if err := row.Scan(&wr.RequestID, &sessionID, &wr.Status, &result, &receivedAt, &finishedAt, &wr.DeliveryCount, &wr.Version); err != nil {
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

// CloseWorkRequest transitions requestID to a terminal status — the write
// that lets an operator close a request a dead worker left running
// (docs/DATA-API.md phase 3). It carries two guards, both checked inside the
// write transaction so no interleaving write can slip between a check and
// the UPDATE:
//
//   - Optimistic concurrency: wantVersion must equal the row's current
//     version, or VersionConflictError is returned. The version is read from
//     the work-request representation and echoed back in If-Match.
//   - Idleness: a running request whose session is still live is refused with
//     ActiveRequestError. The request's session id is the signal: a live
//     session — one whose most recent event is newer than minIdle — is a
//     request a live pool worker is running right now, which will publish its
//     own terminal result, so closing it underneath the worker is the same
//     class of mistake as closing a live session. A request with no session
//     id has never run — nothing is in flight to protect — and passes.
//
// A running request gets the new status and finished_at = now. One already
// terminal keeps both — a re-close is a version bump and nothing else, so a
// retried write is idempotent and a finished request cannot be relabelled.
// The updated row is returned. The event log and the session row are
// untouched.
func (s *Store) CloseWorkRequest(ctx context.Context, requestID, status string, wantVersion int, now time.Time, minIdle time.Duration) (WorkRequest, error) {
	if status == WorkRequestStatusRunning {
		return WorkRequest{}, fmt.Errorf("store: CloseWorkRequest: %s is not a terminal status", status)
	}
	var out WorkRequest
	err := s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		var sessionID sql.NullString
		var version int
		if err := tx.QueryRow(`SELECT status, session_id, version FROM work_requests WHERE request_id = ?`, requestID).Scan(&storedStatus, &sessionID, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if version != wantVersion {
			return &VersionConflictError{Resource: "work_request " + requestID, Want: wantVersion, Current: version}
		}
		if storedStatus == WorkRequestStatusRunning && sessionID.Valid && sessionID.String != "" {
			last, ok, err := lastEventAt(tx, sessionID.String)
			if err != nil {
				return err
			}
			if ok && now.Sub(last) < minIdle {
				return &ActiveRequestError{RequestID: requestID, SessionID: sessionID.String, LastEventAt: last}
			}
		}
		// Only a running request takes the new status. A row that is already
		// terminal keeps the status it finished with, mirroring
		// CloseSession: this endpoint exists to close a request a dead worker
		// left running, not to relabel a finished one, and a finished
		// request's status is a fact about what happened. A re-close lands as
		// a version bump and nothing else, which keeps a retried PATCH
		// idempotent.
		newStatus := storedStatus
		var fa sql.NullString
		if storedStatus == WorkRequestStatusRunning {
			newStatus = status
			fa = sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := tx.Exec(`UPDATE work_requests SET status = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE request_id = ?`, newStatus, fa, requestID); err != nil {
			return err
		}
		var err2 error
		out, err2 = selectWorkRequestTx(tx, requestID)
		return err2
	})
	if err != nil {
		return WorkRequest{}, err
	}
	return out, nil
}

// DeleteWorkRequest removes requestID's row. It refuses a request whose
// session is still live — the row a live pool worker is using, which will
// publish its own terminal result — with ActiveRequestError, so a delete
// cannot land underneath a run the way nothing may delete a live session.
// A request whose session is idle or absent has no live worker holding it
// and may be deleted directly (docs/DATA-API.md phase 3). wantVersion
// enforces the optimistic-concurrency precondition: it must equal the row's
// current version, or VersionConflictError is returned.
func (s *Store) DeleteWorkRequest(ctx context.Context, requestID string, wantVersion int, now time.Time, minIdle time.Duration) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		var sessionID sql.NullString
		var version int
		if err := tx.QueryRow(`SELECT status, session_id, version FROM work_requests WHERE request_id = ?`, requestID).Scan(&storedStatus, &sessionID, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if version != wantVersion {
			return &VersionConflictError{Resource: "work_request " + requestID, Want: wantVersion, Current: version}
		}
		if storedStatus == WorkRequestStatusRunning && sessionID.Valid && sessionID.String != "" {
			last, ok, err := lastEventAt(tx, sessionID.String)
			if err != nil {
				return err
			}
			if ok && now.Sub(last) < minIdle {
				return &ActiveRequestError{RequestID: requestID, SessionID: sessionID.String, LastEventAt: last}
			}
		}
		_, err := tx.Exec(`DELETE FROM work_requests WHERE request_id = ?`, requestID)
		return err
	})
}
