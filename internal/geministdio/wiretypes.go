package geministdio

import "encoding/json"

// The payload shapes that cross the pipe. Every one of them is a shape from
// Google's Interactions API reference, spelled in Google's snake_case, so a
// client written against <https://ai.google.dev/api/interactions> reads them
// without a translation table. Fields this harness adds ride in a `harness`
// sub-object rather than beside Google's own keys, so a client that ignores
// that one key is left with a document Google's schema still describes.

// Notification method names. They are the SSE event names of Google's
// streaming surface: on the HTTP surface these are the `event:` line of each
// frame, and here they are the JSON-RPC method, with the frame's `data` as
// the params. The `done` frame has no counterpart — a JSON-RPC stream has no
// [DONE] sentinel because interaction.completed already ends the interaction
// and the pipe stays open for the next one.
const (
	NotifyInteractionCreated      = "interaction.created"
	NotifyInteractionStatusUpdate = "interaction.status_update"
	NotifyStepStart               = "step.start"
	NotifyStepDelta               = "step.delta"
	NotifyStepStop                = "step.stop"
	NotifyInteractionCompleted    = "interaction.completed"
	NotifyError                   = "error"

	// NotifyToolOutput is this protocol's own: incremental stdout from a
	// tool that is still running. Google has nothing for it because on
	// Google's surface the client runs the tools and already has the
	// output.
	NotifyToolOutput = "harness.tool_output"
	// NotifyUsage is this protocol's own: one request's token accounting,
	// as it happens. Google reports usage once, on interaction.completed,
	// which for an interaction spanning a hundred sub-turns is an hour late
	// for anything that wants to show spend as it accrues. The
	// interaction.completed total still arrives and is still authoritative.
	NotifyUsage = "harness.usage"
)

// Step types, from Google's Step union. Only the ones this surface can
// produce are named; the server-side tool steps (google_search_call,
// code_execution_call, …) have no counterpart because this harness runs its
// own tools rather than Google's.
const (
	StepUserInput      = "user_input"
	StepThought        = "thought"
	StepModelOutput    = "model_output"
	StepFunctionCall   = "function_call"
	StepFunctionResult = "function_result"
)

// Delta types, from Google's step.delta vocabulary.
const (
	DeltaText             = "text"
	DeltaThoughtSummary   = "thought_summary"
	DeltaThoughtSignature = "thought_signature"
	DeltaArguments        = "arguments_delta"
)

// Interaction statuses, from Google's Interaction.status enum.
const (
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
	StatusFailed     = "failed"
	StatusIncomplete = "incomplete"
)

// Content is one text or image block, Google's Content union narrowed to the
// two members this surface produces and accepts.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

// TextContent builds the one-block content array a plain string becomes.
func TextContent(s string) []Content {
	if s == "" {
		return nil
	}
	return []Content{{Type: "text", Text: s}}
}

// Step is Google's Step union, flattened. Every member of the union this
// surface emits is covered by some subset of these fields, which is how
// Google's own JSON works: the `type` discriminator says which of them are
// meaningful.
//
// Harness carries the fields this protocol adds. It is absent on every step
// where there is nothing to add.
type Step struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   []Content       `json:"content,omitempty"`
	Summary   []Content       `json:"summary,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Result    []Content       `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Harness   *StepHarness    `json:"harness,omitempty"`
}

// StepHarness is the harness extension block on a step.
type StepHarness struct {
	// SubTurn is which sub-turn of the agent loop produced this step. An
	// interaction here is a whole agentic run, so a client that wants to
	// group steps the way the harness's own transcript does needs the
	// number; Google's surface has no sub-turn because one interaction is
	// one model call there.
	SubTurn int `json:"sub_turn,omitempty"`
	// Rule names the permission rule that refused a call, on a
	// function_result step standing in for a denial.
	Rule string `json:"rule,omitempty"`
	// Truncated says the result was cut to the tool output cap.
	Truncated bool `json:"truncated,omitempty"`
	// ChildInteractionID is set on a Task tool's result: the sub-agent ran
	// as its own session, and this is the id to read it under.
	ChildInteractionID string `json:"child_interaction_id,omitempty"`
	// Source is where a user_input step came from — "input" for the
	// interaction's own input, "append" for one added mid-flight by
	// interactions.append, "reminder" for one the loop generated itself.
	Source string `json:"source,omitempty"`
	// MessageID echoes the client-supplied id from interactions.create or
	// interactions.append, on the user_input step that carries that input.
	MessageID string `json:"message_id,omitempty"`
}

