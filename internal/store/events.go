package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Event kinds, per docs/DESIGN.md §4.1. The event log is the one source of
// truth the fold, the disk mirror, the SSE stream, and NATS progress
// messages all read from.
const (
	KindSessionStarted EventKind = "session_started"
	KindTurnStarted    EventKind = "turn_started"
	KindReasoningDelta EventKind = "reasoning_delta"
	KindContentDelta   EventKind = "content_delta"
	KindToolCall       EventKind = "tool_call"
	KindToolDenied     EventKind = "tool_denied"
	KindToolStdout     EventKind = "tool_stdout"
	KindToolResult     EventKind = "tool_result"
	KindUsage          EventKind = "usage"
	KindTurnFinished   EventKind = "turn_finished"
	KindRunFinished    EventKind = "run_finished"
	KindError          EventKind = "error"
	KindSteerMessage   EventKind = "steer_message"
	KindSteerApplied   EventKind = "steer_applied"
)

// EventKind is the tag on an Event row that says how to decode its payload.
type EventKind string

// EventKinds is every kind the event log can hold, in declaration order. The
// HTTP layer's ?kind= filter and its 400 "valid kinds" message derive from
// this list rather than a literal of their own (docs/DATA-API.md "events"),
// so adding an event kind to the log automatically extends the API's filter
// surface instead of silently leaving the new kind unfilterable.
var EventKinds = []EventKind{
	KindSessionStarted, KindTurnStarted, KindReasoningDelta, KindContentDelta,
	KindToolCall, KindToolDenied, KindToolStdout, KindToolResult, KindUsage,
	KindTurnFinished, KindRunFinished, KindError, KindSteerMessage, KindSteerApplied,
}

// ValidEventKind reports whether name is a kind the event log can hold — the
// membership test the HTTP layer's ?kind= filter runs before touching the
// store. The answer comes from EventKinds, the same list the filter's 400
// message is built from, so the two can never disagree.
func ValidEventKind(name string) bool {
	for _, k := range EventKinds {
		if string(k) == name {
			return true
		}
	}
	return false
}

// SessionStartedPayload carries a user message that starts a new turn of
// the conversation. It appears once, before the first sub-turn, for every
// session; Runner.Resume appends a second (or later) one to carry the
// instruction a resumed session continues with, which the fold treats
// exactly the same way — append a user message at this point in the log.
// The system prompt and tool schema live on the Session row instead, frozen
// separately from the event log (docs/CACHE.md).
type SessionStartedPayload struct {
	OpeningMessage string `json:"opening_message"`
	// SkillCatalogue is the exact substring of OpeningMessage that lists the
	// workspace's skills (internal/skills), empty when there are none. It is
	// stored separately so the browser can lift the catalogue into its own
	// panel without parsing the message text. OpeningMessage still holds the
	// bytes that went to the model.
	SkillCatalogue string `json:"skill_catalogue,omitempty"`
	// ClaudeMDBlock is the exact substring of OpeningMessage that renders the
	// root CLAUDE.md files found in the workspace (internal/claudemd), empty
	// when there are none. It is stored separately the way SkillCatalogue is,
	// so a consumer can lift it out without parsing the message text.
	ClaudeMDBlock string `json:"claude_md_block,omitempty"`
	// Task is the exact substring of OpeningMessage that is the task
	// instruction — the "Task:\n..." tail of the rendered message, which is
	// the launching agent's own instruction for an agent-started run — empty
	// when the run was created with none (a browser start waits for its
	// first message). Stored separately the way SkillCatalogue is, so the
	// watch page can render the launcher's instruction as its own message
	// without parsing the message text. Only the run-creating
	// session_started carries it: a resume instruction is a continuation,
	// not the launch instruction.
	Task string `json:"task,omitempty"`
	// Attachments are the paths the request's attachments were materialised
	// into — "scratch/attachments/<name>" inside the session workspace
	// (internal/workspace), the same paths the opening message lists, empty
	// when the request carried none. Stored separately the way SkillCatalogue
	// is, so the browser can render the images through
	// GET /api/sessions/{id}/screenshot without parsing the message text.
	Attachments []string `json:"attachments,omitempty"`
}

// TurnStartedPayload marks the start of one sub-turn.
type TurnStartedPayload struct {
	SubTurn int `json:"sub_turn"`
}

// ReasoningDeltaPayload is one fragment of reasoning_content, accumulated by
// the fold into the sub-turn's assistant message.
type ReasoningDeltaPayload struct {
	Text string `json:"text"`
}

// ContentDeltaPayload is one fragment of content, accumulated the same way.
type ContentDeltaPayload struct {
	Text string `json:"text"`
}

