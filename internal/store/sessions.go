package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
)

// Session statuses.
const (
	StatusRunning   = "running"
	StatusCreating  = "creating"
	StatusOK        = "ok"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusMaxTurns  = "max_turns"
	StatusCancelled = "cancelled"
	StatusCompacted = "compacted"
)

// IsLive reports whether status is a live session status: "running", or
// "creating" while a queue-driven run's workspace is still being prepared.
// Every branch and SQL predicate that means "this session is live" is built
// off this constant and IsLive, never off a string literal, so the Go side
// and the SQL side cannot disagree about what live is.
func IsLive(status string) bool {
	return status == StatusRunning || status == StatusCreating
}

// Session is the frozen metadata row for one agent session. SystemPrompt and
// ToolSchema are rendered once at creation and never regenerated from the
// running binary, so a harness upgrade cannot change the prefix of a
// resumable session (docs/CACHE.md).
type Session struct {
	ID       string
	ParentID string
	JobType  string
	// Task is the job's description: the launching instruction of the run,
	// the same value the session_started payload's Task field carries
	// (internal/store/events.go), frozen on the row so the session list can
	// say what a session is about without reading the event log. Empty when
	// the run was created with no prompt (a browser start waits for its
	// first message).
	Task string
	// Title is the run's name, at most agentmeta.MaxTitleWords words, shown
	// bold on the main page in place of the raw prompt. Empty covers a
	// pre-migration row and a producer that left it blank (the browser
	// start form); the browser then renders the task line without a bold
	// title rather than an empty one.
	Title string
	// Description is what change the agent is making, at most
	// agentmeta.MaxDescriptionWords words, shown under the title on the main
	// page. Empty covers a pre-migration row and a producer that left it
	// blank; the browser falls back to the task as the description line.
	Description string
	// Phase is this run's 1-based position in a multi-phase chain (the
	// deepseek-flash-plan skill cuts a job into phases). Zero together with
	// TotalPhases zero means the run is not part of a chain; the browser
	// shows no phase chip then.
	Phase int
	// TotalPhases is how many phases the chain has. Zero together with Phase
	// zero means the run is not part of a chain; when set, Phase is 1-based
	// and at most TotalPhases (agentmeta.ValidatePhase).
	TotalPhases     int
	ParentAgentType string
	ParentAgentID   string
	// ParentIsUser records that a person started this session directly. It is
	// producer-stamped on the request and inherited by child sessions; false
	// on a pre-migration row is correct for essentially every historical row.
	ParentIsUser bool
	Model        string
	// PromptVariant is the name of the system prompt variant this session
	// runs under, frozen on the row like SystemPrompt and ToolSchema: a
	// resumed session must keep sending the array and rendering the head its
	// variant asks for, and the name is the only record of that once the run
	// is over. Empty is the shipped prompt.
	PromptVariant  string
	Effort         string
	Thinking       bool
	Workspace      string
	PermissionMode string
	DenyPatterns   []string
	SystemPrompt   string
	ToolSchema     json.RawMessage
	// MCPReadOnly is the per-server read-only allowance this run froze, the
	// third part of its permission policy (internal/tools,
	// Policy.MCPReadOnlyServers). A resume reads it here rather than
	// resolving it again, so nothing an operator or a client changes between
	// the run and the resume can widen what the session may call.
	MCPReadOnly  map[string]bool
	ResultSchema json.RawMessage
	Status       string
	// CompleteStatus is the status argument the model gave Complete ("done"
	// or "gave_up"), when it called the tool at all. Empty covers both a
	// pre-migration row and a session that ended without calling Complete;
	// the browser renders the empty value as
	// the plain terminal status rather than guessing.
	CompleteStatus string
	// Plan is the JSON encoding of the working plan's todos array, written
	// verbatim from the latest TodoWrite call.
	// Empty covers both a pre-migration row and a session that never called
	// TodoWrite; the browser renders the empty value as "no plan section"
	// rather than an empty list.
	Plan string
	// RecentToolCalls is the rolling roll of the last few tool calls the
	// session made, written alongside the plan in the same store write. The
	// in-flight card no longer renders it (the card's activity panel is
	// gone); it is kept because it rides that write, and dropping it would
	// save nothing. Nil when the session made none yet.
	RecentToolCalls []RecentToolCall
	// Summary is the summary argument the model gave Complete, its own
	// one-line account of what the run did, shown under the finished table's
	// session id. Empty when Complete was
	// never called.
	Summary    string
	CreatedAt  time.Time
	FinishedAt *time.Time
	// Version is the row's optimistic-concurrency counter
	// "Optimistic concurrency"): 1 at creation, incremented by 1 on every
	// successful mutation. The HTTP surface returns it in the session
	// representation and requires it echoed back in If-Match on a mutating
	// write, so a write based on a stale read fails with a version conflict
	// instead of racing whoever changed the row first.
	Version int
}

