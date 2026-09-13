package stdiosession

import "encoding/json"

// The payload shapes that cross the pipe. Every one of them is a shape from
// the OpenAI Responses API, spelled in that surface's own snake_case, so a
// client written against a Responses implementation reads them without a
// translation table. DeepSeek serves the same surface
// (third_party/deepseek-docs/api/create-response.md), which is what makes
// this protocol one vocabulary end to end: the item a parent reads here is
// the item internal/deepseek sends the provider.
//
// Fields this harness adds ride in a `harness` sub-object rather than beside
// the surface's own keys, so a client that ignores that one key is left with
// a document the Responses schema still describes.

// Notification method names. They are the semantic SSE event names of the
// Responses streaming surface: on the HTTP surface these are the `event:`
// line of each frame, and here they are the JSON-RPC method, with the
// frame's `data` as the params.
//
// There is no `[DONE]` on either — the Responses stream ends on
// response.completed, and here the pipe simply stays open for the next
// response.
const (
	NotifyResponseCreated       = "response.created"
	NotifyResponseInProgress    = "response.in_progress"
	NotifyOutputItemAdded       = "response.output_item.added"
	NotifyOutputItemDone        = "response.output_item.done"
	NotifyOutputTextDelta       = "response.output_text.delta"
	NotifyReasoningTextDelta    = "response.reasoning_text.delta"
	NotifyFunctionCallArgsDelta = "response.function_call_arguments.delta"
	NotifyResponseCompleted     = "response.completed"
	NotifyResponseFailed        = "response.failed"

	// NotifyToolOutput is this protocol's own: incremental stdout from a
	// tool that is still running. The Responses surface has nothing for it
	// because there the client runs the tools and already has the output.
	NotifyToolOutput = "harness.tool_output"
	// NotifyUsage is this protocol's own: one request's token accounting,
	// as it happens. The Responses surface reports usage once, on
	// response.completed, which for a response spanning a hundred sub-turns
	// is an hour late for anything showing spend as it accrues. The
	// response.completed total still arrives and is still authoritative.
	NotifyUsage = "harness.usage"
)

// Output item types. The first three are the surface's own; the last two are
// this protocol's, and are the price of a response being a whole agentic run
// rather than one model turn (docs/STDIO-PROTOCOL.md, "Deviations").
//
// On the HTTP surface a function_call_output is something the *client* sends
// back in the next request's input, and a user message only ever appears in
// input. Here the loop runs the tools itself and folds its own inputs, so
// both appear in the output — as the same items, in the same shapes, in the
// order they happened.
const (
	ItemMessage            = "message"
	ItemReasoning          = "reasoning"
	ItemFunctionCall       = "function_call"
	ItemFunctionCallOutput = "function_call_output"
)

// Content part types, from the Responses content-part union. The surface
// splits text by direction: what the model reads is input_text, what it
// wrote is output_text, and its chain-of-thought is reasoning_text.
const (
	PartInputText     = "input_text"
	PartOutputText    = "output_text"
	PartReasoningText = "reasoning_text"
	PartInputImage    = "input_image"
)

// Response statuses, from the Responses status enum. Cancelled is this
// protocol's own: the surface has no verb for interrupting a response, so it
// has no status for one either (docs/STDIO-PROTOCOL.md, "Deviations").
const (
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)

// ContentPart is one part of an item's content or output.
//
// ImageURL is a bare string, as it is on the Responses surface, and carries
// a base64 data URL. A parent rendering a screenshot a tool produced reads
// it directly rather than reassembling a mime type and a payload.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// TextPart builds the one-part content a plain string becomes, typed for the
// direction it travels.
func TextPart(kind, s string) []ContentPart {
	if s == "" {
		return nil
	}
	return []ContentPart{{Type: kind, Text: s}}
}

