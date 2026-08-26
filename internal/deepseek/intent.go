package deepseek

import "github.com/mrgeoffrich/agent-harness/internal/wire"

// requestFromIntent turns the session's provider-neutral intent into
// DeepSeek's request shape: thinking mode as thinking:{type}, effort as
// reasoning_effort, tools as tools (docs/KIMI-INTEGRATION.md §4.1). Kimi K3
// will map the same wire.ChatIntent onto its own spellings — a top-level
// reasoning_effort and no thinking field — which is the whole point of the
// seam: the loop states intent and the provider spells it.
//
// The field order follows wire.ChatCompletionRequest's declaration order,
// which is the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2, docs/KIMI-INTEGRATION.md §4.2): building the request
// here rather than in internal/session must not move a single byte of the
// serialised body, and internal/deepseek/intent_test.go pins that the
// translation from intent matches the request the loop used to build
// directly.
func requestFromIntent(intent wire.ChatIntent) wire.ChatCompletionRequest {
	thinkingType := wire.ThinkingDisabled
	if intent.Thinking {
		thinkingType = wire.ThinkingEnabled
	}
	return wire.ChatCompletionRequest{
		Model:           intent.Model,
		Messages:        intent.Messages,
		Thinking:        &wire.ThinkingConfig{Type: thinkingType},
		ReasoningEffort: intent.Effort,
		MaxTokens:       intent.MaxTokens,
		Tools:           intent.Tools,
	}
}
