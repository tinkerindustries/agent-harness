// Package store is the harness's SQLite persistence: sessions, their event
// logs, work requests, workspace leases, and settings (docs/DESIGN.md §4.8).
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

// ErrSessionCancelled is returned when a write targets a session that was
// stopped: its status is "cancelled", which is terminal and final
// (docs/RUN-CONTROL.md "Half two"). AppendEvents refuses a cancelled session
// so a wedged goroutine that wakes long after a stop cannot dirty the log it
// was stopped in, and FinishSession and UpdateSessionStatus refuse to move a
// cancelled row anywhere else.
var ErrSessionCancelled = errors.New("store: session is cancelled")

// SessionRunningError is returned when a mutating write targets a session
// whose status is still "running": nothing may delete (or otherwise
// overwrite) a row a live session goroutine is still appending events to.
// It surfaces as a 409 on the HTTP surface (docs/DATA-API.md).
type SessionRunningError struct {
	SessionID string
}

func (e *SessionRunningError) Error() string {
	return fmt.Sprintf("store: refusing to delete %s: it is still running", e.SessionID)
}

// ActiveSessionError is returned when a write would close a running session
// whose most recent event is newer than the caller's idle threshold — the
// row looks live, so the write refuses rather than race the run loop
// (docs/DATA-API.md "Preconditions"). LastEventAt is what the 409 message
// names: when the session was actually last heard from.
type ActiveSessionError struct {
	SessionID   string
	LastEventAt time.Time
}

func (e *ActiveSessionError) Error() string {
	return fmt.Sprintf("store: session %s is still active: most recent event at %s",
		e.SessionID, e.LastEventAt.UTC().Format(time.RFC3339))
}

// VersionConflictError is returned when a mutating write carries an If-Match
// version that does not equal the row's current version — the client read the
// row before someone else changed it (docs/DATA-API.md "Optimistic
// concurrency"). It surfaces as a 412 on the HTTP surface. Resource names the
// row ("session <id>", "work_request <id>") so the message is
// self-describing in every resource's handler.
type VersionConflictError struct {
	Resource string
	Want     int
	Current  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("store: %s changed since it was read: If-Match %d, current version %d",
		e.Resource, e.Want, e.Current)
}

// ActiveRequestError is returned when a write would close or delete a work
// request whose session is still live — the row a live pool worker is using,
// which will publish its own terminal result. The request's session id is
// the signal: the session's most recent event newer than the caller's idle
// threshold means the request is genuinely in flight, so the write refuses
// rather than race the worker (docs/DATA-API.md "Preconditions"). It
// surfaces as a 409 on the HTTP surface, and LastEventAt is what the message
// names: when the session was actually last heard from.
type ActiveRequestError struct {
	RequestID   string
	SessionID   string
	LastEventAt time.Time
}

func (e *ActiveRequestError) Error() string {
	return fmt.Sprintf("store: work request %s is still in flight: its session %s had its most recent event at %s",
		e.RequestID, e.SessionID, e.LastEventAt.UTC().Format(time.RFC3339))
}

// ActiveLeaseError is returned when a write would release a workspace lease
// whose heartbeat is newer than the caller's idle threshold — the lease a
// live session is still holding, so the write refuses rather than releasing
// a workspace that is being used right now (docs/DATA-API.md
// "Preconditions"). It surfaces as a 409 on the HTTP surface, and
// LastHeartbeatAt is what the message names: when the lease was actually
// last heartbeated.
type ActiveLeaseError struct {
	Workspace       string
	SessionID       string
	LastHeartbeatAt time.Time
}

