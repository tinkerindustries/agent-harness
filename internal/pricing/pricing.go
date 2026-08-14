// Package pricing loads the DeepSeek price table from config and computes
// cost from token counts. Rates change on DeepSeek's schedule, not the
// harness's, so they are never compiled in (docs/DESIGN.md §4.9).
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
)

// ModelPrices are USD rates per one million tokens for a single model.
// Source and CapturedAt are optional per-model provenance for rates that
// come from a different page or date than the table's own top-level fields
// — the Gemini entries, whose prices come from Google's pricing page while
// the table's top-level source names DeepSeek's. Absent per model, a cost
// readout falls back to the table's top-level date.
//
// Note is a dated warning about the rate itself, for the case the Gemini
// entries are in: a price that is correct today because it is promotional,
// and that steps up on a known date with nothing in this package watching
// for it. It is a real field rather than a comment because JSON has no
// comments and an unknown key here is silently dropped — which is exactly
// how gemini-3.6-flash sat at its standard rate through an introductory
// period and over-reported every vision call by a factor of two.
type ModelPrices struct {
	InputCacheHitPerMillionUSD  float64 `json:"input_cache_hit_per_million_usd"`
	InputCacheMissPerMillionUSD float64 `json:"input_cache_miss_per_million_usd"`
	OutputPerMillionUSD         float64 `json:"output_per_million_usd"`
	Source                      string  `json:"source,omitempty"`
	CapturedAt                  string  `json:"captured_at,omitempty"`
	Note                        string  `json:"note,omitempty"`
}

// Table is a price table read from config. CapturedAt records when the
// rates were checked against the live pricing page, so a cost readout can
// show its own age rather than implying current accuracy it doesn't have.
type Table struct {
	CapturedAt string                 `json:"captured_at"`
	Source     string                 `json:"source"`
	Models     map[string]ModelPrices `json:"models"`
}

// Load reads and parses a price table from path.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read %s: %w", path, err)
	}
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	if t.CapturedAt == "" {
		return nil, fmt.Errorf("pricing: %s has no captured_at date", path)
	}
	return &t, nil
}

// Cost computes USD cost from token counts for model. completionTokens is
// the total output token count returned by the API; reasoning tokens and
// answer tokens bill at the same output rate, so reasoning is not charged
// separately from completionTokens.
func (t *Table) Cost(model string, cacheHitTokens, cacheMissTokens, completionTokens int) (float64, error) {
	p, ok := t.Models[model]
	if !ok {
		return 0, fmt.Errorf("pricing: no entry for model %q in table captured %s", model, t.CapturedAt)
	}
	cost := float64(cacheHitTokens)/1e6*p.InputCacheHitPerMillionUSD +
		float64(cacheMissTokens)/1e6*p.InputCacheMissPerMillionUSD +
		float64(completionTokens)/1e6*p.OutputPerMillionUSD
	return cost, nil
}
