package gemini

import "slices"

// Which thinking levels each model accepts.
//
// The levels are not a property of the API, they are a property of the
// model: gemini-3.7-flash rejects "minimal" with a 400 naming the three it
// takes, while gemini-3.6-flash and 3.5-flash-lite accept all four. Measured
// against the live API on 2026-09-08 (docs/OBSERVED.md) and matching
// third_party/gemini-docs/thinking.md, "Levels Supported". gemini-3.8-flash
// (released after that measurement) matches 3.7's shape — low/medium/high,
// no minimal — per ai.google.dev/gemini-api/docs/thinking's own "Controlling
// thinking" table, read 2026-09-08; unlike the other four entries here it is
// not yet confirmed against a live 400.
//
// The table covers every Gemini model this repository names — the five
// internal/provider routes and google.vision_model may be pointed at. A
// model absent here is not refused: LevelsFor returns nil and the caller
// passes the level through for the API to judge, because a table that has
// not been updated for a new model must not be what stops it working.
var thinkingLevels = map[string][]string{
	"gemini-3.8-flash":      {ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh},
	"gemini-3.7-flash":      {ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh},
	"gemini-3.6-flash":      {ThinkingLevelMinimal, ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh},
	"gemini-3.5-flash":      {ThinkingLevelMinimal, ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh},
	"gemini-3.5-flash-lite": {ThinkingLevelMinimal, ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh},
}

// LevelsFor returns the thinking levels model accepts, or nil for a model
// this table does not know.
func LevelsFor(model string) []string {
	levels, ok := thinkingLevels[model]
	if !ok {
		return nil
	}
	return slices.Clone(levels)
}

// LevelSupported reports whether model accepts level. A model the table does
// not know accepts anything, so an unlisted model reaches the API rather than
// being refused here on the strength of a table nobody updated.
func LevelSupported(model, level string) bool {
	levels, ok := thinkingLevels[model]
	if !ok {
		return true
	}
	return slices.Contains(levels, level)
}

// contextWindowTokens is each model's total input token budget, read from
// Google's own `models.get` endpoint (inputTokenLimit) against the live API
// on 2026-09-08 — the same measurement method docs/GEMINI-INTEGRATION.md's
// "Context limit ≥ 1,000,011 input tokens" finding used, but read directly
// from the model metadata endpoint rather than inferred from a request that
// happened not to fail. All four models this repository names share the one
// figure today; a model added later without an entry here reports zero,
// which InitializeResult.ModelDetails then omits rather than guesses at.
var contextWindowTokens = map[string]int{
	"gemini-3.8-flash":      1048576,
	"gemini-3.7-flash":      1048576,
	"gemini-3.6-flash":      1048576,
	"gemini-3.5-flash":      1048576,
	"gemini-3.5-flash-lite": 1048576,
}

// displayNames is a human-readable name for each model, read from the same
// `models.get` response's displayName field. A model absent here has none:
// callers fall back to the bare id rather than inventing one.
var displayNames = map[string]string{
	"gemini-3.8-flash":      "Gemini 3.8 Flash",
	"gemini-3.7-flash":      "Gemini 3.7 Flash",
	"gemini-3.6-flash":      "Gemini 3.6 Flash",
	"gemini-3.5-flash":      "Gemini 3.5 Flash",
	"gemini-3.5-flash-lite": "Gemini 3.5 Flash Lite",
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
