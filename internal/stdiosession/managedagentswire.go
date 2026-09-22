package stdiosession

import "encoding/json"

// The payload shapes Anthropic's Managed Agents vocabulary puts on the pipe,
// as docs/STDIO-MANAGED-AGENTS.md settles them. The `ma` prefix is Go's
// problem, not the wire's, the same reason interactionswire.go's `iact`
// prefix exists: this package holds three vocabularies' own Usage and Error
// and the rest, and none of the prefixes reach a client.

// Notification method names. Anthropic's own event `type` becomes the
// JSON-RPC method, and `session_id` is added to every one, the way the
// other two dialects add their own run id.
const (
	notifyMASessionStatusRunning = "session.status_running"
	notifyMASessionStatusIdle    = "session.status_idle"
	notifyMASessionError         = "session.error"
	notifyMASessionUsage         = "session.usage"
	notifyMAAgentMessage         = "agent.message"
	notifyMAAgentThinking        = "agent.thinking"
	notifyMAAgentToolUse         = "agent.tool_use"
	notifyMAAgentToolResult      = "agent.tool_result"
	notifyMAAgentMCPToolUse      = "agent.mcp_tool_use"
	notifyMAAgentCustomToolUse   = "agent.custom_tool_use"
	notifyMASpanRequestStart     = "span.model_request_start"
	notifyMASpanRequestEnd       = "span.model_request_end"
	notifyMAEventStart           = "event_start"
	notifyMAEventDelta           = "event_delta"
	// notifyMAToolOutput is this protocol's own, unchanged in spirit from
	// the other two dialects' harness.tool_output: incremental stdout from a
	// tool that is still running.
	notifyMAToolOutput = "harness.tool_output"
)

// maEventKind names the two spans event_start/event_delta can preview —
// the two text channels a sub-turn can have open (docs/STDIO-MANAGED-AGENTS.md
// deviation 11: both stream text here, where Anthropic's own agent.thinking
// preview never does).
const (
	maEventKindMessage  = "message"
	maEventKindThinking = "thinking"
)

// maContent is one text or image block, Anthropic's own content union
// narrowed to the members this surface produces and accepts.
type maContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

func maTextContent(s string) []maContent {
	if s == "" {
		return nil
	}
	return []maContent{{Type: "text", Text: s}}
}

func maAppendText(parts *[]maContent, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == "text" {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, maContent{Type: "text", Text: text})
}

// --- sessions.create ---

// maCreateParams is Anthropic's create-session request body, narrowed to
// what this surface honours, plus the `harness` block. Every field the real
// surface carries and this process cannot honour is decoded far enough to
// be refused by name (docs/STDIO-MANAGED-AGENTS.md, "The `agent` field").
type maCreateParams struct {
	Agent         json.RawMessage   `json:"agent,omitempty"`
	EnvironmentID json.RawMessage   `json:"environment_id,omitempty"`
	InitialEvents []json.RawMessage `json:"initial_events,omitempty"`
	Tools         []Tool            `json:"tools,omitempty"`
	VaultIDs      json.RawMessage   `json:"vault_ids,omitempty"`
	Budget        json.RawMessage   `json:"budget,omitempty"`
	Stream        *bool             `json:"stream,omitempty"`
	Store         *bool             `json:"store,omitempty"`
	Harness       *maCreateHarness  `json:"harness,omitempty"`
}

// maCreateHarness is CreateHarness plus result_schema and max_sub_turns,
// spelled the same as the other two dialects' own harness block
// (docs/STDIO-MANAGED-AGENTS.md, `sessions.create`'s field table).
type maCreateHarness struct {
	CWD             string          `json:"cwd"`
	ResumeSessionID string          `json:"resume_session_id,omitempty"`
	PermissionMode  string          `json:"permission_mode,omitempty"`
	Deny            []string        `json:"deny,omitempty"`
	MaxSubTurns     json.RawMessage `json:"max_sub_turns,omitempty"`
	MessageID       string          `json:"message_id,omitempty"`
	Title           string          `json:"title,omitempty"`
	Description     string          `json:"description,omitempty"`
	ResultSchema    json.RawMessage `json:"result_schema,omitempty"`
}

func (h *maCreateHarness) toCreateHarness() *CreateHarness {
	if h == nil {
		return nil
	}
	return &CreateHarness{
		CWD: h.CWD, ResumeSessionID: h.ResumeSessionID, PermissionMode: h.PermissionMode,
		Deny: h.Deny, MaxSubTurns: h.MaxSubTurns, MessageID: h.MessageID,
		Title: h.Title, Description: h.Description,
	}
}

// maAgentSpec is the one shape `agent` is accepted in:
// {"type": "agent_with_overrides", "model": {"id", "effort"}}. Every other
// field on it is refused by name.
type maAgentSpec struct {
	Type       string           `json:"type"`
	ID         json.RawMessage  `json:"id,omitempty"`
	Version    json.RawMessage  `json:"version,omitempty"`
	System     json.RawMessage  `json:"system,omitempty"`
	Tools      json.RawMessage  `json:"tools,omitempty"`
	MCPServers json.RawMessage  `json:"mcp_servers,omitempty"`
	Skills     json.RawMessage  `json:"skills,omitempty"`
	Model      *maModelOverride `json:"model,omitempty"`
}