// RecentToolCall is one entry of the session row's rolling roll of the last
// few tool calls. The roll used to feed the in-flight card's activity panel;
// the card no longer renders it, and the roll is kept because it rides the
// same store write as the plan. Arguments is the raw text the model produced;
// it is not guaranteed to be valid JSON and is kept only so a consumer can
// shape a one-line target (file path, command, pattern) out of it.
type RecentToolCall struct {
	Name      string    `json:"name"`
	Arguments string    `json:"arguments"`
	CreatedAt time.Time `json:"created_at"`
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
	readOnly, err := mcpReadOnlyJSON(sess.MCPReadOnly)
	if err != nil {
		return err
	}
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
			INSERT INTO sessions (id, parent_id, job_type, task, title, description, phase, total_phases,
				parent_agent_type, parent_agent_id,
				model, prompt_variant, effort, thinking, workspace, permission_mode, deny_patterns, system_prompt,
				tool_schema, mcp_read_only, result_schema, status, created_at, finished_at, version, parent_is_user)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 1, ?)`,
			sess.ID, parentID, sess.JobType, sess.Task, sess.Title, sess.Description, sess.Phase, sess.TotalPhases,
			sess.ParentAgentType, sess.ParentAgentID,
			sess.Model, sess.PromptVariant, sess.Effort, sess.Thinking, sess.Workspace, sess.PermissionMode,
			deny, sess.SystemPrompt, toolSchema, readOnly, resultSchema, sess.Status, createdAt.Format(time.RFC3339Nano),
			sess.ParentIsUser)
		return err
	})
}

// PromoteSession flips a "creating" session row to "running" and writes the
// four columns that are only resolvable once the run starts: the workspace
// (which the worker has now finished preparing), the rendered system prompt,
// the tool schema, and the result schema. It bumps version like any other
// mutation.
//
// resultSchema is written even when nil — CreateSession's own NULL-vs-empty
// handling doesn't apply here because a "creating" row from Runner.Create
// never had a result_schema to begin with, so this column has nothing to
// preserve on a no-op promotion.
//
// A row already "running" is a no-op success, so a redelivery that calls
// PromoteSession twice — once from a retried Runner.Run after a crash — is
// idempotent. A terminal row refuses with SessionFinishedError: the run
// ended (a stop during preparation, a setup failure) and nothing may relabel
// it.
func (s *Store) PromoteSession(ctx context.Context, id, workspace, systemPrompt string, toolSchema, resultSchema []byte, mcpReadOnly map[string]bool) error {
	readOnly, err := mcpReadOnlyJSON(mcpReadOnly)
	if err != nil {
		return err
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&storedStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		switch storedStatus {
		case StatusRunning:
			return nil
		case StatusCreating:
		default:
			return &SessionFinishedError{SessionID: id, Status: storedStatus}
		}
		var resultSchemaCol sql.NullString
		if len(resultSchema) > 0 {
			resultSchemaCol = sql.NullString{String: string(resultSchema), Valid: true}
		}
		_, err := tx.Exec(`UPDATE sessions SET status = ?, workspace = ?, system_prompt = ?, tool_schema = ?, mcp_read_only = ?, result_schema = ?, version = version + 1 WHERE id = ?`,
			StatusRunning, workspace, systemPrompt, string(toolSchema), readOnly, resultSchemaCol, id)
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
// event log. It refuses to move a
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
// keeps. The roll no longer feeds a panel — the in-flight card's activity
// panel is gone — but the trimming rule stays because the roll is still
// written alongside the plan.
const maxRecentToolCalls = 5

// UpdateSessionLiveState atomically rewrites the session row's live plan
// and recent-tool-call roll. plan is the
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
// It accepts "creating" as well as "running", which is what lets a stop
// during workspace preparation mark the row instead of finding nothing
// (docs/RUN-CONTROL.md "Half two": the escalation's no-row branch). A second
// cancel is a no-op: the row is already cancelled, and re-stamping it would
// only move finished_at away from the moment the stop actually landed. A
// session that reached any *other* terminal status refuses with
// SessionFinishedError — it finished on its own in the gap between the
// operator asking and the stop landing, and relabelling a completed run as
// cancelled would destroy the one distinction the status carries.
func (s *Store) CancelRunningSession(ctx context.Context, id string, now time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var storedStatus string
		if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&storedStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		switch storedStatus {
		case StatusCancelled:
			return nil
		case StatusRunning, StatusCreating:
		default:
			return &SessionFinishedError{SessionID: id, Status: storedStatus}
		}
		fa := sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true}
		_, err := tx.Exec(`UPDATE sessions SET status = ?, finished_at = COALESCE(?, finished_at), version = version + 1 WHERE id = ?`,
			StatusCancelled, fa, id)
		return err
	})
}

// CloseSession transitions id to a terminal status — the write that lets an
// caller close a session an interrupted run left behind.
// A "creating" row — a dead worker's attempt that was still preparing its
// workspace — is treated exactly like "running": it takes the new status,
// so an operator can close a row a dead worker left mid-clone.
// It carries two guards, both checked inside the write transaction so no
// interleaving write can slip between a check and the UPDATE:
//
//   - Optimistic concurrency: wantVersion must equal the row's current
//     version, or VersionConflictError is returned. The version is read from
//     the session representation and echoed back in If-Match.
//   - Idleness: a live session whose most recent event is newer than
//     minIdle is presumed live and refused with ActiveSessionError. A live
//     run appends events continuously, so "abandoned" means quiet for
//     minIdle; a session with no events has nothing recent and passes. now is
//     the clock the idleness is judged against — the caller's — so the rule
//     is the HTTP layer's policy, not the store's.
//
// A live session gets the new status and finished_at = now. One already
// terminal keeps both — a re-close is a version bump and nothing else, so a
// retried write is idempotent and a finished run cannot be relabelled. The
// updated row is returned. The event log is untouched.
func (s *Store) CloseSession(ctx context.Context, id, status string, wantVersion int, now time.Time, minIdle time.Duration) (Session, error) {
	if status == StatusRunning || status == StatusCreating {
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
		if IsLive(storedStatus) {
			last, ok, err := lastEventAt(tx, id)
			if err != nil {
				return err
			}
			if ok && now.Sub(last) < minIdle {
				return &ActiveSessionError{SessionID: id, LastEventAt: last}
			}
		}
		// Only a live session takes the new status. A row that is
		// already terminal keeps the status it finished with: this endpoint
		// exists to close an abandoned run, not to relabel a finished one,
		// and a completed session's status is a fact about what happened.
		// Overwriting it would let any client — or any bug — rewrite the
		// record the transcript, the fold and resume all read as history.
		// A re-close therefore lands as a version bump and nothing else,
		// which keeps a retried PATCH idempotent.
		newStatus := storedStatus
		var fa sql.NullString
		if IsLive(storedStatus) {
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
// session whose status is still live — "running", or "creating" while a
// worker is cloning into that directory: nothing may delete a row a live
// session goroutine is still writing. wantVersion enforces the
// optimistic-concurrency precondition: it must equal the
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
		if IsLive(status) {
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
	var parentIsUser int
	var denyJSON, createdAt, recentCalls, readOnlyJSON string
	err := row.Scan(&sess.ID, &parentID, &sess.JobType, &sess.Task, &sess.Title, &sess.Description,
		&sess.Phase, &sess.TotalPhases, &sess.ParentAgentType, &sess.ParentAgentID,
		&sess.Model, &sess.PromptVariant, &sess.Effort, &thinking, &sess.Workspace,
		&sess.PermissionMode, &denyJSON, &sess.SystemPrompt, (*sqlText)(&sess.ToolSchema), &readOnlyJSON, &resultSchema,
		&sess.Status, &createdAt, &finishedAt, &sess.CompleteStatus, &sess.Plan, &recentCalls, &sess.Summary,
		&sess.Version, &parentIsUser)
	if err != nil {
		return Session{}, err
	}
	sess.ParentID = parentID.String
	sess.Thinking = thinking != 0
	sess.ParentIsUser = parentIsUser != 0
	if resultSchema.Valid {
		sess.ResultSchema = json.RawMessage(resultSchema.String)
	}
	if err := json.Unmarshal([]byte(denyJSON), &sess.DenyPatterns); err != nil {
		return Session{}, fmt.Errorf("store: decode deny_patterns: %w", err)
	}
	// A pre-migration row scans as '' rather than '{}', and an empty
	// allowance is the same thing as none.
	if readOnlyJSON != "" {
		if err := json.Unmarshal([]byte(readOnlyJSON), &sess.MCPReadOnly); err != nil {
			return Session{}, fmt.Errorf("store: decode mcp_read_only: %w", err)
		}
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

const sessionColumns = `id, parent_id, job_type, task, title, description, phase, total_phases,
	parent_agent_type, parent_agent_id, model, prompt_variant, effort,
	thinking, workspace, permission_mode, deny_patterns, system_prompt, tool_schema,
	mcp_read_only, result_schema, status, created_at, finished_at, complete_status, plan, recent_tool_calls, summary, version, parent_is_user`

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

// mcpReadOnlyJSON encodes a session's per-server read-only allowance for the
// mcp_read_only column. A nil or empty map is stored as "{}" rather than
// "null", so every row scans back through the same decode.
func mcpReadOnlyJSON(m map[string]bool) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("store: encode mcp_read_only: %w", err)
	}
	return string(b), nil
}
