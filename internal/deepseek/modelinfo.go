package deepseek

import (
	"slices"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// This file is DeepSeek's answer to internal/gemini/thinkinglevels.go: the
// per-model facts a hosted session's handshake advertises to its parent
// (docs/STDIO-PROTOCOL.md, `model_details`). It is one more piece of the
// provider's own dialect, kept here for the same reason the request shape is
// (docs/KIMI-INTEGRATION.md §4.1) — the caller states intent, the provider
// spells it.

// advertisedEfforts is what a client may ask for, per model.
//
// DeepSeek's reasoning_effort takes low, high and max, and defaults to high
// (third_party/deepseek-docs/api/create-chat-completion.md). Unlike Gemini's
// thinking levels, the set does not differ between models: all three take the
// same three. The table is still keyed by model, because the reason Gemini's
// is — a model that accepts a different set — is a property of models rather
// than of APIs, and a fourth DeepSeek model that narrows the set should have
// somewhere to say so.
var advertisedEfforts = map[string][]string{
	"deepseek-v4-pro":              {wire.EffortLow, wire.EffortHigh, wire.EffortMax},
	"deepseek-v4-flash":            {wire.EffortLow, wire.EffortHigh, wire.EffortMax},
	"deepseek-v4-flash-vision-exp": {wire.EffortLow, wire.EffortHigh, wire.EffortMax},
}

// compatibilityEfforts are spellings DeepSeek accepts and maps onto high
// rather than rejecting, per the same page: "For compatibility, `medium` and
// `xhigh` are mapped to `high`." They are accepted and not advertised. The
// distinction matters to a parent speaking Google's vocabulary over
// docs/STDIO-PROTOCOL.md: `medium` is one of Google's four thinking levels,
// so a client that offers Google's set must not have its create refused on a
// level the API would have taken — while advertising `medium` on a model
// that silently treats it as `high` would promise a gradation that is not
// there.
//
// `minimal`, Google's fourth level, is deliberately absent: DeepSeek
// documents neither an effort of that name nor a mapping for one, so it is
// refused at create with the accepted set named, rather than reaching the
// API to fail a started run with a 400 the client never sees.
var compatibilityEfforts = []string{"medium", "xhigh"}

// EffortsFor returns the reasoning efforts model advertises, or nil for a
// model this table does not know.
func EffortsFor(model string) []string {
	efforts, ok := advertisedEfforts[model]
	if !ok {
		return nil
	}
	return slices.Clone(efforts)
}

// EffortSupported reports whether model accepts effort. A model the table
// does not know accepts anything, so an unlisted model reaches the API
// rather than being refused here on the strength of a table nobody updated —
// the same rule internal/gemini's LevelSupported follows, and for the same
// reason.
func EffortSupported(model, effort string) bool {
	efforts, ok := advertisedEfforts[model]
	if !ok {
		return true
	}
	return slices.Contains(efforts, effort) || slices.Contains(compatibilityEfforts, effort)
}

// contextWindowTokens is each model's total input token budget.
//
// The figure is DeepSeek's own published one — "CONTEXT LENGTH | 1M"
// (third_party/deepseek-docs/quick_start/pricing.md) — read decimally,
// because the same table prices tokens per 1M meaning 1,000,000. It is not a
// measurement: DeepSeek publishes no per-model metadata endpoint to read an
// exact limit from, the way internal/gemini's figures were read from
// Google's models.get. A client divides its latest sub-turn's input tokens
// by this to draw a context percentage, so a figure that is slightly low
// reports slightly full, which is the harmless direction to be wrong in.
var contextWindowTokens = map[string]int{
	"deepseek-v4-pro":              1000000,
	"deepseek-v4-flash":            1000000,
	"deepseek-v4-flash-vision-exp": 1000000,
}

// displayNames is a human-readable name for each model. DeepSeek publishes
// no displayName field, so these are the model versions its own news posts
// and pricing table use (third_party/deepseek-docs/news/news260821.md).
var displayNames = map[string]string{
	"deepseek-v4-pro":              "DeepSeek V4 Pro",
	"deepseek-v4-flash":            "DeepSeek V4 Flash",
	"deepseek-v4-flash-vision-exp": "DeepSeek V4 Flash Vision (experimental)",
}

// ContextWindowTokens returns model's total input token budget, or zero for
// a model this table does not know.
func ContextWindowTokens(model string) int {
	return contextWindowTokens[model]
}

// DisplayName returns a human-readable name for model, or "" for a model
// this table does not know.
func DisplayName(model string) string {
	return displayNames[model]
}