type maModelOverride struct {
	ID           string          `json:"id,omitempty"`
	Effort       string          `json:"effort,omitempty"`
	InferenceGeo json.RawMessage `json:"inference_geo,omitempty"`
}

const maAgentWithOverrides = "agent_with_overrides"

// maCreateResult is what sessions.create answers with.
type maCreateResult struct {
	Session maSession `json:"session"`
}

// maSession is Anthropic's Session resource, narrowed to the fields this
// surface populates.
type maSession struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	CreatedAt string            `json:"created_at,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
	Agent     *maAgentEcho      `json:"agent,omitempty"`
	Events    []maEventOut      `json:"events,omitempty"`
	Usage     *maUsage          `json:"usage,omitempty"`
	Error     *maError          `json:"error,omitempty"`
	Harness   *maSessionHarness `json:"harness,omitempty"`
}

type maAgentEcho struct {
	Model maModelEcho `json:"model"`
}

type maModelEcho struct {
	ID     string `json:"id"`
	Effort string `json:"effort,omitempty"`
}

// maSessionHarness is the harness extension on a session resource.
type maSessionHarness struct {
	TurnID              string          `json:"turn_id,omitempty"`
	Reason              string          `json:"reason,omitempty"`
	Text                string          `json:"text,omitempty"`
	Result              json.RawMessage `json:"result,omitempty"`
	SubTurns            int             `json:"sub_turns,omitempty"`
	UnappliedMessageIDs []string        `json:"unapplied_message_ids,omitempty"`
}

// maStopReason is session.status_idle's own stop_reason object.
type maStopReason struct {
	Type     string   `json:"type"`
	EventIDs []string `json:"event_ids,omitempty"`
}

// --- sessions.events ---

// maEventsParams is sessions.events' request body: one or more events posted
// against a session.
type maEventsParams struct {
	SessionID string            `json:"session_id"`
	Events    []json.RawMessage `json:"events"`
}

// maWireEvent is the union every element of maEventsParams.Events can be,
// flattened the way iactStep flattens Google's own step union.
type maWireEvent struct {
	Type            string          `json:"type"`
	Content         []maContent     `json:"content,omitempty"`
	CustomToolUseID string          `json:"custom_tool_use_id,omitempty"`
	IsError         bool            `json:"is_error,omitempty"`
	Harness         *maEventHarness `json:"harness,omitempty"`
}

type maEventHarness struct {
	TurnID    string `json:"turn_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

const (
	maEventUserMessage          = "user.message"
	maEventUserInterrupt        = "user.interrupt"
	maEventUserCustomToolResult = "user.custom_tool_result"
)

// maEventsResult is sessions.events' answer: one entry per posted event, in
// the order sent.
type maEventsResult struct {
	SessionID string             `json:"session_id"`
	Results   []maEventResultRow `json:"results"`
}

// maEventResultRow is one posted event's own outcome.
type maEventResultRow struct {
	Type            string          `json:"type"`
	Seq             int64           `json:"seq,omitempty"`
	CustomToolUseID string          `json:"custom_tool_use_id,omitempty"`
	Harness         *maEventHarness `json:"harness,omitempty"`
}

// --- sessions.get / sessions.delete ---

type maIDParams struct {
	SessionID string        `json:"session_id"`
	Harness   *maGetHarness `json:"harness,omitempty"`
}

type maGetHarness struct {
	TurnID string `json:"turn_id,omitempty"`
}

type maGetResult struct {
	Session maSession `json:"session"`
}

// --- handshake ---

type maInitializeResult struct {
	ServerInfo   ServerInfo           `json:"server_info"`
	Capabilities maServerCapabilities `json:"capabilities"`
	Models       []string             `json:"models"`
	DefaultModel string               `json:"default_model"`
	ModelDetails []maModelDetail      `json:"model_details,omitempty"`
}

// maServerCapabilities is what this process implements, in Anthropic's own
// capability names — a different shape from the other two dialects'
// (docs/STDIO-MANAGED-AGENTS.md, "`initialize` result").
type maServerCapabilities struct {
	Streaming       bool     `json:"streaming"`
	Events          bool     `json:"events"`
	Interrupt       bool     `json:"interrupt"`
	ResumeSession   bool     `json:"resume_session"`
	MCPServers      bool     `json:"mcp_servers"`
	CustomTools     bool     `json:"custom_tools"`
	PermissionModes []string `json:"permission_modes"`
}

// maModelDetail is one model's entry in the handshake. The effort set lands
// under `effort_levels`, this dialect's own key.
type maModelDetail struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"display_name,omitempty"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	EffortLevels        []string `json:"effort_levels,omitempty"`
}

// --- agent.custom_tool_use ---