// ToolCallPayload is one completed tool call the model made in this
// sub-turn. Events of this kind are appended in tool_calls array order, so
// folding them back preserves that order without needing the Index field —
// it is kept for diagnostics.
type ToolCallPayload struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResultPayload is the outcome of executing one tool call. Diff and
// ChildSessionID are populated only for the tools that produce them (Edit
// and Task respectively); every other tool leaves them empty, and the
// frontend's per-tool block shaping reads them opt-in.
//
// ImageURL is the base64 data URI of an image Read returned as an image_url
// part (docs/KIMI-INTEGRATION.md §4.5). It is populated only for that case
// and lives in the event payload rather than on disk: the fold rebuilds the
// parts array from Content and ImageURL, so replaying the log reproduces
// the exact bytes regardless of what happened to the file since
// (internal/fold/fold.go, docs/DESIGN.md §4.1).
type ToolResultPayload struct {
	ToolCallID     string     `json:"tool_call_id"`
	Name           string     `json:"name"`
	Content        string     `json:"content"`
	IsError        bool       `json:"is_error,omitempty"`
	Truncated      bool       `json:"truncated,omitempty"`
	Diff           []DiffLine `json:"diff,omitempty"`
	ChildSessionID string     `json:"child_session_id,omitempty"`
	ImageURL       string     `json:"image_url,omitempty"`
}

// ToolDeniedPayload is a tool call refused by permission policy. It folds
// into a tool-role message exactly like ToolResultPayload; it is a distinct
// event kind only so denials are independently findable (docs/TOOLS.md).
type ToolDeniedPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Rule       string `json:"rule"`
	Content    string `json:"content"`
}

// ToolStdoutPayload is incremental command output for live display. It is
// never read back to build the message array; Bash still records the final
// ToolResultPayload.
type ToolStdoutPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Text       string `json:"text"`
}

// UsagePayload is one request's token accounting plus the cache churn
// diagnostic (docs/CACHE.md).
//
// One event per request to the API, not per sub-turn: the reasoning-starved
// retry in internal/session sends a second request for the same sub-turn and
// is billed for both, so that turn commits two of these, sharing a SubTurn
// and distinguished by Attempt. Summing over every event is what gives a
// session's true cost. Attempt is zero, and omitted, on the ordinary path
// where the sub-turn made exactly one request.
//
// Model is set only when the request was not the session's own: a
// ReviewScreenshot call bills a vision model and commits its usage as a
// second event on the same sub-turn, and until this field existed the two
// were distinguishable only by that collision. An empty Model therefore
// means the session's own model, and a non-empty one names what else was
// billed — which is what makes vision spend separable from the run's
// (docs/reviews/vision-path-2026-08-14.md measured it at 39% of a session's
// cost, invisible in the log).
type UsagePayload struct {
	SubTurn               int     `json:"sub_turn"`
	Attempt               int     `json:"attempt,omitempty"`
	Model                 string  `json:"model,omitempty"`
	PromptTokens          int     `json:"prompt_tokens"`
	PromptCacheHitTokens  int     `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int     `json:"prompt_cache_miss_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	ReasoningTokens       int     `json:"reasoning_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	// RateTier is which set of rates CostUSD was computed at — "flat",
	// "peak" or "off_peak" (internal/pricing). From 2026-08-16 DeepSeek
	// bills peak hours at twice off-peak, so two identical sub-turns can
	// differ threefold in cost for no reason visible in their token counts;
	// this is the record of which one applied, on the event that already
	// carries the figure. Empty on a session committed before the split,
	// and on any usage the price table could not cost at all.
	RateTier           string `json:"rate_tier,omitempty"`
	ExpectedMissTokens int    `json:"expected_miss_tokens"`
	ChurnPointIndex    *int   `json:"churn_point_index,omitempty"`
}

// TurnFinishedPayload closes out a sub-turn's assistant message. ElapsedMs is
// the wall time the sub-turn's request(s) took, measured in the runner around
// the stream calls; created_at cannot carry it because AppendEvents stamps one
// instant across the whole batch. Absent on sessions committed before this
// field existed.
type TurnFinishedPayload struct {
	SubTurn      int    `json:"sub_turn"`
	FinishReason string `json:"finish_reason"`
	ElapsedMs    int64  `json:"elapsed_ms,omitempty"`
}

// RunFinishedPayload is the terminal event of a session.
type RunFinishedPayload struct {
	Reason  string          `json:"reason"`
	Status  string          `json:"status,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Text    string          `json:"text"`
}

// ErrorPayload records a run-ending failure.
type ErrorPayload struct {
	Message string `json:"message"`
}

// SteerMessagePayload is an operator instruction accepted for a running
// session (docs/RUN-CONTROL.md "Steering"). It carries no messages-array
// content: the loop decides where the model sees it, and records that with a
// steer_applied event. Source is who sent it — "web", "mcp", or "cli".
type SteerMessagePayload struct {
	Text   string `json:"text"`
	Source string `json:"source,omitempty"` // "web", "mcp", "cli"
}

// SteerAppliedPayload is the point in the log where a steer_message became a
// message in the conversation. SourceSeq links it to the steer_message it
// applies, which is what lets a resumed run recompute which steers are
// outstanding from the log alone, with no in-memory high-water mark to lose.
// A reminder the loop generated for itself carries no source, so SourceSeq is
// zero and the high-water mark ignores it (internal/session/reminders.go).
type SteerAppliedPayload struct {
	SourceSeq int64  `json:"source_seq"`
	Text      string `json:"text"`
	SubTurn   int    `json:"sub_turn"`
	// Role is the message role this folds to. Empty is "user", which is what
	// every operator steer is and what every row written before this field
	// existed holds.
	Role string `json:"role,omitempty"`
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