func (e *ActiveLeaseError) Error() string {
	return fmt.Sprintf("store: lease on workspace %s is still held by live session %s: last heartbeat at %s",
		e.Workspace, e.SessionID, e.LastHeartbeatAt.UTC().Format(time.RFC3339))
}

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
	// CompleteStatus is the status argument the model gave Complete ("done"
	// or "gave_up"), when it called the tool at all. Empty covers both a
	// pre-migration row and a session that ended without calling Complete
	// (docs/WEB-REDESIGN.md phase 2); the browser renders the empty value as
	// the plain terminal status rather than guessing.
	CompleteStatus string
	// Plan is the JSON encoding of the working plan's todos array, written
	// verbatim from the latest TodoWrite call (docs/WEB-REDESIGN.md phase 3).
	// Empty covers both a pre-migration row and a session that never called
	// TodoWrite; the browser renders the empty value as "no plan section"
	// rather than an empty list.
	Plan string
	// RecentToolCalls is the rolling roll of the last few tool calls the
	// session made, for the in-flight card's activity panel
	// (docs/WEB-REDESIGN.md phase 3). Nil when the session made none yet.
	RecentToolCalls []RecentToolCall
	// Summary is the summary argument the model gave Complete, its own
	// one-line account of what the run did, shown under the finished table's
	// session id (docs/WEB-REDESIGN.md phase 3). Empty when Complete was
	// never called.
	Summary    string
	CreatedAt  time.Time
	FinishedAt *time.Time
	// Version is the row's optimistic-concurrency counter (docs/DATA-API.md
	// "Optimistic concurrency"): 1 at creation, incremented by 1 on every
	// successful mutation. The HTTP surface returns it in the session
	// representation and requires it echoed back in If-Match on a mutating
	// write, so a write based on a stale read fails with a version conflict
	// instead of racing whoever changed the row first.
	Version int
}

// RecentToolCall is one entry of the session row's rolling roll of the last
// few tool calls, carried on the session list so an in-flight card can show
// what a running session is doing without opening its transcript
// (docs/WEB-REDESIGN.md phase 3, design/sessions.html's "Last five calls").
// Arguments is the raw text the model produced; it is not guaranteed to be
// valid JSON and is kept only so the browser can shape a one-line target
// (file path, command, pattern) out of it.
type RecentToolCall struct {
	Name      string    `json:"name"`
	Arguments string    `json:"arguments"`
	CreatedAt time.Time `json:"created_at"`
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
	finished_at       TEXT,
	version           INTEGER NOT NULL DEFAULT 1
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
	delivery_count INTEGER NOT NULL DEFAULT 0,
	version        INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS workspace_leases (
	workspace    TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL,
	version      INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
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
	if err := migrateTableColumns(writeDB, "sessions", sessionMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate sessions table: %w", err)
	}
	if err := migrateTableColumns(writeDB, "work_requests", workRequestMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate work_requests table: %w", err)
	}
	if err := migrateTableColumns(writeDB, "workspace_leases", workspaceLeaseMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate workspace_leases table: %w", err)
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

// migrateTableColumns adds columns to a table created by an older binary. It
// reads the existing columns and adds only the missing ones, so it is a
// no-op on a database that already has them. ALTER TABLE ADD COLUMN with a
// constant NOT NULL default backfills existing rows in the same statement.
func migrateTableColumns(db *sql.DB, table string, columns []migrationColumn) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
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

	for _, col := range columns {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return err
		}
	}
	return nil
}

// migrationColumn is one column migrateTableColumns adds to a table created
// by an older binary.
type migrationColumn struct {
	name string
	def  string
}

// sessionMigrationColumns are the columns migrateTableColumns adds to a
// sessions table created by an older binary.
var sessionMigrationColumns = []migrationColumn{
	{"job_type", "TEXT NOT NULL DEFAULT 'implementation'"},
	{"parent_agent_type", "TEXT NOT NULL DEFAULT ''"},
	{"parent_agent_id", "TEXT NOT NULL DEFAULT ''"},
	// complete_status: the status argument to Complete, kept alongside the
	// session's own status so the session list can tell DONE from GAVE UP
	// (docs/WEB-REDESIGN.md phase 2). Older rows default to the empty string,
	// which the browser renders as the plain terminal status rather than
	// guessing.
	{"complete_status", "TEXT NOT NULL DEFAULT ''"},
	// plan: the JSON todos array of the latest TodoWrite call, so the
	// session list carries the live plan without re-walking the event log
	// and keeps it for finished sessions (docs/WEB-REDESIGN.md phase 3).
	// Older rows default to the empty string, which the browser renders as
	// "no plan section" rather than an empty list.
	{"plan", "TEXT NOT NULL DEFAULT ''"},
	// recent_tool_calls: the rolling roll of the last few tool calls, for
	// the in-flight card's activity panel (docs/WEB-REDESIGN.md phase 3).
	{"recent_tool_calls", "TEXT NOT NULL DEFAULT ''"},
	// summary: the summary argument the model gave Complete, its own
	// one-line account of the run, shown under the finished table's session
	// id (docs/WEB-REDESIGN.md phase 3). Older rows default to the empty
	// string, which the browser renders as no subtitle.
	{"summary", "TEXT NOT NULL DEFAULT ''"},
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
}

// workRequestMigrationColumns are the columns migrateTableColumns adds to a
// work_requests table created by an older binary.
var workRequestMigrationColumns = []migrationColumn{
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
}

// workspaceLeaseMigrationColumns are the columns migrateTableColumns adds to
// a workspace_leases table created by an older binary.
var workspaceLeaseMigrationColumns = []migrationColumn{
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
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
				tool_schema, result_schema, status, created_at, finished_at, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 1)`,
			sess.ID, parentID, sess.JobType, sess.ParentAgentType, sess.ParentAgentID,
			sess.Model, sess.Effort, sess.Thinking, sess.Workspace, sess.PermissionMode,
			deny, sess.SystemPrompt, toolSchema, resultSchema, sess.Status, createdAt.Format(time.RFC3339Nano))
		return err
	})
}

// UpdateSessionStatus sets status and, when non-nil, finishedAt. It refuses
// to move a cancelled row: cancelled is terminal and final, and a wedged
// goroutine that wakes after a stop must not be able to relabel the session
// it was stopped in (docs/RUN-CONTROL.md "Half two").
func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string, finishedAt *time.Time) error {
	var fa sql.NullString
	if finishedAt != nil {
		fa = sql.NullString{String: finishedAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		var stored string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&stored); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if stored == StatusCancelled {
			return ErrSessionCancelled
		}
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE id = ?`, status, fa, id)
		return err
	})
}

