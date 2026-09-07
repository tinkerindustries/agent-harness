package gemini

import (
	"slices"
	"testing"
)

// gemini-3.7-flash is the model this harness routes and the vision default,
// and it is the one that refuses "minimal". Measured against the live API:
// 400, "'minimal' is not a supported thinking level for this model. Allowed
// values are: medium, low, high."
func TestLevelsForFlash37ExcludesMinimal(t *testing.T) {
	levels := LevelsFor("gemini-3.7-flash")
	if slices.Contains(levels, ThinkingLevelMinimal) {
		t.Errorf("gemini-3.7-flash levels = %v, must not include minimal", levels)
	}
	for _, want := range []string{ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh} {
		if !slices.Contains(levels, want) {
			t.Errorf("gemini-3.7-flash levels = %v, want %s in it", levels, want)
		}
	}
	if LevelSupported("gemini-3.7-flash", ThinkingLevelMinimal) {
		t.Error("LevelSupported said gemini-3.7-flash takes minimal")
	}
	if !LevelSupported("gemini-3.7-flash", ThinkingLevelHigh) {
		t.Error("LevelSupported said gemini-3.7-flash does not take high")
	}
}

// The siblings do take it, which is why the table is per-model rather than
// one list.
func TestLevelsForSiblingsIncludeMinimal(t *testing.T) {
	for _, model := range []string{"gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite"} {
		if !LevelSupported(model, ThinkingLevelMinimal) {
			t.Errorf("%s: minimal should be supported", model)
		}
	}
}

// A model the table does not know is not refused: an out-of-date table must
// not be what stops a new model working.
func TestUnknownModelTakesAnyLevel(t *testing.T) {
	if LevelsFor("gemini-9.9-flash") != nil {
		t.Error("LevelsFor of an unknown model should be nil")
	}
	if !LevelSupported("gemini-9.9-flash", "whatever") {
		t.Error("an unknown model must accept any level")
	}
}

// The caller cannot edit the table through the slice it is handed.
func TestLevelsForReturnsACopy(t *testing.T) {
	levels := LevelsFor("gemini-3.7-flash")
	levels[0] = "clobbered"
	if LevelsFor("gemini-3.7-flash")[0] == "clobbered" {
		t.Error("LevelsFor handed out the table's own slice")
	}
}

// The default vision model must be one the table knows, or the vision tools
// send a level nothing has checked.
func TestDefaultModelIsInTheTable(t *testing.T) {
	if LevelsFor(DefaultModel) == nil {
		t.Errorf("DefaultModel %q has no thinking-level entry", DefaultModel)
	}
}
