package session

import (
	"context"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Client is the narrow seam between the agent loop and a model provider's
// API client (docs/KIMI-INTEGRATION.md §4.1). It is declared here, where it
// is consumed, and implemented by the provider packages — *deepseek.Client
// today, Kimi K3's client next — with cmd/harness choosing the
// implementation when it builds the Runner. That is the same shape
// RunPublisher and RunController take (internal/CLAUDE.md, ARCHITECTURE.md):
// a narrow interface declared at the consumer, implemented elsewhere, wired
// in cmd/harness.
//
// The loop states intent (wire.ChatIntent) and the implementation turns it
// into its provider's request shape: DeepSeek's `thinking: {type}` and
// Kimi K3's top-level `reasoning_effort` are different spellings of the
// same intent, and nothing in this package knows which it is talking to.
// The provider's response quirks and usage mapping live behind the seam for
// the same reason: UsageSplit maps a provider's usage figures onto the
// cache-hit/cache-miss counts the cost model uses, and IsReasoningStarved
// and RepairArguments repair DeepSeek behaviours recorded in
// docs/OBSERVED.md — Kimi's implementation is free to do nothing.
type Client interface {
	// StreamChatCompletion sends one streaming completion expressing intent
	// and returns its channel of typed deltas (docs/DESIGN.md §4.3).
	StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error)

	// CreateChatCompletion sends one non-streaming completion expressing
	// intent — the compaction summary, in this package — and waits for the
	// full response.
	CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error)

	// UsageSplit maps one request's usage figures onto the cache-hit and
	// cache-miss counts the stored usage payload and the cost model use.
	// DeepSeek reports the two counts separately; Kimi K3 reports a single
	// cached_tokens figure and derives the split itself
	// (docs/KIMI-INTEGRATION.md §2).
	UsageSplit(usage *wire.Usage) (cacheHit, cacheMiss int)

	// CacheSlack is the churn detector's tolerance for this provider: the
	// largest miss over its prediction a healthy sub-turn may show before a
	// churn report fires (internal/cache/churn.go). It is an empirical bound
	// on the provider's over-prediction, not a property of its cache —
	// DeepSeek's 127 is the trailing partial 128-token block, Kimi K3's 512
	// the largest over-prediction measured so far (docs/OBSERVED.md).
	CacheSlack() int

	// IsReasoningStarved reports whether a completion hit its max_tokens
	// ceiling before producing any answer text — DeepSeek's quirk, and the
	// reason the loop retries at double the budget (docs/OBSERVED.md).
	IsReasoningStarved(finishReason, content string) bool

	// RepairArguments corrects a single misplaced brace in assembled
	// tool-call arguments — DeepSeek's quirk; Kimi's implementation is free
	// to do nothing (docs/OBSERVED.md).
	RepairArguments(finishReason, args string) (string, bool)
}