// FinishSession marks a session terminal the way finishRun ends one: status
// and finished_at together with complete_status, the status argument the
// model gave Complete ("" when it never called the tool), and summary, the
// model's own one-line account of the run ("" when it never called the
// tool). Keeping the complete_status and summary writes in the same UPDATE
// as the terminal status is what makes the session list able to tell DONE
// from GAVE UP and to subtitle the finished table without re-walking the
// event log (docs/WEB-REDESIGN.md phases 2 and 3). It refuses to move a
// cancelled row, like UpdateSessionStatus: a cancelled session is terminal
// and final, and no wedged goroutine that wakes after a stop may relabel it
// (docs/RUN-CONTROL.md "Half two").
func (s *Store) FinishSession(ctx context.Context, id, status, completeStatus, summary string, finishedAt *time.Time) error {
	var fa sql.NullString
	if finishedAt != nil {
		fa = sql.NullString{String: finishedAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		var stored string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&stored); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if stored == StatusCancelled {
			return ErrSessionCancelled
		}
		_, err := tx.Exec(`UPDATE sessions SET status = ?, complete_status = ?, summary = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE id = ?`,
			status, completeStatus, summary, fa, id)
		return err
	})
}

// maxRecentToolCalls is how many tool calls the session row's rolling roll
// keeps for the in-flight card's activity panel (docs/WEB-REDESIGN.md phase
// 3, design/sessions.html's "Last five calls").
const maxRecentToolCalls = 5

// UpdateSessionLiveState atomically rewrites the session row's live plan
// and recent-tool-call roll (docs/WEB-REDESIGN.md phase 3). plan is the
// JSON todos array from the latest TodoWrite call; the empty string keeps
// the existing plan, so a sub-turn with no TodoWrite never clobbers one
// that had it. calls are appended to the stored roll and trimmed to the
// most recent maxRecentToolCalls. The runner calls this where it already
// appends tool_call events — the one place that sees every tool call and
// already holds the store handle; tools.Executor never touches the store.
func (s *Store) UpdateSessionLiveState(ctx context.Context, id, plan string, calls []RecentToolCall) error {
	if plan == "" && len(calls) == 0 {
		return nil
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		if plan != "" {
			if _, err := tx.Exec(`UPDATE sessions SET plan = ?, version = version + 1 WHERE id = ?`, plan, id); err != nil {
				return err
			}
		}
		if len(calls) > 0 {
			var existing string
			if err := tx.QueryRow(`SELECT recent_tool_calls FROM sessions WHERE id = ?`, id).Scan(&existing); err != nil {
				return err
			}
			var roll []RecentToolCall
			_ = json.Unmarshal([]byte(existing), &roll)
			roll = append(roll, calls...)
			if len(roll) > maxRecentToolCalls {
				roll = roll[len(roll)-maxRecentToolCalls:]
			}
			b, err := json.Marshal(roll)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE sessions SET recent_tool_calls = ?, version = version + 1 WHERE id = ?`, string(b), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResumeSession marks a terminal session running again and clears
// finished_at, unconditionally rather than through UpdateSessionStatus's
// COALESCE — a resumed session is not finished anymore, so the old
// timestamp must go, not survive. Runner.Resume calls this once it has
// loaded the session and is about to append its continuation.
func (s *Store) ResumeSession(ctx context.Context, id string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = NULL, version = version + 1 WHERE id = ?`, StatusRunning, id)
		return err
	})
}

