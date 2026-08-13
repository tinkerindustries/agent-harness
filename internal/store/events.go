package store

import "encoding/json"

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
type ToolResultPayload struct {
	ToolCallID     string     `json:"tool_call_id"`
	Name           string     `json:"name"`
	Content        string     `json:"content"`
	IsError        bool       `json:"is_error,omitempty"`
	Truncated      bool       `json:"truncated,omitempty"`
	Diff           []DiffLine `json:"diff,omitempty"`
	ChildSessionID string     `json:"child_session_id,omitempty"`
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
type UsagePayload struct {
	SubTurn               int     `json:"sub_turn"`
	Attempt               int     `json:"attempt,omitempty"`
	PromptTokens          int     `json:"prompt_tokens"`
	PromptCacheHitTokens  int     `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int     `json:"prompt_cache_miss_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	ReasoningTokens       int     `json:"reasoning_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	ExpectedMissTokens    int     `json:"expected_miss_tokens"`
	ChurnPointIndex       *int    `json:"churn_point_index,omitempty"`
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
