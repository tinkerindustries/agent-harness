// Package store is the harness's SQLite persistence: sessions, their event
// logs, work requests, and workspace leases (docs/DESIGN.md §4.8).
//
// All writes funnel through one goroutine fed by a channel, so SQLITE_BUSY
// never arises from our own concurrency (docs/DESIGN.md §4.5). Reads use a
// separate connection pool and never touch the write path. WAL mode lets
// those reads proceed concurrently with the writer.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	_ "modernc.org/sqlite"
)

// Session statuses.
const (
	StatusRunning   = "running"
	StatusOK        = "ok"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusMaxTurns  = "max_turns"
	StatusCancelled = "cancelled"
	StatusCompacted = "compacted"
)

// ErrClosed is returned by Store methods called after Close.
var ErrClosed = errors.New("store: closed")

// ErrWorkspaceLeased is returned when a workspace is already leased to a
// different session.
var ErrWorkspaceLeased = errors.New("store: workspace already leased")

// ErrNotFound is returned when a lookup by id finds no row.
var ErrNotFound = errors.New("store: not found")

// Session is the frozen metadata row for one agent session. SystemPrompt and
// ToolSchema are rendered once at creation and never regenerated from the
// running binary, so a harness upgrade cannot change the prefix of a
// resumable session (docs/CACHE.md).
type Session struct {
	ID              string
	ParentID        string
	JobType         string
	ParentAgentType string
	ParentAgentID   string
	Model           string
	Effort          string
	Thinking        bool
	Workspace       string
	PermissionMode  string
	DenyPatterns    []string
	SystemPrompt    string
	ToolSchema      json.RawMessage
	ResultSchema    json.RawMessage
	Status          string
	CreatedAt       time.Time
	FinishedAt      *time.Time
}

// Event is one row of a session's append-only log, keyed by (session_id,
// seq). Payload's shape depends on Kind; see events.go. The JSON tags are
// load-bearing: the HTTP layer marshals Event directly onto the wire, for
// both the paged /events endpoint and the SSE stream's data field.
type Event struct {
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	Kind      EventKind       `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// EventInput is one event to append. Payload is marshalled to JSON by
// AppendEvents.
type EventInput struct {
	Kind    EventKind
	Payload any
}

// Store owns the SQLite handle pair and the writer goroutine.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
	jobs    chan job
	stopped chan struct{}
}

type job struct {
	fn   func(*sql.Tx) error
	resp chan error
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id                TEXT PRIMARY KEY,
	parent_id         TEXT,
	job_type          TEXT NOT NULL DEFAULT 'implementation',
	parent_agent_type TEXT NOT NULL DEFAULT '',
	parent_agent_id   TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL,
	effort            TEXT NOT NULL,
	thinking          INTEGER NOT NULL,
	workspace         TEXT NOT NULL,
	permission_mode   TEXT NOT NULL,
	deny_patterns     TEXT NOT NULL DEFAULT '[]',
	system_prompt     TEXT NOT NULL,
	tool_schema       TEXT NOT NULL,
	result_schema     TEXT,
	status            TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	finished_at       TEXT
);

CREATE TABLE IF NOT EXISTS events (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	payload    TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS work_requests (
	request_id     TEXT PRIMARY KEY,
	session_id     TEXT,
	status         TEXT NOT NULL,
	result         TEXT,
	received_at    TEXT NOT NULL,
	finished_at    TEXT,
	delivery_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS workspace_leases (
	workspace    TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL
);

-- Read paths: the session list's usage summary filters events down to two
-- kinds before scanning, and looks a session up by the request that
-- created it.
CREATE INDEX IF NOT EXISTS idx_events_session_kind ON events (session_id, kind);
CREATE INDEX IF NOT EXISTS idx_work_requests_session_id ON work_requests (session_id);
`

// Open opens (creating if needed) the SQLite database at path, applies the
// schema, and starts the writer goroutine.
func Open(path string) (*Store, error) {
	writeDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", path)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	readDB.SetMaxOpenConns(4)

	// busy_timeout goes first: it registers SQLite's busy handler before any
	// statement that might contend for the file, including journal_mode
	// itself. Two processes opening the same database for the first time
	// both race to switch it into WAL mode, and without a busy handler
	// already active that race can return SQLITE_BUSY immediately instead
	// of waiting.
	for _, db := range []*sql.DB{writeDB, readDB} {
		if err := setPragmasWithRetry(db); err != nil {
			writeDB.Close()
			readDB.Close()
			return nil, fmt.Errorf("store: set pragmas: %w", err)
		}
	}
	if _, err := writeDB.Exec(schema); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := migrateSessions(writeDB); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate sessions table: %w", err)
	}

	s := &Store{
		writeDB: writeDB,
		readDB:  readDB,
		jobs:    make(chan job),
		stopped: make(chan struct{}),
	}
	go s.writerLoop()
	return s, nil
}

