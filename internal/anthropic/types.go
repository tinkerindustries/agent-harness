package anthropic

import "encoding/json"

// The Messages API request and response vocabulary
// (docs/ANTHROPIC-INTEGRATION.md). Bodies are Go structs, never
// map[string]any, so identical values always serialise to identical bytes —
// the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2), matching internal/gemini's and internal/deepseek's
// own request types.

// MessagesRequest is the request body for POST /v1/messages. Field order
// below is what encoding/json emits, so the same value always produces the
// same bytes. There is no tool_choice: "auto" is the default and is what
// the agent loop wants, matching DeepSeek's and Gemini's own requests.
type MessagesRequest struct {
	Model        string          `json:"model"`
	System       []ContentBlock  `json:"system,omitempty"`
	Messages     []Message       `json:"messages"`
	MaxTokens    int             `json:"max_tokens"`
	Thinking     *ThinkingConfig `json:"thinking,omitempty"`
	OutputConfig *OutputConfig   `json:"output_config,omitempty"`
	Tools        []any           `json:"tools,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
}

// ThinkingConfig is the request's `thinking` field. This client always sends
// adaptive thinking with a summarised display and the preserved-thinking
// prefix check under the thinking-binding-controls-2026-08-01 beta
// (docs/ANTHROPIC-INTEGRATION.md, "Thinking and effort").
type ThinkingConfig struct {
	Type         string        `json:"type"`
	Display      string        `json:"display,omitempty"`
	BlockBinding *BlockBinding `json:"block_binding,omitempty"`
}

// BlockBinding is thinking.block_binding: how the API should react when a
// replayed thinking block's signature no longer matches the conversation
// prefix it was bound to.
type BlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

// OutputConfig carries the effort level. Anthropic's effort vocabulary
// (low/medium/high/xhigh/max) already matches wire.ChatIntent.Effort's
// values where wire defines one ("low", "high", "max"); everything else —
// "", "medium", "xhigh" — passes through verbatim, "" omitting the field
// and leaving the API's own default ("high") in force.
type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// Message is one entry of the request's `messages` array. Content is a
// json.RawMessage rather than []ContentBlock so an assistant turn captured
// with raw provider blocks (wire.Item.ProviderBlocks) can be replayed
// byte-for-byte without a decode-and-rebuild round trip — see
// blocksJSON and intent.go's raw-block handling.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ContentBlock is every content block shape this client builds or reads,
// carrying the union of their fields the way internal/gemini's own Content
// type does for its surface — one struct, the fields relevant to Type
// populated and the rest omitted. It is not used to represent an entire
// assistant turn captured from the API (that stays the literal
// json.RawMessage the stream produced, per Message.Content's doc comment);
// it is what this client builds by hand for a user message, a tool result,
// or the system prompt.
type ContentBlock struct {
	Type string `json:"type"`
	// Text is a text block's content.
	Text string `json:"text,omitempty"`
	// Source is an image block's payload.
	Source *ImageSource `json:"source,omitempty"`
	// ID and Name are a tool_use block's call id and tool name.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// Input is a tool_use block's arguments, a genuine JSON object rather
	// than the string wire.ToolCallFunc.Arguments carries — see
	// argumentsToObject.
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID, Content and IsError are a tool_result block's fields.
	// Content is always the array form here, never the bare-string
	// shorthand the API also accepts, so one shape covers a text-only
	// result and one carrying an image alike.
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	// Thinking and Signature are a thinking block's fields, used only by the
	// no-raw-blocks fallback path (intent.go's assistantBlocksFallback) —
	// the ordinary path replays a captured turn's raw blocks verbatim
	// instead of reconstructing this shape.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// CacheControl places a prompt-cache breakpoint on this block
	// (docs/ANTHROPIC-INTEGRATION.md, "Caching").
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ImageSource is an image content block's `source`: inline base64 bytes,
// the only form this harness ever sends (tool results and pasted
// attachments already carry a data URI, never a hosted URL).
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// CacheControl marks a prompt-cache breakpoint. TTL is never set, which
// defaults to the API's 5-minute cache — this harness's sessions are driven
// interactively, well inside that window, and never lean on the 1-hour
// tier.
type CacheControl struct {
	Type string `json:"type"`
}

// ToolDefinition is one entry of the request's `tools` array for a
// client-declared tool: name, description and input_schema directly on the
// object, with no nested `function` wrapper and no `type` field — the shape
// a custom tool takes on this surface, distinct from a server tool
// (ServerToolDefinition). Parameters stays json.RawMessage, matching
// wire.ToolFunction, so a tool's JSON Schema serialises byte-identically
// across requests (docs/DESIGN.md §3.2).
type ToolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

// ServerToolDefinition is a server tool entry in the `tools` array —
// web_search and web_fetch, appended after the client-declared array in a
// fixed order (docs/ANTHROPIC-INTEGRATION.md, "Tools").
type ServerToolDefinition struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Server tool type strings this client appends after the frozen tool array,
// in this fixed order, on every request.
const (
	ToolTypeWebSearch = "web_search_20260209"
	ToolTypeWebFetch  = "web_fetch_20260209"
)

// messagesResponse is the non-streaming response body — CreateChatCompletion's
// own shape, used only for the compaction summary. rawContentBlock reads
// every field any content block kind carries; compaction never needs the
// raw-block replay path (a compacted session starts fresh with the summary
// in its system prompt, never replaying a synthetic assistant turn), so this
// response is read generically rather than captured verbatim.
type messagesResponse struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Content      []rawContentBlock `json:"content"`
	Model        string            `json:"model"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	StopDetails  json.RawMessage   `json:"stop_details,omitempty"`
	Usage        *messagesUsage    `json:"usage"`
}

// rawContentBlock reads any response content block generically: the union
// of every field a text, thinking, redacted_thinking, tool_use,
// server_tool_use or *_tool_result block may carry. Used to build the unary
// CreateChatCompletion response and, in stream.go, as the shape one
// streamed block's fields are accumulated into before it is re-marshalled
// verbatim into the raw block array.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// messagesUsage is the response `usage` object. InputTokens is the tokens
// after the last cache breakpoint only (docs/ANTHROPIC-INTEGRATION.md,
// "Caching") — the total a request actually cost is the sum of all three.
type messagesUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// apiErrorBody is the {"type":"error","error":{"type":...,"message":...}}
// envelope every non-2xx response and every mid-stream error event carries
// (docs/ANTHROPIC-INTEGRATION.md, "Errors and retry").
type apiErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}
