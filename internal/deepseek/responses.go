package deepseek

import (
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// This file is DeepSeek's second dialect: the request body of
// `POST /responses`, the Responses API
// (third_party/deepseek-docs/api/create-response.md). It sits beside
// intent.go, which spells the same wire.ChatIntent as a Chat Completions
// body, and the two never mix — a session speaks one surface for its whole
// life, because the frozen prefix the prompt cache is built on is the
// serialised request head and the two dialects do not serialise alike
// (docs/DESIGN.md §3.2).
//
// Everything here is a Go struct with fixed field order, never a
// map[string]any, for that same reason: encoding/json emits fields in
// declaration order, so identical values always produce identical bytes.

// Item types in the `input` list. Only the five the harness produces are
// named; the API takes two more (custom_tool_call, web_search_call) that
// nothing here sends.
const (
	itemTypeMessage            = "message"
	itemTypeReasoning          = "reasoning"
	itemTypeFunctionCall       = "function_call"
	itemTypeFunctionCallOutput = "function_call_output"
)

// Content part types. The Responses API splits by direction where Chat
// Completions had one "text": model input is input_text, model output
// replayed back is output_text, and chain-of-thought is reasoning_text.
const (
	partInputText     = "input_text"
	partOutputText    = "output_text"
	partInputImage    = "input_image"
	partReasoningText = "reasoning_text"
)

// effortNone disables thinking mode. The Responses API has no separate
// toggle — where Chat Completions takes `thinking: {"type": "disabled"}`
// beside `reasoning_effort`, this surface folds both into one field, and
// "none" is the off position (third_party/deepseek-docs/guides/
// thinking_mode.md, "Thinking Mode Toggle and Effort Control").
const effortNone = "none"

// responsesRequest is the body for POST /responses.
//
// max_output_tokens is omitempty because the field is nullable and zero is
// not a legal value for it: DeepSeek answers a `max_output_tokens: 0` with
// "the valid range of max_tokens is [1, 393216]". An intent that names no
// ceiling means the model's own default, so the field is absent rather than
// zero (measured 2026-09-10, docs/OBSERVED.md).
//
// Absent fields are absent on purpose. `tool_choice` is never sent for the
// reason it is never sent on the other surface (docs/DESIGN.md §4.4);
// `previous_response_id`, `conversation` and `store` are not supported by
// DeepSeek at all — the API is stateless and the harness sends the whole
// conversation every time, which is what its own event log already is.
type responsesRequest struct {
	Model           string           `json:"model"`
	Instructions    string           `json:"instructions,omitempty"`
	Input           []inputItem      `json:"input"`
	Reasoning       *reasoningConfig `json:"reasoning,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Tools           []responsesTool  `json:"tools,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
}

// reasoningConfig is the thinking mode toggle and effort in one field.
type reasoningConfig struct {
	Effort string `json:"effort"`
}

// inputItem is one entry of the `input` list. It is one struct covering
// every item kind rather than an interface per kind, so field order is
// fixed across kinds and a marshalled item cannot vary with which Go type
// produced it. Every field but Type is omitempty, so each kind emits only
// its own.
type inputItem struct {
	Type      string       `json:"type"`
	Role      string       `json:"role,omitempty"`
	Content   *itemContent `json:"content,omitempty"`
	CallID    string       `json:"call_id,omitempty"`
	Name      string       `json:"name,omitempty"`
	Arguments string       `json:"arguments,omitempty"`
	Output    *itemContent `json:"output,omitempty"`
}

// itemContent is the API's oneOf[string, array of parts], the same shape
// wire.Content carries for Chat Completions and marshalled the same way: a
// bare string when there are no parts, the array when there are. Text-only
// content is by far the common case and stays one string on the wire.
type itemContent struct {
	Text  string
	Parts []contentPart
}

// MarshalJSON emits the string form when Parts is empty and the array form
// otherwise.
func (c itemContent) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Parts)
}

