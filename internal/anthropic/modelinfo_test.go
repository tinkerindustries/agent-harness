package anthropic

import "testing"

func TestModelInfoKnownModels(t *testing.T) {
	for _, model := range []string{ModelOpus55, ModelSonnet55, ModelFable51} {
		if DisplayName(model) == "" {
			t.Errorf("DisplayName(%q) is empty", model)
		}
		if ContextWindowTokens(model) != 1_000_000 {
			t.Errorf("ContextWindowTokens(%q) = %d, want 1,000,000", model, ContextWindowTokens(model))
		}
		levels := EffortLevelsFor(model)
		want := []string{"low", "medium", "high", "xhigh", "max"}
		if len(levels) != len(want) {
			t.Fatalf("EffortLevelsFor(%q) = %v", model, levels)
		}
		for i, l := range want {
			if levels[i] != l {
				t.Errorf("EffortLevelsFor(%q)[%d] = %q, want %q", model, i, levels[i], l)
			}
			if !EffortSupported(model, l) {
				t.Errorf("EffortSupported(%q, %q) = false", model, l)
			}
		}
		if EffortSupported(model, "unlimited") {
			t.Errorf("EffortSupported(%q, \"unlimited\") = true, want false", model)
		}
	}
}

func TestModelInfoUnknownModel(t *testing.T) {
	if DisplayName("claude-haiku-4-5") != "" {
		t.Error("DisplayName for an unrouted model should be empty")
	}
	if ContextWindowTokens("claude-haiku-4-5") != 0 {
		t.Error("ContextWindowTokens for an unrouted model should be zero")
	}
	if EffortLevelsFor("claude-haiku-4-5") != nil {
		t.Error("EffortLevelsFor for an unrouted model should be nil")
	}
	// An unlisted model accepts anything rather than being refused on the
	// strength of a table nobody updated.
	if !EffortSupported("claude-haiku-4-5", "anything") {
		t.Error("EffortSupported for an unrouted model should default to true")
	}
}
