// Package wire is the provider-neutral wire vocabulary of the Chat
// Completions API: the message and tool types, the request and response
// bodies, the streaming chunk types, the SSE scanner, the tool-call
// assembler, and the stream event vocabulary. Both providers produce and
// consume these unchanged — DeepSeek today, Kimi next — while the client and
// the per-provider behaviour live behind it in internal/deepseek
// (docs/KIMI-INTEGRATION.md §4.1). Request and response bodies are Go
// structs, never map[string]any: encoding/json marshals struct fields in
// declaration order, so identical values always serialise to identical
// bytes, which the prompt cache depends on (docs/DESIGN.md §3.2). The
// package imports nothing from internal/.
package wire

import "encoding/json"

// Message roles accepted by the API. "developer" is rejected; use
// RoleSystem instead.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Reasons the API reports for ending generation.
const (
	FinishStop                 = "stop"
	FinishLength               = "length"
	FinishContentFilter        = "content_filter"
	FinishToolCalls            = "tool_calls"
	FinishInsufficientResource = "insufficient_system_resource"
)

// Reasoning effort levels. "medium" and "xhigh" are accepted by the API and
// remapped server-side but are not meaningful values to request.
const (
	EffortLow  = "low"
	EffortHigh = "high"
	EffortMax  = "max"
)

// Thinking mode toggle values.
const (
	ThinkingEnabled  = "enabled"
	ThinkingDisabled = "disabled"
)

// Message is one entry in the messages array. Field order below is what
// encoding/json emits, so the same Message value always produces the same
// bytes. Content has no omitempty: an assistant message carrying tool_calls
// must serialise content as "" rather than being absent or null
// (docs/DESIGN.md §4.4).
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	Name             string     `json:"name,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// SystemMessage builds a system-role message.
func SystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}

// UserMessage builds a user-role message.
func UserMessage(content string) Message {
	return Message{Role: RoleUser, Content: content}
}

// ToolCall is a completed function call, as it appears in a non-streaming
// response or in an assistant message being replayed into a later request.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc is the name and JSON-encoded arguments of a ToolCall.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool describes a function the model may call. `harness ask` sends none,
// but the type exists so message round-tripping compiles against the same
// shape the agent loop uses.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool's schema. Parameters is raw JSON Schema, kept as
// json.RawMessage rather than map[string]any so a tool array serialises
// byte-stably across requests, matching the messages array.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ThinkingConfig toggles thinking mode.
type ThinkingConfig struct {
	Type string `json:"type"`
}

// StreamOptions controls what a streaming response includes.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatCompletionRequest is the request body for POST /chat/completions.
// tool_choice has no field here: thinking mode rejects "required" and named
// tool forcing, and "auto" is the default when tools are present, so there
// is never a reason to send it (docs/DESIGN.md §4.4).
type ChatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	MaxTokens       int             `json:"max_tokens"`
	Tools           []Tool          `json:"tools,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	StreamOptions   *StreamOptions  `json:"stream_options,omitempty"`
}

// Usage is token accounting for one request. CompletionTokens is the total
// billed as output; it already includes reasoning tokens, which are broken
// out separately for display in CompletionTokensDetails but are not an
// additional charge.
//
// How prefix caching is reported is a provider decision: DeepSeek splits it
// into PromptCacheHitTokens and PromptCacheMissTokens, while Kimi K3 reports
// a single CachedTokens and derives the split itself
// (docs/KIMI-INTEGRATION.md §2, third_party/kimi-docs/api/chat.md). CachedTokens
// is decoded for any provider and left at zero for the one that never sends
// it, so the same struct round-trips both bodies; it is omitempty because a
// zero value must not appear in any re-serialised DeepSeek body.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	PromptCacheHitTokens    int                      `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens   int                      `json:"prompt_cache_miss_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CachedTokens            int                      `json:"cached_tokens,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// CompletionTokensDetails breaks down CompletionTokens.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ChatCompletionResponse is the non-streaming response body.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage"`
}

// Choice is one completion candidate. The API returns exactly one.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatCompletionChunk is one streamed SSE data frame.
type ChatCompletionChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is one choice within a streamed chunk. FinishReason is nil
// until the final chunk of the choice.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is one streamed fragment of an assistant message. Content and
// ReasoningContent are pointers because the API sends Content as an
// explicit JSON null while reasoning is streaming, not as an absent or
// empty field; a bare string would make that indistinguishable from an
// empty delta (docs/OBSERVED.md).
type ChunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content"`
	ReasoningContent *string         `json:"reasoning_content"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is one fragment of a streamed tool call, keyed by Index.
// Only Index is guaranteed present on every frame; ID, Type, and
// Function.Name arrive once on the opening frame, and Function.Arguments
// arrives fragmented across many frames, mid-token and mid-string
// (docs/OBSERVED.md).
type ToolCallDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function ToolCallFuncDelta `json:"function"`
}

// ToolCallFuncDelta is the function-call portion of a ToolCallDelta.
type ToolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