// contentPart is one part of an itemContent.
//
// ImageURL is a plain string here, where Chat Completions wraps it in an
// object with its own `url` field. That difference is the kind of thing
// this whole file exists for: the same image, the same base64 data URL, a
// different envelope (third_party/deepseek-docs/api/create-response.md,
// "Image content part").
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// responsesTool is a function tool. The Responses API flattens what Chat
// Completions nests: name, description and parameters sit on the tool
// itself rather than under a `function` object.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responsesRequestFromIntent turns the loop's provider-neutral intent into a
// Responses API body. It is intent.go's requestFromIntent for the other
// surface, and the two must agree on meaning while agreeing on nothing at
// all about bytes.
func responsesRequestFromIntent(intent wire.ChatIntent) responsesRequest {
	instructions, items := inputFromMessages(intent.Messages)
	return responsesRequest{
		Model:           intent.Model,
		Instructions:    instructions,
		Input:           items,
		Reasoning:       &reasoningConfig{Effort: responsesEffort(intent)},
		MaxOutputTokens: intent.MaxTokens,
		Tools:           responsesToolsFrom(intent.Tools),
	}
}

// responsesEffort folds the loop's two reasoning controls into the one
// field this surface has. Thinking off is "none" whatever effort was asked
// for, because an effort is meaningless without thinking; thinking on with
// no effort named is "high", which is the API's own default and what
// Chat Completions gets by omitting reasoning_effort entirely.
func responsesEffort(intent wire.ChatIntent) string {
	if !intent.Thinking {
		return effortNone
	}
	if intent.Effort == "" {
		return wire.EffortHigh
	}
	return intent.Effort
}