// maCustomToolUse is agent.custom_tool_use's own payload.
type maCustomToolUse struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     toolArguments  `json:"input"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// maStepHarness is the harness extension every per-event notification
// carries: the turn id every notification gets under this dialect
// (docs/STDIO-MANAGED-AGENTS.md, "Notifications" — "Every notification
// carries harness.turn_id"), plus the sub-turn the other two dialects'
// harness block already carries.
type maStepHarness struct {
	TurnID  string `json:"turn_id,omitempty"`
	SubTurn int    `json:"sub_turn,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Rule names the permission rule that refused a call, on an
	// agent.tool_result standing in for a denial.
	Rule string `json:"rule,omitempty"`
	// Truncated says the result was cut to the tool output cap.
	Truncated bool `json:"truncated,omitempty"`
	// ChildTurnID is set on a Task tool's result: the sub-agent ran as its
	// own session, and this is the id to read it under.
	ChildTurnID string `json:"child_turn_id,omitempty"`
	// Source is where a user.message came from — "input", "append",
	// "reminder".
	Source string `json:"source,omitempty"`
	// MessageID echoes the client-supplied id.
	MessageID string `json:"message_id,omitempty"`
}

// --- agent.tool_use / agent.mcp_tool_use ---

type maToolUse struct {
	SessionID           string         `json:"session_id"`
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Input               toolArguments  `json:"input"`
	EvaluatedPermission string         `json:"evaluated_permission,omitempty"`
	Harness             *maStepHarness `json:"harness,omitempty"`
}

// --- agent.tool_result ---

type maToolResult struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id,omitempty"`
	ToolUseID string         `json:"tool_use_id"`
	Content   []maContent    `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// --- agent.message / agent.thinking ---

type maMessage struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id"`
	Content   []maContent    `json:"content"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

type maThinking struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id"`
	Thinking  []maContent    `json:"thinking"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// --- event_start / event_delta ---

type maEventStartPayload struct {
	SessionID string       `json:"session_id"`
	Event     maEventStart `json:"event"`
}

type maEventStart struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type maEventDeltaPayload struct {
	SessionID string      `json:"session_id"`
	EventID   string      `json:"event_id"`
	Delta     maEventText `json:"delta"`
}

type maEventText struct {
	Text string `json:"text"`
}

// --- span.model_request_start / _end ---

type maSpanStart struct {
	SessionID   string         `json:"session_id"`
	ID          string         `json:"id"`
	Model       string         `json:"model,omitempty"`
	InputTokens int            `json:"input_tokens,omitempty"`
	Harness     *maStepHarness `json:"harness,omitempty"`
}

type maSpanEnd struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// --- session.status_running / session.status_idle ---

type maStatusRunning struct {
	SessionID string         `json:"session_id"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

type maStatusIdle struct {
	SessionID  string         `json:"session_id"`
	StopReason maStopReason   `json:"stop_reason"`
	Harness    *maStepHarness `json:"harness,omitempty"`
}

// --- session.error ---

type maError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

type maErrorEvent struct {
	SessionID string         `json:"session_id"`
	Error     maError        `json:"error"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// --- session.usage ---

// maUsage is the usage object this dialect reports — its own vocabulary,
// since Anthropic's real Managed Agents usage shape is not one this process
// reads from live traffic (docs/plans/archive/claude-provider.md, "Not building").
type maUsage struct {
	InputTokens          int       `json:"input_tokens"`
	CacheReadInputTokens int       `json:"cache_read_input_tokens,omitempty"`
	OutputTokens         int       `json:"output_tokens"`
	TotalTokens          int       `json:"total_tokens"`
	Harness              *maUsageX `json:"harness,omitempty"`
}

type maUsageX struct {
	CostUSD  float64 `json:"cost_usd"`
	SubTurns int     `json:"sub_turns,omitempty"`
}

type maUsageEvent struct {
	SessionID string         `json:"session_id"`
	Usage     maUsage        `json:"usage"`
	Harness   *maStepHarness `json:"harness,omitempty"`
}

// --- harness.tool_output ---

type maToolOutput struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	Text      string `json:"text"`
}

// --- the assembled document sessions.get answers with ---

// maEventOut is one entry of the assembled session's `events` array — every
// per-event notification this translator ever emitted, flattened the way
// iactStep flattens Google's own step union, so sessions.get can answer from
// exactly what streamed.
type maEventOut struct {
	Type                string         `json:"type"`
	ID                  string         `json:"id,omitempty"`
	Name                string         `json:"name,omitempty"`
	Input               toolArguments  `json:"input,omitempty"`
	Content             []maContent    `json:"content,omitempty"`
	Thinking            []maContent    `json:"thinking,omitempty"`
	ToolUseID           string         `json:"tool_use_id,omitempty"`
	IsError             bool           `json:"is_error,omitempty"`
	EvaluatedPermission string         `json:"evaluated_permission,omitempty"`
	Harness             *maStepHarness `json:"harness,omitempty"`
}