// Delta is Google's step.delta payload's `delta` object, flattened the same
// way Step is.
type Delta struct {
	Type      string   `json:"type"`
	Text      string   `json:"text,omitempty"`
	Content   *Content `json:"content,omitempty"`
	Arguments string   `json:"arguments,omitempty"`
	Signature string   `json:"signature,omitempty"`
}

// Usage is Google's usage object, in the field names the live API returns.
type Usage struct {
	TotalTokens        int     `json:"total_tokens"`
	TotalInputTokens   int     `json:"total_input_tokens"`
	TotalCachedTokens  int     `json:"total_cached_tokens"`
	TotalOutputTokens  int     `json:"total_output_tokens"`
	TotalToolUseTokens int     `json:"total_tool_use_tokens"`
	TotalThoughtTokens int     `json:"total_thought_tokens"`
	Harness            *UsageX `json:"harness,omitempty"`
}

// UsageX is the harness extension on usage: what the tokens cost. Google
// reports tokens and prices them elsewhere; a parent showing spend per
// session would otherwise have to carry this harness's price table.
type UsageX struct {
	CostUSD  float64 `json:"cost_usd"`
	SubTurns int     `json:"sub_turns,omitempty"`
}

// Interaction is Google's Interaction resource, narrowed to the fields this
// surface populates.
type Interaction struct {
	ID     string  `json:"id"`
	Object string  `json:"object,omitempty"`
	Model  string  `json:"model,omitempty"`
	Status string  `json:"status"`
	Steps  []Step  `json:"steps,omitempty"`
	Usage  *Usage  `json:"usage,omitempty"`
	Errors []Error `json:"errors,omitempty"`

	Created string `json:"created,omitempty"`
	Updated string `json:"updated,omitempty"`

	Harness *InteractionHarness `json:"harness,omitempty"`
}

// InteractionHarness is the harness extension on an interaction.
type InteractionHarness struct {
	// SessionID is the harness's own session id for this interaction. It is
	// what the transcript on disk is named after and what a bug report
	// should quote; the interaction id is derived from it.
	SessionID string `json:"session_id,omitempty"`
	// Reason is why the run ended — "complete", "no_tool_calls",
	// "max_sub_turns", "complete_rejected", "cancelled". Google's status
	// enum does not separate an agent that finished from one that ran out of
	// sub-turns, and the difference decides whether a parent offers to
	// continue.
	Reason string `json:"reason,omitempty"`
	// Text is the final assistant message of the run.
	Text string `json:"text,omitempty"`
	// Result is the structured result the Complete tool returned, when the
	// interaction asked for one with response_format.
	Result json.RawMessage `json:"result,omitempty"`
	// SubTurns is how many sub-turns the run took.
	SubTurns int `json:"sub_turns,omitempty"`
}

// Error is Google's Error shape: a string code and a human-readable message.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// --- notification payloads ---

// interactionEnvelope is the payload of interaction.created and
// interaction.completed.
type interactionEnvelope struct {
	Interaction Interaction `json:"interaction"`
	EventType   string      `json:"event_type"`
}

// statusUpdate is the payload of interaction.status_update.
type statusUpdate struct {
	InteractionID string       `json:"interaction_id"`
	Status        string       `json:"status"`
	EventType     string       `json:"event_type"`
	Harness       *StepHarness `json:"harness,omitempty"`
}

// stepStart is the payload of step.start.
type stepStart struct {
	InteractionID string `json:"interaction_id"`
	Index         int    `json:"index"`
	Step          Step   `json:"step"`
	EventType     string `json:"event_type"`
}

// stepDelta is the payload of step.delta.
type stepDelta struct {
	InteractionID string `json:"interaction_id"`
	Index         int    `json:"index"`
	Delta         Delta  `json:"delta"`
	EventType     string `json:"event_type"`
}

// stepStop is the payload of step.stop.
type stepStop struct {
	InteractionID string `json:"interaction_id"`
	Index         int    `json:"index"`
	EventType     string `json:"event_type"`
}

// errorEvent is the payload of the error notification.
type errorEvent struct {
	InteractionID string `json:"interaction_id,omitempty"`
	Error         Error  `json:"error"`
	EventType     string `json:"event_type"`
}

// toolOutput is the payload of harness.tool_output.
type toolOutput struct {
	InteractionID string `json:"interaction_id"`
	CallID        string `json:"call_id"`
	Text          string `json:"text"`
	EventType     string `json:"event_type"`
}

// usageEvent is the payload of harness.usage.
type usageEvent struct {
	InteractionID string `json:"interaction_id"`
	SubTurn       int    `json:"sub_turn"`
	Usage         Usage  `json:"usage"`
	EventType     string `json:"event_type"`
}