// setPragmasWithRetry sets the pragmas Open needs, retrying on SQLITE_BUSY.
// The busy handler from the first successful PRAGMA busy_timeout is not yet
// active while that very statement runs, so two processes opening a fresh
// database at the same instant can still each see one immediate SQLITE_BUSY
// before the handler takes hold; a few short retries absorb that without
// requiring the caller to serialise process startup.
func setPragmasWithRetry(db *sql.DB) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") && !strings.Contains(err.Error(), "database is locked") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return err
}

// migrateSessions adds the session provenance columns to a sessions table
// created by an older binary. It reads the existing columns and adds only
// the missing ones, so it is a no-op on a database that already has them.
// ALTER TABLE ADD COLUMN with a constant NOT NULL default backfills existing
// rows in the same statement.
func migrateSessions(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, col := range sessionMigrationColumns {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return err
		}
	}
	return nil
}

// sessionMigrationColumns are the columns migrateSessions adds to a sessions
// table created by an older binary.
var sessionMigrationColumns = []struct {
	name string
	def  string
}{
	{"job_type", "TEXT NOT NULL DEFAULT 'implementation'"},
	{"parent_agent_type", "TEXT NOT NULL DEFAULT ''"},
	{"parent_agent_id", "TEXT NOT NULL DEFAULT ''"},
}

// Close stops the writer goroutine and closes both connection pools. It
// waits for any in-flight write to finish first.
func (s *Store) Close() error {
	close(s.jobs)
	<-s.stopped
	if err := s.writeDB.Close(); err != nil {
		return err
	}
	return s.readDB.Close()
}

func (s *Store) writerLoop() {
	defer close(s.stopped)
	for j := range s.jobs {
		j.resp <- s.runJob(j)
	}
}

func (s *Store) runJob(j job) error {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	if err := j.fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// submit runs fn inside a transaction on the single writer goroutine and
// waits for the result.
func (s *Store) submit(ctx context.Context, fn func(*sql.Tx) error) error {
	resp := make(chan error, 1)
	select {
	case s.jobs <- job{fn: fn, resp: resp}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func denyPatternsJSON(patterns []string) (string, error) {
	if patterns == nil {
		patterns = []string{}
	}
	b, err := json.Marshal(patterns)
	return string(b), err
}

// CreateSession inserts sess. Status defaults to StatusRunning if unset, and
// JobType defaults to the implementation job type.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if sess.Status == "" {
		sess.Status = StatusRunning
	}
	sess.JobType = agentmeta.NormalizeJobType(sess.JobType)
	deny, err := denyPatternsJSON(sess.DenyPatterns)
	if err != nil {
		return err
	}
	toolSchema := string(sess.ToolSchema)
	var resultSchema sql.NullString
	if len(sess.ResultSchema) > 0 {
		resultSchema = sql.NullString{String: string(sess.ResultSchema), Valid: true}
	}
	var parentID sql.NullString
	if sess.ParentID != "" {
		parentID = sql.NullString{String: sess.ParentID, Valid: true}
	}
	createdAt := sess.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO sessions (id, parent_id, job_type, parent_agent_type, parent_agent_id,
				model, effort, thinking, workspace, permission_mode, deny_patterns, system_prompt,
				tool_schema, result_schema, status, created_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			sess.ID, parentID, sess.JobType, sess.ParentAgentType, sess.ParentAgentID,
			sess.Model, sess.Effort, sess.Thinking, sess.Workspace, sess.PermissionMode,
			deny, sess.SystemPrompt, toolSchema, resultSchema, sess.Status, createdAt.Format(time.RFC3339Nano))
		return err
	})
}

// UpdateSessionStatus sets status and, when non-nil, finishedAt.
func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string, finishedAt *time.Time) error {
	var fa sql.NullString
	if finishedAt != nil {
		fa = sql.NullString{String: finishedAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = COALESCE(?, finished_at) WHERE id = ?`, status, fa, id)
		return err
	})
}

// ResumeSession marks a terminal session running again and clears
// finished_at, unconditionally rather than through UpdateSessionStatus's
// COALESCE — a resumed session is not finished anymore, so the old
// timestamp must go, not survive. Runner.Resume calls this once it has
// loaded the session and is about to append its continuation.
func (s *Store) ResumeSession(ctx context.Context, id string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = NULL WHERE id = ?`, StatusRunning, id)
		return err
	})
}