// responsesToolsFrom flattens the tool array. A nil array stays nil, so a
// request with no tools omits the field rather than sending an empty list.
func responsesToolsFrom(tools []wire.Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

// inputFromMessages translates the folded message array into instructions
// plus an input item list.
//
// The shapes differ more than the names suggest, and three of the
// translations are load-bearing:
//
//   - The system prompt becomes `instructions` rather than a system message
//     item. The API inserts it as the first system message, so the model
//     sees exactly what it saw before.
//
//   - An assistant message carrying reasoning becomes **two** items, a
//     `reasoning` item followed by the message. That is not cosmetic: with
//     a `tools` array in the request — which every request of this harness
//     carries — DeepSeek requires the intermediate assistant's
//     chain-of-thought to be passed back in every later turn and answers
//     400 when it is not (third_party/deepseek-docs/guides/
//     thinking_mode.md, "Tool Calls"). Chat Completions replays it as
//     `reasoning_content` on the message; here it is an item of its own,
//     which the API merges back into the adjacent assistant message.
//
//   - A tool result becomes a `function_call_output` item whose `output`
//     may carry `input_image` parts. Chat Completions has no documented
//     home for an image in a tool message — the harness sends one anyway,
//     on measured behaviour its vendor's schema contradicts
//     (docs/DEEPSEEK-VISION.md §2). This surface documents it, so the
//     vision path stops resting on an undocumented allowance.
//
// An assistant message with no text emits no message item, only its
// reasoning and its calls: an empty assistant message would be a turn the
// model never took.
func inputFromMessages(msgs []wire.Message) (instructions string, items []inputItem) {
	items = make([]inputItem, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case wire.RoleSystem:
			// Only the first system message becomes instructions; the
			// harness sends exactly one, at index 0. A second would be a
			// system message item, which the API takes, rather than being
			// silently concatenated into the first.
			if i == 0 && instructions == "" {
				instructions = m.Content.String()
				continue
			}
			items = append(items, inputItem{
				Type: itemTypeMessage, Role: wire.RoleSystem,
				Content: contentFor(m.Content, partInputText),
			})

		case wire.RoleUser:
			items = append(items, inputItem{
				Type: itemTypeMessage, Role: wire.RoleUser,
				Content: contentFor(m.Content, partInputText),
			})

		case wire.RoleAssistant:
			if m.ReasoningContent != nil && *m.ReasoningContent != "" {
				items = append(items, inputItem{
					Type: itemTypeReasoning,
					Content: &itemContent{Parts: []contentPart{
						{Type: partReasoningText, Text: *m.ReasoningContent},
					}},
				})
			}
			// Either form of content counts. Text-only is what the fold
			// produces today (wire.TextContent of the accumulated answer),
			// but a parts content carries its text in the parts and leaves
			// Content.Text empty, so testing the string alone would drop a
			// whole assistant turn silently.
			if m.Content.Text != "" || len(m.Content.Parts) > 0 {
				items = append(items, inputItem{
					Type: itemTypeMessage, Role: wire.RoleAssistant,
					Content: contentFor(m.Content, partOutputText),
				})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, inputItem{
					Type:      itemTypeFunctionCall,
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}

		case wire.RoleTool:
			items = append(items, inputItem{
				Type:   itemTypeFunctionCallOutput,
				CallID: m.ToolCallID,
				Output: contentFor(m.Content, partInputText),
			})
		}
	}
	return instructions, items
}

// contentFor turns one message's content into an item's content, using
// textPart as the type of its text parts — input_text for anything the
// model reads, output_text for its own words being replayed. Text-only
// content stays a bare string, which is what all but the vision path sends.
func contentFor(c wire.Content, textPart string) *itemContent {
	if len(c.Parts) == 0 {
		return &itemContent{Text: c.Text}
	}
	out := &itemContent{Parts: make([]contentPart, 0, len(c.Parts))}
	for _, p := range c.Parts {
		switch p.Type {
		case wire.PartTypeImageURL:
			if p.ImageURL == nil {
				continue
			}
			out.Parts = append(out.Parts, contentPart{Type: partInputImage, ImageURL: p.ImageURL.URL})
		default:
			out.Parts = append(out.Parts, contentPart{Type: textPart, Text: p.Text})
		}
	}
	return out
}

// usageFromResponses maps the Responses API's usage object onto the one the
// cost model and the stored payload use.
//
// The cache split is derived rather than read. Chat Completions reports
// prompt_cache_hit_tokens and prompt_cache_miss_tokens as two figures; this
// surface reports one input total with the hit broken out under
// input_tokens_details, so the miss is the remainder — the same derivation
// internal/kimi does for the same reason (docs/KIMI-INTEGRATION.md §2).
func usageFromResponses(u *responsesUsage) *wire.Usage {
	if u == nil {
		return nil
	}
	hit := u.InputTokensDetails.CachedTokens
	miss := u.InputTokens - hit
	if miss < 0 {
		miss = 0
	}
	out := &wire.Usage{
		PromptTokens:          u.InputTokens,
		PromptCacheHitTokens:  hit,
		PromptCacheMissTokens: miss,
		CompletionTokens:      u.OutputTokens,
		TotalTokens:           u.TotalTokens,
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &wire.CompletionTokensDetails{
			ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
		}
	}
	return out
}

// finishReasonFor maps a terminal response object onto the finish reason
// vocabulary the loop already understands (wire.Finish*), so nothing above
// the seam learns that this surface reports completion as a status and an
// incomplete_details reason rather than as one string.
func finishReasonFor(r *responseObject) string {
	if r == nil {
		return wire.FinishStop
	}
	switch r.Status {
	case statusIncomplete:
		if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter" {
			return wire.FinishContentFilter
		}
		// max_output_tokens, and anything new: the loop's response to a
		// truncated answer is the right one for an unknown truncation too.
		return wire.FinishLength
	case statusCompleted:
		for _, item := range r.Output {
			if item.Type == itemTypeFunctionCall {
				return wire.FinishToolCalls
			}
		}
		return wire.FinishStop
	default:
		return wire.FinishStop
	}
}

// messageFromResponse rebuilds the assistant message a non-streaming
// response describes, for the callers that want the whole answer at once
// (compaction, the eval judge, WebFetch). The output items are flattened
// the way the streaming path accumulates them: reasoning into
// reasoning_content, message text into content, function calls into
// tool_calls.
func messageFromResponse(r *responseObject) wire.Message {
	var reasoning, content strings.Builder
	var calls []wire.ToolCall
	for _, item := range r.Output {
		switch item.Type {
		case itemTypeReasoning:
			for _, p := range item.Content {
				reasoning.WriteString(p.Text)
			}
		case itemTypeMessage:
			for _, p := range item.Content {
				content.WriteString(p.Text)
			}
		case itemTypeFunctionCall:
			calls = append(calls, wire.ToolCall{
				ID:       item.CallID,
				Type:     "function",
				Function: wire.ToolCallFunc{Name: item.Name, Arguments: item.Arguments},
			})
		}
	}
	msg := wire.Message{
		Role:      wire.RoleAssistant,
		Content:   wire.TextContent(content.String()),
		ToolCalls: calls,
	}
	if reasoning.Len() > 0 {
		r := reasoning.String()
		msg.ReasoningContent = &r
	}
	return msg
}
