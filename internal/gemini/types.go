package gemini

import "strings"

// Content types for the items in an interaction's input array and in a
// response step's content.
const (
	ContentTypeText  = "text"
	ContentTypeImage = "image"
)

// InteractionRequest is the request body for POST /v1beta/interactions
// (docs/gemini-3.5-flash-ui-review-prompting.md's sources document this
// surface, so the JSON is snake_case throughout). Field order below is what
// encoding/json emits, so the same value always produces the same bytes.
// There is deliberately no temperature, top_p, or top_k field: the doc is
// explicit that Gemini 3.x must not receive them.
type InteractionRequest struct {
	Model             string            `json:"model"`
	SystemInstruction string            `json:"system_instruction,omitempty"`
	Input             []Content         `json:"input"`
	GenerationConfig  *GenerationConfig `json:"generation_config,omitempty"`
	// ResponseFormat asks for structured output. It is a top-level request
	// field, not part of generation_config (generation_config has no
	// response_mime_type), and it is optional: a caller that wants prose
	// omits it. Setting it means the answer comes back as bare JSON rather
	// than inside a ```json fence (measured against the live API: the fence
	// disappears when the type is set).
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Stream asks for the answer as server-sent events. Every call this
	// client makes sets it: see stream.go for why and for the frame
	// vocabulary.
	Stream bool `json:"stream,omitempty"`
}

// ResponseFormat is the top-level response_format request field: 'array',
// 'object', 'video', 'text', 'image', 'integer', 'string', 'number',
// 'boolean', and 'audio' are the accepted types.
//
// It carries a type and nothing else. There is no field for a JSON schema on
// this surface, so asking for a particular shape is a matter for the system
// instruction — and constraining the container costs the contents, which is
// why the vision tools' own call sends no response_format at all and parses
// tolerantly instead (internal/tools/vision.go, WithResponseFormat("")).
type ResponseFormat struct {
	Type string `json:"type"`
}

// Content is one item of an interaction's input: a text part or an image
// part. ContentTypeImage parts carry MIMEType, Data (base64), and an
// optional per-image Resolution; ContentTypeText parts carry Text.
type Content struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Data       string `json:"data,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// GenerationConfig holds the generation parameters this harness sets.
// thinking_level replaces the deprecated thinking_budget; the doc says
// mixing the two is a 400, so thinking_budget has no field here.
//
// MaxOutputTokens is openapi.json's max_output_tokens, added for the
// agentic path's requestFromIntent (intent.go), which sends
// intent.MaxTokens through it: a live measurement found it is honoured —
// max_output_tokens=50 answered 200 with status "incomplete" and 46 output
// tokens, against 678 uncapped — contradicting an earlier line in
// docs/OBSERVED.md that recorded a failed search for such a field as its
// absence. omitempty keeps this unset (and the vision path's Interact,
// which never sets it) serialising exactly as before this field existed.
type GenerationConfig struct {
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// InteractionResponse is the response body of an interactions call. The
// model's text sits in the steps whose Type is "model_output", in their
// content's text parts; Text() concatenates those. Usage, when the API
// returns it, is the call's token accounting.
type InteractionResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Steps  []Step `json:"steps"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Usage is one interactions call's token accounting, in the shape the live
// API returns it (verified sample: total_tokens 72 = total_input_tokens 15
// + total_output_tokens 1 + total_thought_tokens 56, with
// total_cached_tokens 0 and input_tokens_by_modality summing to the input
// count).
type Usage struct {
	TotalTokens           int             `json:"total_tokens"`
	TotalInputTokens      int             `json:"total_input_tokens"`
	InputTokensByModality []ModalityUsage `json:"input_tokens_by_modality,omitempty"`
	TotalCachedTokens     int             `json:"total_cached_tokens"`
	TotalOutputTokens     int             `json:"total_output_tokens"`
	TotalToolUseTokens    int             `json:"total_tool_use_tokens"`
	TotalThoughtTokens    int             `json:"total_thought_tokens"`
	RawPromptToken        int             `json:"raw_prompt_token"`
}

// ModalityUsage is one entry of usage.input_tokens_by_modality.
type ModalityUsage struct {
	Modality string `json:"modality"`
	Tokens   int    `json:"tokens"`
}

// TokenSplit maps this usage onto the harness's pricing shape — cache-hit
// input, cache-miss input, and completion — which is the shape
// pricing.Table.Cost expects. Two mappings are decisions, not guesses:
//
//   - Thinking tokens bill at the output rate. Google's pricing page labels
//     every output price "Output price (including thinking tokens)", so
//     total_thought_tokens and total_output_tokens are both output: the
//     billed completion is their sum (and the verified sample's total_tokens
//     is input + output + thought, confirming there is no third rate). The
//     thought half is also reported as the reasoning counter, mirroring how
//     DeepSeek's CompletionTokens already include its reasoning tokens.
//   - total_cached_tokens is a subset of total_input_tokens: a context-cache
//     hit is input the API served from cache, so the uncached input is the
//     difference. (The verified sample — input 15, cached 0 — agrees
//     trivially; the subset relationship is the API's documented contract
//     for context caching.)
//
// One thing this shape cannot express: Gemini's context caching also carries
// a per-hour storage charge that has no counterpart in the three-rate table,
// so a cost figure here covers the cached reads, not the cached storage.
func (u *Usage) TokenSplit() (cacheHit, cacheMiss, completion, reasoning int) {
	cacheHit = u.TotalCachedTokens
	cacheMiss = u.TotalInputTokens - u.TotalCachedTokens
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	completion = u.TotalOutputTokens + u.TotalThoughtTokens
	reasoning = u.TotalThoughtTokens
	return cacheHit, cacheMiss, completion, reasoning
}

// Step is one step of an interaction. Only model_output steps carry the
// model's answer; the others (thought, function_call, ...) are skipped by
// Text().
//
// Summary holds a thought step's reasoning summary. It arrives only when the
// request asks for it with generation_config.thinking_summaries, which this
// client does not yet do, and even then the model decides per call whether
// to produce one.
type Step struct {
	Type    string    `json:"type"`
	Content []Content `json:"content,omitempty"`
	Summary []Content `json:"summary,omitempty"`
}

// Text returns the model's output as the concatenation of every text part
// of every model_output step, or "" when there is none.
func (r *InteractionResponse) Text() string {
	var parts []string
	for _, s := range r.Steps {
		if s.Type != "model_output" {
			continue
		}
		for _, p := range s.Content {
			if p.Type == ContentTypeText && p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
