package gemini

import "encoding/json"

// The agentic request/response vocabulary: the Interactions surface as the
// session loop uses it, alongside types.go's InteractionRequest and
// InteractionResponse, which stay exactly as Interact left them for the
// vision tools. Field names and shapes here are pinned against
// third_party/gemini-docs/openapi.json's Step oneOf (FunctionCallStep,
// FunctionResultStep, ThoughtStep, ModelOutputStep, UserInputStep) and
// against the captured fixtures under testdata/, not guessed
// (docs/GEMINI-INTEGRATION.md §7 "Phase 4").

// Step type discriminators for the agentic input array and the steps a
// response returns (openapi.json "Step" oneOf; the harness only ever
// produces or reads the five named here — the rest of the oneOf is Gemini's
// built-in tools, out of scope per docs/GEMINI-INTEGRATION.md §8).
const (
	StepTypeUserInput      = "user_input"
	StepTypeModelOutput    = "model_output"
	StepTypeThought        = "thought"
	StepTypeFunctionCall   = "function_call"
	StepTypeFunctionResult = "function_result"
)

// ChatInteractionRequest is the request body for POST /v1beta/interactions
// on the agentic path: a Step-typed input array, a flattened tool array, and
// store:false always (docs/GEMINI-INTEGRATION.md §5.3). Field order below
// matches that section's worked example so the frozen head reads the same
// as the plan that specified it. Deliberately absent, matching DeepSeek's
// and Kimi's request shape: tool_choice (nested in GenerationConfig and never
// sent, docs/OBSERVED.md "tool_choice is nested in generation_config, not
// top-level"), and temperature/top_p/top_k (types.go's GenerationConfig
// already carries only ThinkingLevel, for the same reason it does for the
// vision path).
type ChatInteractionRequest struct {
	Model string `json:"model"`
	// SystemInstruction carries the frozen system prompt. Gemini has its own
	// top-level field for it, unlike DeepSeek/Kimi's OpenAI-format messages
	// array, where the system message is just the first entry
	// (docs/GEMINI-INTEGRATION.md §3).
	SystemInstruction string `json:"system_instruction,omitempty"`
	// Store is always false and never omitted: the zero value and "absent"
	// would both serialise to nothing, but the API's default is store:true,
	// so an omitted field would silently opt into the server-side state this
	// harness deliberately avoids (docs/GEMINI-INTEGRATION.md §5.3).
	Store bool `json:"store"`
	// Stream is never omitted either, for the same reason Store is not:
	// CreateChatCompletion explicitly wants stream:false — the genuinely
	// different unary response shape docs/OBSERVED.md's "Unary (stream:
	// false)" finding measured, not merely "omit the flag and hope the
	// server's default matches" — and StreamChatCompletion explicitly wants
	// stream:true, matching the vision path's own InteractionRequest.Stream,
	// which is likewise always set rather than ever relying on omission.
	Stream bool `json:"stream"`
	// Input is a []any of the step structs below (UserInputStep,
	// ModelOutputStep, ThoughtStep, FunctionCallStep, FunctionResultStep):
	// each step kind has its own field set and its own fixed field order, so
	// a heterogeneous slice still serialises byte-stably — encoding/json
	// marshals every element through its own concrete type, not through a
	// shared one that could reorder fields (docs/DESIGN.md §3.2).
	Input            []any             `json:"input"`
	Tools            []FunctionTool    `json:"tools,omitempty"`
	GenerationConfig *GenerationConfig `json:"generation_config,omitempty"`
}

// FunctionTool is one entry of the agentic tools array: flattened, unlike
// OpenAI-format's {type:"function", function:{name, description,
// parameters}} — Gemini puts name, description, and parameters directly on
// the tool object (docs/GEMINI-INTEGRATION.md §3, openapi.json "Function").
// Parameters stays json.RawMessage, matching wire.ToolFunction, so the JSON
// Schema a tool carries serialises byte-identically across requests rather
// than being reordered by a map[string]any round trip (docs/DESIGN.md §3.2).
type FunctionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// UserInputStep is a user_input step (openapi.json "UserInputStep").
type UserInputStep struct {
	Type    string    `json:"type"`
	Content []Content `json:"content,omitempty"`
}

// ModelOutputStep is a model_output step (openapi.json "ModelOutputStep").
type ModelOutputStep struct {
	Type    string    `json:"type"`
	Content []Content `json:"content,omitempty"`
}

// ThoughtStep is a thought step carrying only the opaque signature that must
// be replayed verbatim — never the summary, which is display text the model
// does not require back (docs/GEMINI-INTEGRATION.md §5.2, openapi.json
// "ThoughtStep"). A thought step may carry a signature with no summary even
// on the wire the API sends; the reverse (a summary this client would need
// to replay) never applies, since this client never asks for
// thinking_summaries.
type ThoughtStep struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

// FunctionCallStep is a function_call step. Arguments is a genuine JSON
// object on the wire in both directions (docs/OBSERVED.md, "Full
// function_call → function_result round trip"), carried as json.RawMessage
// so converting wire.ToolCallFunc.Arguments (a JSON string) into this shape
// never reorders its keys (docs/GEMINI-INTEGRATION.md §3, openapi.json
// "FunctionCallStep").
type FunctionCallStep struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// FunctionResultStep is a function_result step. Result is an array of typed
// content blocks rather than a bare string, which is what makes a tool
// result multimodal — the same Content type the vision path's Input array
// uses already covers the text and image shapes a result needs
// (docs/GEMINI-INTEGRATION.md §3, §5.3, openapi.json "FunctionResultStep").
// Name is optional on the wire; it is populated here when the request
// builder still has it from the function_call step this result answers, but
// its absence is not an error.
type FunctionResultStep struct {
	Type    string    `json:"type"`
	CallID  string    `json:"call_id"`
	Name    string    `json:"name,omitempty"`
	Result  []Content `json:"result"`
	IsError bool      `json:"is_error,omitempty"`
}

// chatStep is the union of every step-object field the unary decode path
// (CreateChatCompletion) needs to read back, keyed by Type. It is distinct
// from types.go's Step, which the vision path's decodeStream builds and
// which only ever needs Content and Summary: extending that shared type
// with function_call/function_result fields would mix two callers' concerns
// for no benefit, since decodeStream and Interact never see those step
// kinds (Interact sends no tools).
type chatStep struct {
	Type      string          `json:"type"`
	Content   []Content       `json:"content,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Summary   []Content       `json:"summary,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Result    []Content       `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// chatInteractionResponse is the unary (stream:false) response body:
// docs/OBSERVED.md's "Unary (stream: false)" finding is that the signature
// sits directly on the step object here, not nested in a delta — which is
// exactly the shape chatStep.Signature reads.
type chatInteractionResponse struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Steps  []chatStep `json:"steps"`
	Usage  *Usage     `json:"usage,omitempty"`
}
