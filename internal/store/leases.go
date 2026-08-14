package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// WorkspaceLease is one row of the workspace_leases table: a workspace held
// by a session. heartbeat_at is the liveness signal the HTTP release
// endpoint judges (docs/DATA-API.md): a live session heartbeats its
// lease, so a recent heartbeat means the workspace is in use right now.
// Version is the row's optimistic-concurrency counter (docs/DATA-API.md
// "Optimistic concurrency"), carried like every other row resource so the
// release write can be fenced with If-Match.
type WorkspaceLease struct {
	Workspace   string
	SessionID   string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	Version     int
}

// leaseColumns is the column list every workspace_leases read uses, so a
// column added for one query cannot silently miss another.
const leaseColumns = `workspace, session_id, acquired_at, heartbeat_at, version`

func scanLease(row interface {
	Scan(dest ...any) error
}) (WorkspaceLease, error) {
	var l WorkspaceLease
	var acquiredAt, heartbeatAt string
	if err := row.Scan(&l.Workspace, &l.SessionID, &acquiredAt, &heartbeatAt, &l.Version); err != nil {
		return WorkspaceLease{}, err
	}
	var err error
	l.AcquiredAt, err = time.Parse(time.RFC3339Nano, acquiredAt)
	if err != nil {
		return WorkspaceLease{}, err
	}
	l.HeartbeatAt, err = time.Parse(time.RFC3339Nano, heartbeatAt)
	if err != nil {
		return WorkspaceLease{}, err
	}
	return l, nil
}

// ListWorkspaceLeases returns every lease, keyed by workspace.
func (s *Store) ListWorkspaceLeases(ctx context.Context) ([]WorkspaceLease, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+leaseColumns+` FROM workspace_leases ORDER BY workspace ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceLease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteWorkspaceLease releases workspace's lease — the write that lets an
// operator release a lease a dead worker left stranded (docs/DATA-API.md).
// It carries two guards, both checked inside the write transaction
// so no interleaving write can slip between a check and the DELETE:
//
//   - Optimistic concurrency: wantVersion must equal the row's current
//     version, or VersionConflictError is returned. The version is read from
//     the lease representation and echoed back in If-Match.
//   - Idleness: a lease whose heartbeat is newer than minIdle is held by a
//     live session — one that heartbeats its lease — and is refused with
//     ActiveLeaseError, the lease analog of the session endpoints' active
//     refusal.
func (s *Store) DeleteWorkspaceLease(ctx context.Context, workspace string, wantVersion int, now time.Time, minIdle time.Duration) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var sessionID, heartbeatAt string
		var version int
		if err := tx.QueryRow(`SELECT session_id, heartbeat_at, version FROM workspace_leases WHERE workspace = ?`, workspace).Scan(&sessionID, &heartbeatAt, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if version != wantVersion {
			return &VersionConflictError{Resource: "workspace_lease " + workspace, Want: wantVersion, Current: version}
		}
		last, err := time.Parse(time.RFC3339Nano, heartbeatAt)
		if err != nil {
			return err
		}
		if now.Sub(last) < minIdle {
			return &ActiveLeaseError{Workspace: workspace, SessionID: sessionID, LastHeartbeatAt: last}
		}
		_, err = tx.Exec(`DELETE FROM workspace_leases WHERE workspace = ?`, workspace)
		return err
	})
}

// AcquireWorkspaceLease claims workspace for sessionID. It fails fast with
// ErrWorkspaceLeased if another session already holds it; no caller waits
// (docs/DESIGN.md §4.5).
func (s *Store) AcquireWorkspaceLease(ctx context.Context, workspace, sessionID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.submit(ctx, func(tx *sql.Tx) error {
		var holder string
		err := tx.QueryRow(`SELECT session_id FROM workspace_leases WHERE workspace = ?`, workspace).Scan(&holder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.Exec(`INSERT INTO workspace_leases (workspace, session_id, acquired_at, heartbeat_at, version) VALUES (?, ?, ?, ?, 1)`,
				workspace, sessionID, now, now)
			return err
		case err != nil:
			return err
		case holder == sessionID:
			return nil
		default:
			return ErrWorkspaceLeased
		}
	})
}

// ReleaseWorkspaceLease drops the lease if sessionID holds it. Releasing an
// unheld or differently-held lease is not an error.
func (s *Store) ReleaseWorkspaceLease(ctx context.Context, workspace, sessionID string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM workspace_leases WHERE workspace = ? AND session_id = ?`, workspace, sessionID)
		return err
	})
}

// AcquireWorkspaceLeaseWait retries AcquireWorkspaceLease on pollInterval
// until it succeeds or ctx is done, which is what makes wait-or-fail a
// per-request choice (docs/DESIGN.md §4.5): the caller bounds ctx by the
// request's own deadline, so a request with little time left effectively
// fails fast and one with a long deadline effectively waits.
func (s *Store) AcquireWorkspaceLeaseWait(ctx context.Context, workspace, sessionID string, pollInterval time.Duration) error {
	for {
		err := s.AcquireWorkspaceLease(ctx, workspace, sessionID)
		if err == nil || !errors.Is(err, ErrWorkspaceLeased) {
			return err
		}
		t := time.NewTimer(pollInterval)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}