// DeleteSession removes id's row and its whole event log. It refuses a
// session whose status is still "running": nothing may delete a row a live
// session goroutine is still appending events to. The caller is responsible
// for removing the disk mirror directory, which this has no path for.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == StatusRunning {
			return fmt.Errorf("store: refusing to delete %s: it is still running", id)
		}
		if _, err := tx.Exec(`DELETE FROM events WHERE session_id = ?`, id); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM sessions WHERE id = ?`, id)
		return err
	})
}

func scanSession(row interface {
	Scan(dest ...any) error
}) (Session, error) {
	var sess Session
	var parentID, resultSchema, finishedAt sql.NullString
	var thinking int
	var denyJSON, createdAt string
	err := row.Scan(&sess.ID, &parentID, &sess.JobType, &sess.ParentAgentType, &sess.ParentAgentID,
		&sess.Model, &sess.Effort, &thinking, &sess.Workspace,
		&sess.PermissionMode, &denyJSON, &sess.SystemPrompt, (*sqlText)(&sess.ToolSchema), &resultSchema,
		&sess.Status, &createdAt, &finishedAt)
	if err != nil {
		return Session{}, err
	}
	sess.ParentID = parentID.String
	sess.Thinking = thinking != 0
	if resultSchema.Valid {
		sess.ResultSchema = json.RawMessage(resultSchema.String)
	}
	if err := json.Unmarshal([]byte(denyJSON), &sess.DenyPatterns); err != nil {
		return Session{}, fmt.Errorf("store: decode deny_patterns: %w", err)
	}
	sess.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Session{}, fmt.Errorf("store: decode created_at: %w", err)
	}
	if finishedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAt.String)
		if err != nil {
			return Session{}, fmt.Errorf("store: decode finished_at: %w", err)
		}
		sess.FinishedAt = &t
	}
	return sess, nil
}

// sqlText scans a TEXT column into a json.RawMessage.
type sqlText json.RawMessage

func (t *sqlText) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*t = sqlText(v)
	case []byte:
		*t = sqlText(append([]byte(nil), v...))
	case nil:
		*t = nil
	default:
		return fmt.Errorf("store: cannot scan %T into json.RawMessage", src)
	}
	return nil
}

const sessionColumns = `id, parent_id, job_type, parent_agent_type, parent_agent_id, model, effort,
	thinking, workspace, permission_mode, deny_patterns, system_prompt, tool_schema,
	result_schema, status, created_at, finished_at`

// GetSession reads one session by id.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ListSessions returns every session, newest first.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendEvents assigns sequence numbers starting after the session's current
// max and inserts inputs in order within one transaction. It returns the
// stored Events, including their assigned Seq and CreatedAt, so callers can
// mirror the same values to disk.
func (s *Store) AppendEvents(ctx context.Context, sessionID string, inputs []EventInput) ([]Event, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	out := make([]Event, len(inputs))

	err := s.submit(ctx, func(tx *sql.Tx) error {
		var maxSeq sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(seq) FROM events WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
			return err
		}
		next := maxSeq.Int64 + 1

		stmt, err := tx.Prepare(`INSERT INTO events (session_id, seq, kind, payload, created_at) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		createdAt := now.Format(time.RFC3339Nano)
		for i, in := range inputs {
			payload, err := json.Marshal(in.Payload)
			if err != nil {
				return fmt.Errorf("store: encode payload for %s: %w", in.Kind, err)
			}
			seq := next + int64(i)
			if _, err := stmt.Exec(sessionID, seq, string(in.Kind), string(payload), createdAt); err != nil {
				return err
			}
			out[i] = Event{SessionID: sessionID, Seq: seq, Kind: in.Kind, Payload: payload, CreatedAt: now}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetEvents returns every event for sessionID in seq order.
func (s *Store) GetEvents(ctx context.Context, sessionID string) ([]Event, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT session_id, seq, kind, payload, created_at FROM events WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kind, createdAt string
		var payload string
		if err := rows.Scan(&e.SessionID, &e.Seq, &kind, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.Kind = EventKind(kind)
		e.Payload = json.RawMessage(payload)
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("store: decode event created_at: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
			_, err := tx.Exec(`INSERT INTO workspace_leases (workspace, session_id, acquired_at, heartbeat_at) VALUES (?, ?, ?, ?)`,
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
