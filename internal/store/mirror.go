package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mirror writes the disk copy of a session directory
// (docs/DESIGN.md §4.8):
//
//	<data_dir>/sessions/<yyyy-mm-dd>/<session_id>/
//	  session.json      metadata, including the frozen system prompt and tools
//	  events.jsonl       one JSON object per event, appended in seq order
//	  transcript.md      rendered for reading, rewritten at turn boundaries
//	  request.json       the originating work request, when there was one
//
// The mirror is derived, never a second source of truth. Every method here
// is a pure function of the Session and Event values it is given, so the
// same DB rows always reproduce the same bytes whether they are written live
// or rebuilt by Export.
type Mirror struct {
	root string
}

// NewMirror returns a Mirror rooted at <dataDir>/sessions.
func NewMirror(dataDir string) *Mirror {
	return &Mirror{root: filepath.Join(dataDir, "sessions")}
}

// Dir returns the directory a session's mirror lives in.
func (m *Mirror) Dir(sess Session) string {
	day := sess.CreatedAt.UTC().Format("2006-01-02")
	return filepath.Join(m.root, day, sess.ID)
}

type sessionJSON struct {
	ID              string          `json:"id"`
	ParentID        string          `json:"parent_id,omitempty"`
	JobType         string          `json:"job_type"`
	ParentAgentType string          `json:"parent_agent_type,omitempty"`
	ParentAgentID   string          `json:"parent_agent_id,omitempty"`
	Model           string          `json:"model"`
	Effort          string          `json:"effort"`
	Thinking        bool            `json:"thinking"`
	Workspace       string          `json:"workspace"`
	PermissionMode  string          `json:"permission_mode"`
	DenyPatterns    []string        `json:"deny_patterns"`
	SystemPrompt    string          `json:"system_prompt"`
	ToolSchema      json.RawMessage `json:"tool_schema"`
	ResultSchema    json.RawMessage `json:"result_schema,omitempty"`
	Status          string          `json:"status"`
	CreatedAt       string          `json:"created_at"`
	FinishedAt      string          `json:"finished_at,omitempty"`
}

func toSessionJSON(sess Session) sessionJSON {
	sj := sessionJSON{
		ID:              sess.ID,
		ParentID:        sess.ParentID,
		JobType:         sess.JobType,
		ParentAgentType: sess.ParentAgentType,
		ParentAgentID:   sess.ParentAgentID,
		Model:           sess.Model,
		Effort:          sess.Effort,
		Thinking:        sess.Thinking,
		Workspace:       sess.Workspace,
		PermissionMode:  sess.PermissionMode,
		DenyPatterns:    sess.DenyPatterns,
		SystemPrompt:    sess.SystemPrompt,
		ToolSchema:      sess.ToolSchema,
		ResultSchema:    sess.ResultSchema,
		Status:          sess.Status,
		CreatedAt:       sess.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	if sess.FinishedAt != nil {
		sj.FinishedAt = sess.FinishedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}
	if sj.DenyPatterns == nil {
		sj.DenyPatterns = []string{}
	}
	return sj
}

// Init creates the session directory and writes session.json, truncating
// any prior events.jsonl and transcript.md so a rebuild starts clean. It
// writes request.json when request is non-empty.
func (m *Mirror) Init(sess Session, request json.RawMessage) error {
	dir := m.Dir(sess)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	if err := m.UpdateSession(sess); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o644); err != nil {
		return fmt.Errorf("store: create events.jsonl: %w", err)
	}
	if len(request) > 0 {
		var buf bytes.Buffer
		if err := json.Indent(&buf, request, "", "  "); err != nil {
			buf.Reset()
			buf.Write(request)
		}
		if err := os.WriteFile(filepath.Join(dir, "request.json"), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("store: write request.json: %w", err)
		}
	}
	return nil
}

// UpdateSession rewrites session.json, e.g. after a status change.
func (m *Mirror) UpdateSession(sess Session) error {
	dir := m.Dir(sess)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(toSessionJSON(sess), "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode session.json: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(dir, "session.json"), b, 0o644); err != nil {
		return fmt.Errorf("store: write session.json: %w", err)
	}
	return nil
}

