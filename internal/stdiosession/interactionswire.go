package stdiosession

import "encoding/json"

// The payload shapes Google's Interactions vocabulary puts on the pipe.
// Every one of them is a shape from Google's Interactions API reference,
// spelled in Google's snake_case, so a client written against
// <https://ai.google.dev/api/interactions> reads them without a translation
// table. Fields this harness adds ride in a `harness` sub-object rather than
// beside Google's own keys, so a client that ignores that one key is left
// with a document Google's schema still describes.
//
// The names carry an `iact` prefix because the Responses vocabulary lives in
// the same package and calls several of these something else — its Usage is
// not this Usage, and its Error is not this Error. The prefix is Go's
// problem, not the wire's: none of it reaches a client.

// Notification method names. They are the SSE event names of Google's
// streaming surface: on the HTTP surface these are the `event:` line of each
// frame, and here they are the JSON-RPC method, with the frame's `data` as
// the params. The `done` frame has no counterpart — a JSON-RPC stream has no
// [DONE] sentinel because interaction.completed already ends the interaction
// and the pipe stays open for the next one.
const (
	notifyIactCreated      = "interaction.created"
	notifyIactStatusUpdate = "interaction.status_update"
	notifyIactStepStart    = "step.start"
	notifyIactStepDelta    = "step.delta"
	notifyIactStepStop     = "step.stop"
	notifyIactCompleted    = "interaction.completed"
	notifyIactError        = "error"

	// notifyIactToolOutput is this protocol's own: incremental stdout from a
	// tool that is still running. Google has nothing for it because on
	// Google's surface the client runs the tools and already has the
	// output.
	notifyIactToolOutput = "harness.tool_output"
	// notifyIactUsage is this protocol's own: one request's token accounting,
	// as it happens. Google reports usage once, on interaction.completed,
	// which for an interaction spanning a hundred sub-turns is an hour late
	// for anything that wants to show spend as it accrues. The
	// interaction.completed total still arrives and is still authoritative.
	notifyIactUsage = "harness.usage"
)

// iactStep types, from Google's iactStep union. Only the ones this surface can
// produce are named; the server-side tool steps (google_search_call,
// code_execution_call, …) have no counterpart because this harness runs its
// own tools rather than Google's.
const (
	iactStepUserInput      = "user_input"
	iactStepThought        = "thought"
	iactStepModelOutput    = "model_output"
	iactStepFunctionCall   = "function_call"
	iactStepFunctionResult = "function_result"
)

// iactDelta types, from Google's step.delta vocabulary.
const (
	iactDeltaText             = "text"
	iactDeltaThoughtSummary   = "thought_summary"
	iactDeltaThoughtSignature = "thought_signature"
	iactDeltaArguments        = "arguments_delta"
)

// iactContent is one text or image block, Google's Content union narrowed to the
// two members this surface produces and accepts.
type iactContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

// iactTextContent builds the one-block content array a plain string becomes.
func iactTextContent(s string) []iactContent {
	if s == "" {
		return nil
	}
	return []iactContent{{Type: "text", Text: s}}
}

// iactStep is Google's iactStep union, flattened. Every member of the union this
// surface emits is covered by some subset of these fields, which is how
// Google's own JSON works: the `type` discriminator says which of them are
// meaningful.
//
// Harness carries the fields this protocol adds. It is absent on every step
// where there is nothing to add.
type iactStep struct {
	Type      string           `json:"type"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	CallID    string           `json:"call_id,omitempty"`
	Arguments json.RawMessage  `json:"arguments,omitempty"`
	Content   []iactContent    `json:"content,omitempty"`
	Summary   []iactContent    `json:"summary,omitempty"`
	Signature string           `json:"signature,omitempty"`
	Result    []iactContent    `json:"result,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Harness   *iactStepHarness `json:"harness,omitempty"`
}

