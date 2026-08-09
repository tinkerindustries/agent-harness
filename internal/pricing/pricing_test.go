package pricing

import (
	"math"
	"os"
	"path/filepath"
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
