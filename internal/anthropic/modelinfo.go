package anthropic

// The three models this client hosts (docs/plans/archive/claude-provider.md's "Not
// building" names every other Claude model as out of scope for this
// feature).
const (
	ModelOpus5   = "claude-opus-5"
	ModelSonnet5 = "claude-sonnet-5"
	ModelFable51 = "claude-fable-5-1"
)

// displayNames is a human-readable name for each model.
var displayNames = map[string]string{
	ModelOpus5:   "Claude Opus 5",
	ModelSonnet5: "Claude Sonnet 5",
	ModelFable51: "Claude Fable 5.1",
}

// contextWindowTokens is each model's total input token budget: 1,000,000
// for all three, per the memory's External facts table, read from
// <https://platform.claude.com/docs/en/about-claude/models/overview.md>.
var contextWindowTokens = map[string]int{
	ModelOpus5:   1_000_000,
	ModelSonnet5: 1_000_000,
	ModelFable51: 1_000_000,
}

// effortLevels is the effort set every model this client hosts accepts —
// all five (low/medium/high/xhigh/max), per
// <https://platform.claude.com/docs/en/build-with-claude/effort>'s
// per-model recommendation sections for Opus 5, Sonnet 5 and Fable 5.1.
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