type eventJSON struct {
	Seq       int64           `json:"seq"`
	Kind      EventKind       `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// AppendEvents appends events to events.jsonl, one JSON object per line, in
// the order given. Call it with the same Events a successful
// Store.AppendEvents returned, so the mirror always reflects committed rows.
func (m *Mirror) AppendEvents(sess Session, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	dir := m.Dir(sess)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("store: open events.jsonl: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range events {
		ej := eventJSON{
			Seq:       e.Seq,
			Kind:      e.Kind,
			Payload:   e.Payload,
			CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		}
		if err := enc.Encode(ej); err != nil {
			return fmt.Errorf("store: encode event: %w", err)
		}
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("store: append events.jsonl: %w", err)
	}
	return nil
}

// WriteTranscript rewrites transcript.md from the full event list. It is
// meant to be called at turn boundaries, not per delta.
func (m *Mirror) WriteTranscript(sess Session, events []Event) error {
	dir := m.Dir(sess)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	md := RenderTranscript(sess, events)
	if err := os.WriteFile(filepath.Join(dir, "transcript.md"), []byte(md), 0o644); err != nil {
		return fmt.Errorf("store: write transcript.md: %w", err)
	}
	return nil
}

// RenderTranscript renders a session's event log as human-readable
// markdown. It is a pure function of sess and events: no wall-clock read, so
// the same log always renders identical bytes.
func RenderTranscript(sess Session, events []Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Session %s\n\n", sess.ID)
	fmt.Fprintf(&b, "- model: %s (effort %s, thinking %v)\n", sess.Model, sess.Effort, sess.Thinking)
	fmt.Fprintf(&b, "- workspace: %s\n", sess.Workspace)
	fmt.Fprintf(&b, "- permission mode: %s\n", sess.PermissionMode)
	fmt.Fprintf(&b, "- status: %s\n\n", sess.Status)

	subTurn := 0
	sawStart := false
	for _, e := range events {
		switch e.Kind {
		case KindSessionStarted:
			var p SessionStartedPayload
			_ = json.Unmarshal(e.Payload, &p)
			// The first session_started event is the opening task; any
			// later one is Runner.Resume appending a continuation, which
			// gets its own heading rather than reading as a second task.
			if !sawStart {
				fmt.Fprintf(&b, "## Task\n\n%s\n\n", p.OpeningMessage)
				sawStart = true
			} else {
				fmt.Fprintf(&b, "## Resumed\n\n%s\n\n", p.OpeningMessage)
			}
		case KindTurnStarted:
			var p TurnStartedPayload
			_ = json.Unmarshal(e.Payload, &p)
			subTurn = p.SubTurn
			fmt.Fprintf(&b, "## Sub-turn %d\n\n", subTurn)
		case KindReasoningDelta:
			// Full reasoning lives in events.jsonl; the transcript shows
			// only that reasoning happened, not its content, to stay
			// readable across long sessions.
		case KindContentDelta:
			var p ContentDeltaPayload
			_ = json.Unmarshal(e.Payload, &p)
			b.WriteString(p.Text)
		case KindToolCall:
			var p ToolCallPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n\n**tool call** `%s(%s)`\n", p.Name, p.Arguments)
		case KindToolResult:
			var p ToolResultPayload
			_ = json.Unmarshal(e.Payload, &p)
			label := "tool result"
			if p.IsError {
				label = "tool error"
			}
			fmt.Fprintf(&b, "\n```\n%s [%s]\n%s\n```\n", label, p.Name, p.Content)
		case KindToolDenied:
			var p ToolDeniedPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n**denied** `%s`: %s\n", p.Name, p.Rule)
		case KindUsage:
			var p UsagePayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n_usage: prompt %d (hit %d / miss %d), completion %d, cost $%.6f_\n",
				p.PromptTokens, p.PromptCacheHitTokens, p.PromptCacheMissTokens, p.CompletionTokens, p.CostUSD)
		case KindTurnFinished:
			b.WriteString("\n\n")
		case KindRunFinished:
			var p RunFinishedPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n## Run finished (%s)\n\n%s\n", p.Reason, p.Summary)
		case KindError:
			var p ErrorPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n## Error\n\n%s\n", p.Message)
		}
	}
	return b.String()
}

// ExportTo rebuilds sessionID's mirror under m from the database. It is the
// repair path after a crash between the DB commit and the disk write, and it
// is exercised in tests to prove the mirror matches what a live run wrote.
func ExportTo(ctx context.Context, s *Store, m *Mirror, sessionID string) error {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("store: export %s: %w", sessionID, err)
	}
	events, err := s.GetEvents(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("store: export %s: %w", sessionID, err)
	}
	if err := m.Init(sess, nil); err != nil {
		return err
	}
	if err := m.AppendEvents(sess, events); err != nil {
		return err
	}
	if err := m.WriteTranscript(sess, events); err != nil {
		return err
	}
	return m.UpdateSession(sess)
}
