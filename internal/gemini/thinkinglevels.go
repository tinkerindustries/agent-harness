package gemini

import "slices"

// Which thinking levels each model accepts.
//
// The levels are not a property of the API, they are a property of the
// model: gemini-3.7-flash rejects "minimal" with a 400 naming the three it
// takes, while gemini-3.6-flash and 3.5-flash-lite accept all four. Measured
// against the live API on 2026-09-08 (docs/OBSERVED.md) and matching
// third_party/gemini-docs/thinking.md, "Levels Supported".
//
// The table covers every Gemini model this repository names — the one
// internal/provider routes (gemini-3.7-flash) and the three more the price
// table carries, which google.vision_model may be pointed at. A model absent
// here is not refused: LevelsFor returns nil and the caller passes the level
// through for the API to judge, because a table that has not been updated
// for a new model must not be what stops it working.
var thinkingLevels = map[string][]string{
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