// OutputItem is the Responses output-item union, flattened. Every member
// this surface emits is covered by some subset of these fields, which is how
// the surface's own JSON works: the `type` discriminator says which of them
// are meaningful.
//
// Harness carries the fields this protocol adds, and is absent on every item
// where there is nothing to add.
type OutputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []ContentPart   `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    []ContentPart   `json:"output,omitempty"`
	Harness   *ItemHarness    `json:"harness,omitempty"`
}

// ItemHarness is the harness extension block on an output item.
type ItemHarness struct {
	// SubTurn is which sub-turn of the agent loop produced this item. A
	// response here is a whole agentic run, so a client that wants to group
	// items the way the harness's own transcript does needs the number; the
	// Responses surface has no sub-turn because one response is one model
	// call there.
	SubTurn int `json:"sub_turn,omitempty"`
	// IsError marks a function_call_output the tool failed to produce, or
	// one the permission policy refused. The Responses surface has no error
	// flag on a tool output because the client that ran the tool already
	// knows; here the harness ran it.
	IsError bool `json:"is_error,omitempty"`
	// Rule names the permission rule that refused a call, on the
	// function_call_output standing in for a denial.
	Rule string `json:"rule,omitempty"`
	// Truncated says the output was cut to the tool output cap.
	Truncated bool `json:"truncated,omitempty"`
	// ChildResponseID is set on a Task tool's output: the sub-agent ran as
	// its own session, and this is the id to read it under.
	ChildResponseID string `json:"child_response_id,omitempty"`
	// Source is where a user message item came from — "input" for the
	// response's own input, "append" for one added mid-flight by
	// responses.append, "reminder" for one the loop generated itself.
	Source string `json:"source,omitempty"`
	// MessageID echoes the client-supplied id from responses.create or
	// responses.append, on the user message item that carries that input.
	MessageID string `json:"message_id,omitempty"`
}

// Usage is the Responses usage object, in the surface's own field names.
type Usage struct {
	InputTokens         int          `json:"input_tokens"`
	InputTokensDetails  InputDetails `json:"input_tokens_details"`
	OutputTokens        int          `json:"output_tokens"`
	OutputTokensDetails OutputDetail `json:"output_tokens_details"`
	TotalTokens         int          `json:"total_tokens"`
	Harness             *UsageX      `json:"harness,omitempty"`
}

// InputDetails breaks down the input tokens.
type InputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputDetail breaks down the output tokens.
type OutputDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// UsageX is the harness extension on usage: what the tokens cost. The
// Responses surface reports tokens and prices them elsewhere; a parent
// showing spend per session would otherwise have to carry this harness's
// price table.
type UsageX struct {
	CostUSD  float64 `json:"cost_usd"`
	SubTurns int     `json:"sub_turns,omitempty"`
}

// Response is the Responses `response` resource, narrowed to the fields this
// surface populates.
type Response struct {
	ID        string       `json:"id"`
	Object    string       `json:"object,omitempty"`
	CreatedAt int64        `json:"created_at,omitempty"`
	Status    string       `json:"status"`
	Model     string       `json:"model,omitempty"`
	Output    []OutputItem `json:"output,omitempty"`
	Usage     *Usage       `json:"usage,omitempty"`
	Error     *Error       `json:"error,omitempty"`

	Harness *ResponseHarness `json:"harness,omitempty"`
}

// ResponseHarness is the harness extension on a response.
type ResponseHarness struct {
	// SessionID is the harness's own session id for this response. It is
	// what the transcript on disk is named after and what a bug report
	// should quote; the response id is derived from it.
	SessionID string `json:"session_id,omitempty"`
	// Reason is why the run ended — "complete", "no_tool_calls",
	// "max_sub_turns", "complete_rejected", "cancelled". The status enum
	// does not separate an agent that finished from one that ran out of
	// sub-turns, and the difference decides whether a parent offers to
	// continue.
	Reason string `json:"reason,omitempty"`
	// Text is the final assistant message of the run — the surface's own
	// `output_text` convenience, computed here so a parent does not have to
	// walk the output items for it.
	Text string `json:"text,omitempty"`
	// Result is the structured result the Complete tool returned, when the
	// response asked for one with text.format.
	Result json.RawMessage `json:"result,omitempty"`
	// SubTurns is how many sub-turns the run took.
	SubTurns int `json:"sub_turns,omitempty"`
	// UpdatedAt is when the response last changed, which a long agentic run
	// has and a single model call does not.
	UpdatedAt string `json:"updated_at,omitempty"`
	// UnappliedMessageIDs names, by the message_id each was appended under,
	// the steers the run ended without applying, in the order they were
	// committed. The harness withdrew them, so no later run applies them; a
	// client that still wants one sent starts a new run with it
	// (docs/STDIO-PROTOCOL.md, "responses.append").
	UnappliedMessageIDs []string `json:"unapplied_message_ids,omitempty"`
}

// Error is the Responses error shape: a string code and a human-readable
// message.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// --- notification payloads ---
//
// Each carries `type` echoing its own method name, the way every frame on
// the HTTP surface does, plus `sequence_number`. A client reading the frames
// out of a queue can order them without depending on the transport.

// responseEnvelope is the payload of response.created, response.completed
// and response.failed.
type responseEnvelope struct {
	Type           string   `json:"type"`
	SequenceNumber int      `json:"sequence_number"`
	Response       Response `json:"response"`
}

// inProgress is the payload of response.in_progress. The surface sends it
// once; here it marks every sub-turn boundary, with the sub-turn in the
// harness block.
type inProgress struct {
	Type           string       `json:"type"`
	SequenceNumber int          `json:"sequence_number"`
	ResponseID     string       `json:"response_id"`
	Harness        *ItemHarness `json:"harness,omitempty"`
}

// itemEvent is the payload of response.output_item.added and .done.
type itemEvent struct {
	Type           string     `json:"type"`
	SequenceNumber int        `json:"sequence_number"`
	ResponseID     string     `json:"response_id"`
	OutputIndex    int        `json:"output_index"`
	Item           OutputItem `json:"item"`
}

// textDelta is the payload of response.output_text.delta,
// response.reasoning_text.delta and
// response.function_call_arguments.delta — the three that carry one
// incremental string against an open item.
type textDelta struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ResponseID     string `json:"response_id"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
}

// toolOutput is the payload of harness.tool_output.
type toolOutput struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ResponseID     string `json:"response_id"`
	CallID         string `json:"call_id"`
	Text           string `json:"text"`
}

// usageEvent is the payload of harness.usage.
type usageEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ResponseID     string `json:"response_id"`
	SubTurn        int    `json:"sub_turn"`
	Usage          Usage  `json:"usage"`
}
