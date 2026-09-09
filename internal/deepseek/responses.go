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
	Input           []wire.Item      `json:"input"`
	Reasoning       *reasoningConfig `json:"reasoning,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Tools           []responsesTool  `json:"tools,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
}

// reasoningConfig is the thinking mode toggle and effort in one field.
type reasoningConfig struct {
	Effort string `json:"effort"`
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
// surface, and it has almost nothing to do.
//
// The loop's own conversation vocabulary **is** this surface's
// (wire.Item, internal/wire/item.go), so `input` is the intent's items
// serialised as they stand — no translation, no per-item rebuild, no second
// representation to keep in step. The only work is lifting the leading
// system item into `instructions`, which is where this surface carries the
// system prompt, and folding the loop's two reasoning controls into the one
// field this surface has.
//
// That is the whole point of the loop speaking items: the other three
// renderings (Chat Completions for DeepSeek and Kimi, Interactions for
// Gemini) do real work, and the path this repository's stdio entry point
// runs does none.
func responsesRequestFromIntent(intent wire.ChatIntent) responsesRequest {
	instructions, items := wire.SystemPromptOf(intent.Items)
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
			if item.Type == wire.ItemFunctionCall {
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
		case wire.ItemReasoning:
			for _, p := range item.Content {
				reasoning.WriteString(p.Text)
			}
		case wire.ItemMessage:
			for _, p := range item.Content {
				content.WriteString(p.Text)
			}
		case wire.ItemFunctionCall:
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