// CancelRunningSession marks id cancelled, sets finished_at = now, and bumps
// version — the store half of a stop (docs/RUN-CONTROL.md "Half two"). It
// deliberately takes no idle precondition: CloseSession's idle check exists
// to stop an operator closing a live row, while this writer is the run's own
// owner and the row being live is the point. A distinct method rather than
// CloseSession(..., minIdle: 0) so the intent is readable at the call site.
// A second cancel lands as a version bump and nothing else: the row is
// already cancelled, so finished_at is kept, the way a re-close keeps it.
func (s *Store) CancelRunningSession(ctx context.Context, id string, now time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&storedStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var fa sql.NullString
		if storedStatus != StatusCancelled {
			fa = sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true}
		}
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE id = ?`,
			StatusCancelled, fa, id)
		return err
	})
}

// CloseSession transitions id to a terminal status — the write that lets an
// operator close a session a dead worker left running (docs/DATA-API.md).
// It carries two guards, both checked inside the write transaction so no
// interleaving write can slip between a check and the UPDATE:
//
//   - Optimistic concurrency: wantVersion must equal the row's current
//     version, or VersionConflictError is returned. The version is read from
//     the session representation and echoed back in If-Match.
//   - Idleness: a running session whose most recent event is newer than
//     minIdle is presumed live and refused with ActiveSessionError. A live
//     run appends events continuously, so "abandoned" means quiet for
//     minIdle; a session with no events has nothing recent and passes. now is
//     the clock the idleness is judged against — the caller's — so the rule
//     is the HTTP layer's policy, not the store's.
//
// A running session gets the new status and finished_at = now. One already
// terminal keeps both — a re-close is a version bump and nothing else, so a
// retried write is idempotent and a finished run cannot be relabelled. The
// updated row is returned. The event log is untouched.
func (s *Store) CloseSession(ctx context.Context, id, status string, wantVersion int, now time.Time, minIdle time.Duration) (Session, error) {
	if status == StatusRunning {
		return Session{}, fmt.Errorf("store: CloseSession: %s is not a terminal status", status)
	}
	var out Session
	err := s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		var version int
		if err := tx.QueryRow(`SELECT status, version FROM sessions WHERE id = ?`, id).Scan(&storedStatus, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if version != wantVersion {
			return &VersionConflictError{Resource: "session " + id, Want: wantVersion, Current: version}
		}
		if storedStatus == StatusRunning {
			last, ok, err := lastEventAt(tx, id)
			if err != nil {
				return err
			}
			if ok && now.Sub(last) < minIdle {
				return &ActiveSessionError{SessionID: id, LastEventAt: last}
			}
		}
		// Only a running session takes the new status. A row that is
		// already terminal keeps the status it finished with: this endpoint
		// exists to close an abandoned run, not to relabel a finished one,
		// and a completed session's status is a fact about what happened.
		// Overwriting it would let any client — or any bug — rewrite the
		// record the transcript, the fold and resume all read as history.
		// A re-close therefore lands as a version bump and nothing else,
		// which keeps a retried PATCH idempotent.
		newStatus := storedStatus
		var fa sql.NullString
		if storedStatus == StatusRunning {
			newStatus = status
			fa = sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE id = ?`, newStatus, fa, id); err != nil {
			return err
		}
		var err2 error
		out, err2 = scanSession(tx.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
		return err2
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// lastEventAt returns the created_at of sessionID's most recent event, or
// ok=false when the session has no events. Events are appended in seq order
// under the single writer goroutine, so the last seq is the last event.
func lastEventAt(tx *sql.Tx, sessionID string) (time.Time, bool, error) {
	var createdAt string
	err := tx.QueryRow(`SELECT created_at FROM events WHERE session_id = ? ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: decode event created_at: %w", err)
	}
	return t, true, nil
}

// DeleteSession removes id's row and its whole event log. It refuses a
// session whose status is still "running": nothing may delete a row a live
// session goroutine is still appending events to. wantVersion enforces the
// optimistic-concurrency precondition (docs/DATA-API.md): it must equal the
// row's current version, or VersionConflictError is returned, so a delete
// based on a stale read refuses instead of deleting a row that changed since
// the client saw it. The caller is responsible for removing the disk mirror
// directory, which this has no path for.
func (s *Store) DeleteSession(ctx context.Context, id string, wantVersion int) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var status string
		var version int
		err := tx.QueryRow(`SELECT status, version FROM sessions WHERE id = ?`, id).Scan(&status, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != wantVersion {
			return &VersionConflictError{Resource: "session " + id, Want: wantVersion, Current: version}
		}
		if status == StatusRunning {
			return &SessionRunningError{SessionID: id}
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
	var denyJSON, createdAt, recentCalls string
	err := row.Scan(&sess.ID, &parentID, &sess.JobType, &sess.ParentAgentType, &sess.ParentAgentID,
		&sess.Model, &sess.Effort, &thinking, &sess.Workspace,
		&sess.PermissionMode, &denyJSON, &sess.SystemPrompt, (*sqlText)(&sess.ToolSchema), &resultSchema,
		&sess.Status, &createdAt, &finishedAt, &sess.CompleteStatus, &sess.Plan, &recentCalls, &sess.Summary,
		&sess.Version)
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
	// The column defaults to '' on a pre-migration row, which is not valid
	// JSON; the empty roll is the same thing as none.
	if recentCalls != "" {
		if err := json.Unmarshal([]byte(recentCalls), &sess.RecentToolCalls); err != nil {
			return Session{}, fmt.Errorf("store: decode recent_tool_calls: %w", err)
		}
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
	result_schema, status, created_at, finished_at, complete_status, plan, recent_tool_calls, summary, version`

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
		// The fence sits inside the write transaction, beside the MAX(seq)
		// read: one status lookup per batch, never per event, and a check
		// outside the transaction would be a race against the stop, not a
		// guard. Only "cancelled" refuses — compaction and resume append to
		// sessions in every other terminal status (docs/RUN-CONTROL.md "Half
		// two").
		var status string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status == StatusCancelled {
			return ErrSessionCancelled
		}

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

// SteerMessagesAfter returns the steer_message events for sessionID with seq
// greater than afterSeq, in seq order, capped at limit. The session loop runs
// this once per sub-turn (docs/RUN-CONTROL.md "How the loop picks one up"), so
// the query filters by kind and seq in SQL: it must never pull reasoning or
// content payloads off the disk, the same rule RequestStatus follows for its
// own cheap polled query.
func (s *Store) SteerMessagesAfter(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]Event, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT session_id, seq, kind, payload, created_at FROM events
		 WHERE session_id = ? AND kind = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		sessionID, KindSteerMessage, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kind, createdAt, payload string
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

// LastAppliedSteerSeq returns the highest SourceSeq across the session's
// steer_applied events, or 0 when there are none. The session loop runs this
// once when a run starts or resumes — not per sub-turn — so reading the
// steer_applied payloads and taking the max in Go is simpler than SQL JSON
// extraction, and those payloads are tiny.
func (s *Store) LastAppliedSteerSeq(ctx context.Context, sessionID string) (int64, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT payload FROM events WHERE session_id = ? AND kind = ?`, sessionID, KindSteerApplied)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var max int64
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return 0, err
		}
		var p SteerAppliedPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return 0, fmt.Errorf("store: decode steer_applied payload: %w", err)
		}
		if p.SourceSeq > max {
			max = p.SourceSeq
		}
	}
	return max, rows.Err()
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
