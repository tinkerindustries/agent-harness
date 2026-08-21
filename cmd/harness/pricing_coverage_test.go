package main

import (
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
)

// TestEveryKnownModelIsPriced pins that every model in the model→provider
// table (internal/provider) — not just the ones a settings default happens to
// name, which internal/settings' own TestEveryDefaultModelIsPriced covers —
// has an entry in the shipped price table. Table.Cost errors on a model it
// has no entry for, and the caller that costs a session's usage keeps zero
// when that happens (the settings registry's own comment on
// google.vision_model makes the same point about the vision path), so a
// model a session can actually be started on (queue validation's only gate is
// provider.Known) but prices cannot look up would run for real and vanish
// from every cost figure in the UI rather than erroring.
//
// This lives in cmd/harness rather than internal/pricing or
// internal/provider: both packages "depend on nothing internal"
// (internal/CLAUDE.md), and a test joining them would be the first thing to
// break that — the same reasoning internal/settings/registry_test.go gives
// for keeping its own version of this check out of internal/pricing.
// cmd/harness already imports both in main.go, so it is where the two tables
// actually meet.
func TestEveryKnownModelIsPriced(t *testing.T) {
	table, err := pricing.Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("load the shipped price table: %v", err)
	}
	for _, model := range provider.KnownModels() {
		if _, ok := table.Models[model]; !ok {
			t.Errorf("%s is a known model (internal/provider) with no configs/prices.json entry — its spend would silently report as $0", model)
		}
	}
}

// TestGeminiCodingModelResolvesThroughTheCostLookup is the narrower,
// model-specific check the plan asks for directly
// (docs/GEMINI-INTEGRATION.md §7 Phase 5, "verify the shape the cost lookup
// expects matches"): gemini-3.7-flash must price through exactly the same
// Table.Cost call every other provider's usage does, with a real cache-hit,
// cache-miss and completion split, not just an entry present in the map.
func TestGeminiCodingModelResolvesThroughTheCostLookup(t *testing.T) {
	table, err := pricing.Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("load the shipped price table: %v", err)
	}
	cost, _, err := table.Cost("gemini-3.7-flash", time.Now(), 1000, 2000, 500)
	if err != nil {
		t.Fatalf("Cost(gemini-3.7-flash): %v", err)
	}
	if cost <= 0 {
		t.Errorf("Cost(gemini-3.7-flash) = %v, want > 0 for non-zero token counts", cost)
	}
}
