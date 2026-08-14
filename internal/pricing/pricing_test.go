package pricing

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testTable = `{
  "captured_at": "2026-08-09",
  "source": "https://api-docs.deepseek.com/quick_start/pricing",
  "models": {
    "deepseek-v4-flash": {
      "input_cache_hit_per_million_usd": 0.0028,
      "input_cache_miss_per_million_usd": 0.14,
      "output_per_million_usd": 0.28
    }
  }
}`

func writeTestTable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(testTable), 0o644); err != nil {
		t.Fatalf("write test table: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if table.CapturedAt != "2026-08-09" {
		t.Errorf("CapturedAt = %q, want 2026-08-09", table.CapturedAt)
	}
	if _, ok := table.Models["deepseek-v4-flash"]; !ok {
		t.Errorf("Models missing deepseek-v4-flash")
	}
}

func TestLoadMissingCapturedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{"models":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load with no captured_at: want error, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/prices.json"); err == nil {
		t.Fatal("Load of missing file: want error, got nil")
	}
}

func TestCost(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 512 cache-hit tokens, 97 cache-miss tokens, 238 completion tokens —
	// figures drawn from docs/OBSERVED.md's cache table and a real flash run.
	got, err := table.Cost("deepseek-v4-flash", 512, 97, 238)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	want := float64(512)/1e6*0.0028 + float64(97)/1e6*0.14 + float64(238)/1e6*0.28
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Cost = %.12f, want %.12f", got, want)
	}
}

func TestCostZeroTokens(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := table.Cost("deepseek-v4-flash", 0, 0, 0)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got != 0 {
		t.Errorf("Cost of zero tokens = %v, want 0", got)
	}
}

func TestCostUnknownModel(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := table.Cost("deepseek-v4-nonexistent", 1, 1, 1); err == nil {
		t.Fatal("Cost of unknown model: want error, got nil")
	}
}

// TestRepoTableCarriesGeminiModels pins that the repository's own price
// table keeps the Gemini models the ReviewScreenshot tool costs against,
// each with the per-model source and capture date the brief's convention
// asks for. A typo in the table is the kind of failure that only shows up
// as a silently zero Gemini cost, so it is pinned here instead.
func TestRepoTableCarriesGeminiModels(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	for _, model := range []string{"gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash", "gemini-3.7-flash"} {
		p, ok := table.Models[model]
		if !ok {
			t.Errorf("table is missing %q", model)
			continue
		}
		if p.Source != "https://ai.google.dev/gemini-api/docs/pricing" {
			t.Errorf("%s source = %q, want Google's pricing page", model, p.Source)
		}
		if p.CapturedAt == "" {
			t.Errorf("%s has no captured_at date", model)
		}
		for _, rate := range []float64{p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD} {
			if rate <= 0 {
				t.Errorf("%s has a non-positive rate: %v", model, rate)
			}
		}
	}
}

// TestRepoTableGeminiRates pins the exact Gemini rates against Google's
// pricing page, the way TestRepoTableCarriesKimiK3 pins Moonshot's.
//
// The loose test above — a positive rate, a source, a date — is what let
// gemini-3.6-flash sit at its standard $1.50/$7.50 through an introductory
// period billing $0.75/$3.75, over-reporting every 3.6 vision call by a
// factor of two for as long as it was set. A rate that is plausible is not
// a rate that is right, and the only version of this test that would have
// caught it is one that names the numbers.
//
// 3.7 and 3.6 Flash carry the SAME rates on purpose: 3.7 launched at 3.6's
// price. If a future capture makes them differ, that is a real change and
// this test should be updated to say so — not collapsed into one case.
func TestRepoTableGeminiRates(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	for _, tc := range []struct {
		model                 string
		hit, miss, out        float64
		introductoryUntil2027 bool
	}{
		// Introductory through 2026-12-31; these double on 2027-01-01.
		{"gemini-3.7-flash", 0.075, 0.75, 3.75, true},
		{"gemini-3.6-flash", 0.075, 0.75, 3.75, true},
		// No introductory period: these are the standard rates.
		{"gemini-3.5-flash", 0.15, 1.5, 9.0, false},
		{"gemini-3.5-flash-lite", 0.03, 0.3, 2.5, false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p, ok := table.Models[tc.model]
			if !ok {
				t.Fatalf("table is missing %q", tc.model)
			}
			if p.InputCacheHitPerMillionUSD != tc.hit || p.InputCacheMissPerMillionUSD != tc.miss || p.OutputPerMillionUSD != tc.out {
				t.Errorf("rates = (%v, %v, %v), want (%v, %v, %v)",
					p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD,
					tc.hit, tc.miss, tc.out)
			}
			// A promotional rate with nothing watching its expiry is the
			// failure this whole test exists for, so the entry has to say
			// so in the table itself rather than only in a commit message.
			if tc.introductoryUntil2027 && !strings.Contains(p.Note, "2027-01-01") {
				t.Errorf("note = %q, want it to name the 2027-01-01 step-up", p.Note)
			}
		})
	}
}

// TestRepoTableCarriesKimiK3 pins the repository's own price table keeping
// kimi-k3 with the exact rates from Moonshot's pricing page — 0.30 cache
// hit, 3.00 cache miss, 15.00 output per million tokens — and its own
// per-model source and capture date (third_party/kimi-docs/pricing/chat-k3.md).
// A typo here shows up as a silently wrong Kimi cost, so it is pinned.
func TestRepoTableCarriesKimiK3(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	p, ok := table.Models["kimi-k3"]
	if !ok {
		t.Fatalf("table is missing kimi-k3")
	}
	if p.InputCacheHitPerMillionUSD != 0.30 || p.InputCacheMissPerMillionUSD != 3.00 || p.OutputPerMillionUSD != 15.00 {
		t.Errorf("kimi-k3 rates = (%v, %v, %v), want (0.30, 3.00, 15.00)",
			p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD)
	}
	if p.Source != "https://platform.kimi.ai/docs/pricing/chat-k3" {
		t.Errorf("kimi-k3 source = %q, want Moonshot's K3 pricing page", p.Source)
	}
	if p.CapturedAt != "2026-08-13" {
		t.Errorf("kimi-k3 captured_at = %q, want 2026-08-13", p.CapturedAt)
	}
}
