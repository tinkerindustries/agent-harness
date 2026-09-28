package anthropic

// The three models this client hosts (docs/plans/archive/claude-provider.md's "Not
// building" names every other Claude model as out of scope for this
// feature).
const (
	ModelOpus55   = "claude-opus-5-5"
	ModelSonnet55 = "claude-sonnet-5-5"
	ModelFable51  = "claude-fable-5-1"
)

// displayNames is a human-readable name for each model.
var displayNames = map[string]string{
	ModelOpus55:   "Claude Opus 5.5",
	ModelSonnet55: "Claude Sonnet 5.5",
	ModelFable51:  "Claude Fable 5.1",
}

// contextWindowTokens is each model's total input token budget: 1,000,000
// for all three, per the memory's External facts table, read from
// <https://platform.claude.com/docs/en/about-claude/models/overview.md>.
var contextWindowTokens = map[string]int{
	ModelOpus55:   1_000_000,
	ModelSonnet55: 1_000_000,
	ModelFable51:  1_000_000,
}

// defaultStreamMaxTokens and defaultUnaryMaxTokens are the `max_tokens` a
// request carries when its intent names no ceiling, which is every
// sub-turn stdio-session runs: the Responses vocabulary's
// `max_output_tokens` is optional and a parent that omits it leaves the
// intent at zero. Other providers read zero as "the model's own default"
// and leave the field out; the Messages API requires it and refuses zero
// (`400 invalid_request_error: stream cannot be true when max_tokens is
// 0`, docs/OBSERVED.md). All three models cap output at 128K. A streamed
// request takes half of that, room for adaptive thinking plus a large tool
// call. A unary one takes less, so the response returns before an HTTP
// timeout.
const (
	defaultStreamMaxTokens = 64_000
	defaultUnaryMaxTokens  = 16_000
)

// maxTokensOr returns requested when it names a ceiling and fallback when
// it does not.
func maxTokensOr(requested, fallback int) int {
	if requested > 0 {
		return requested
	}
	return fallback
}

// effortLevels is the effort set every model this client hosts accepts —
// all five (low/medium/high/xhigh/max), per
// <https://platform.claude.com/docs/en/build-with-claude/effort>'s
// per-model recommendation sections for Opus 5.5, Sonnet 5.5 and Fable 5.1.
// wire's own effort constants ("low", "high", "max") are a subset of this
// vocabulary; "medium" and "xhigh" have no wire constant and are passed
// through as literal strings.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// DisplayName returns a human-readable name for model, or "" for a model
// this table does not know.
func DisplayName(model string) string {
	return displayNames[model]
}

// ContextWindowTokens returns model's total input token budget, or zero for
// a model this table does not know.
func ContextWindowTokens(model string) int {
	return contextWindowTokens[model]
}

// EffortLevelsFor returns the effort levels model accepts, or nil for a
// model this table does not know.
func EffortLevelsFor(model string) []string {
	if _, ok := displayNames[model]; !ok {
		return nil
	}
	out := make([]string, len(effortLevels))
	copy(out, effortLevels)
	return out
}

// EffortSupported reports whether model accepts level. A model this table
// does not know accepts anything, so an unlisted model reaches the API
// rather than being refused here on the strength of a table nobody
// updated — internal/gemini.LevelSupported's own reasoning.
func EffortSupported(model, level string) bool {
	if _, ok := displayNames[model]; !ok {
		return true
	}
	for _, l := range effortLevels {
		if l == level {
			return true
		}
	}
	return false
}
