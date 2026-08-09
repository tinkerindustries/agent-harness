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
)

// EventKind is the tag on an Event row that says how to decode its payload.
type EventKind string

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

// UsagePayload is one sub-turn's token accounting plus the cache churn
// diagnostic (docs/CACHE.md).
type UsagePayload struct {
	PromptTokens          int     `json:"prompt_tokens"`
	PromptCacheHitTokens  int     `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int     `json:"prompt_cache_miss_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	ReasoningTokens       int     `json:"reasoning_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	ExpectedMissTokens    int     `json:"expected_miss_tokens"`
	ChurnPointIndex       *int    `json:"churn_point_index,omitempty"`
}

// TurnFinishedPayload closes out a sub-turn's assistant message.
type TurnFinishedPayload struct {
	FinishReason string `json:"finish_reason"`
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
