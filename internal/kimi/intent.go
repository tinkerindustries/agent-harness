package kimi

import "github.com/mrgeoffrich/agent-harness/internal/wire"

// requestFromIntent turns the session's provider-neutral intent into Kimi
// K3's request shape: a top-level reasoning_effort and no thinking field at
// all (docs/KIMI-INTEGRATION.md §4.1). K3 always reasons — "Kimi K3 always
// enables thinking with Preserved Thinking" — and its effort is configured
// with the top-level reasoning_effort field; sending `thinking` to K3 is an
// error, so the intent's Thinking flag is deliberately ignored and no
// thinking config is ever emitted (third_party/kimi-docs/api/models-overview.md,
// guide/use-reasoning-effort.md).
//
// The field order follows wire.ChatCompletionRequest's declaration order,
// the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2, docs/KIMI-INTEGRATION.md §4.2). Sampling parameters
// are fixed server-side for K3 — temperature 1.0, top_p 0.95, n 1, both
// penalties 0 — and passing any of them is an error, so none is ever set
// here (third_party/kimi-docs/api/models-overview.md "Cannot be modified").
func requestFromIntent(intent wire.ChatIntent) wire.ChatCompletionRequest {
	return wire.ChatCompletionRequest{
		Model:           intent.Model,
		Messages:        wire.MessagesFromItems(intent.Items),
		ReasoningEffort: intent.Effort,
		MaxTokens:       intent.MaxTokens,
		Tools:           intent.Tools,
	}
}