// iactStepHarness is the harness extension block on a step.
type iactStepHarness struct {
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

// iactDelta is Google's step.delta payload's `delta` object, flattened the same
// way iactStep is.
type iactDelta struct {
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	Content   *iactContent `json:"content,omitempty"`
	Arguments string       `json:"arguments,omitempty"`
	Signature string       `json:"signature,omitempty"`
}

// iactUsage is Google's usage object, in the field names the live API returns.
type iactUsage struct {
	TotalTokens        int         `json:"total_tokens"`
	TotalInputTokens   int         `json:"total_input_tokens"`
	TotalCachedTokens  int         `json:"total_cached_tokens"`
	TotalOutputTokens  int         `json:"total_output_tokens"`
	TotalToolUseTokens int         `json:"total_tool_use_tokens"`
	TotalThoughtTokens int         `json:"total_thought_tokens"`
	Harness            *iactUsageX `json:"harness,omitempty"`
}

// iactUsageX is the harness extension on usage: what the tokens cost. Google
// reports tokens and prices them elsewhere; a parent showing spend per
// session would otherwise have to carry this harness's price table.
type iactUsageX struct {
	CostUSD  float64 `json:"cost_usd"`
	SubTurns int     `json:"sub_turns,omitempty"`
}

// iactInteraction is Google's iactInteraction resource, narrowed to the fields this
// surface populates.
type iactInteraction struct {
	ID     string      `json:"id"`
	Object string      `json:"object,omitempty"`
	Model  string      `json:"model,omitempty"`
	Status string      `json:"status"`
	Steps  []iactStep  `json:"steps,omitempty"`
	Usage  *iactUsage  `json:"usage,omitempty"`
	Errors []iactError `json:"errors,omitempty"`

	Created string `json:"created,omitempty"`
	Updated string `json:"updated,omitempty"`

	Harness *iactInteractionHarness `json:"harness,omitempty"`
}

// iactInteractionHarness is the harness extension on an interaction.
type iactInteractionHarness struct {
	// SessionID is the harness's own session id for this interaction. It is
	// what the transcript on disk is named after and what a bug report
	// should quote; the interaction id is derived from it.
	SessionID string `json:"session_id,omitempty"`
	// Reason is why the run ended — "complete", "no_tool_calls",
	// "complete_rejected", "cancelled". The status enum does not separate
	// an agent that called Complete from one that answered without a tool
	// call.
	Reason string `json:"reason,omitempty"`
	// Text is the final assistant message of the run.
	Text string `json:"text,omitempty"`
	// Result is the structured result the Complete tool returned, when the
	// interaction asked for one with response_format.
	Result json.RawMessage `json:"result,omitempty"`
	// SubTurns is how many sub-turns the run took.
	SubTurns int `json:"sub_turns,omitempty"`
	// UnappliedMessageIDs names, by the message_id each was appended under,
	// the steers the run ended without applying, in the order they were
	// committed. The harness withdrew them, so no later run applies them; a
	// client that still wants one sent starts a new run with it
	// (docs/STDIO-INTERACTIONS.md, "interactions.append").
	UnappliedMessageIDs []string `json:"unapplied_message_ids,omitempty"`
}

// iactError is Google's iactError shape: a string code and a human-readable message.
type iactError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// --- notification payloads ---

// iactEnvelope is the payload of interaction.created and
// interaction.completed.
type iactEnvelope struct {
	Interaction iactInteraction `json:"interaction"`
	EventType   string          `json:"event_type"`
}

// iactStatusUpdate is the payload of interaction.status_update.
type iactStatusUpdate struct {
	InteractionID string           `json:"interaction_id"`
	Status        string           `json:"status"`
	EventType     string           `json:"event_type"`
	Harness       *iactStepHarness `json:"harness,omitempty"`
}

// iactStepStart is the payload of step.start.
type iactStepStart struct {
	InteractionID string   `json:"interaction_id"`
	Index         int      `json:"index"`
	Step          iactStep `json:"step"`
	EventType     string   `json:"event_type"`
}

// iactStepDelta is the payload of step.delta.
type iactStepDelta struct {
	InteractionID string    `json:"interaction_id"`
	Index         int       `json:"index"`
	Delta         iactDelta `json:"delta"`
	EventType     string    `json:"event_type"`
}

// iactStepStop is the payload of step.stop.
type iactStepStop struct {
	InteractionID string `json:"interaction_id"`
	Index         int    `json:"index"`
	EventType     string `json:"event_type"`
}

// iactErrorEvent is the payload of the error notification.
type iactErrorEvent struct {
	InteractionID string    `json:"interaction_id,omitempty"`
	Error         iactError `json:"error"`
	EventType     string    `json:"event_type"`
}

// iactToolOutput is the payload of harness.tool_output.
type iactToolOutput struct {
	InteractionID string `json:"interaction_id"`
	CallID        string `json:"call_id"`
	Text          string `json:"text"`
	EventType     string `json:"event_type"`
}

// iactUsageEvent is the payload of harness.usage.
type iactUsageEvent struct {
	InteractionID string    `json:"interaction_id"`
	SubTurn       int       `json:"sub_turn"`
	Usage         iactUsage `json:"usage"`
	EventType     string    `json:"event_type"`
}
