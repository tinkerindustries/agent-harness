package config

import (
	"path/filepath"
	"testing"
)

func TestLoadHTTPLogConfig(t *testing.T) {
	// The DeepSeek API key is no longer an environment variable — Load must
	// succeed without DEEPSEEK_API_KEY, so this test deliberately sets none.
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("DEEPSEEK_DATA_DIR", dataDir)

	t.Run("unset leaves capture on", func(t *testing.T) {
		t.Setenv("DEEPSEEK_HTTP_LOG", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.HTTPLogEnabled {
			t.Error("HTTPLogEnabled = false, want true with the variable unset")
		}
		if cfg.HTTPLogRoot != filepath.Join(dataDir, "http") {
			t.Errorf("HTTPLogRoot = %q, want %q", cfg.HTTPLogRoot, filepath.Join(dataDir, "http"))
		}
	})

	for _, v := range []string{"0", "false"} {
		t.Run("value "+v+" turns capture off", func(t *testing.T) {
			t.Setenv("DEEPSEEK_HTTP_LOG", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.HTTPLogEnabled {
				t.Errorf("HTTPLogEnabled = true with DEEPSEEK_HTTP_LOG=%q, want false", v)
			}
		})
	}
}

func TestLoadRunBudgetDefaults(t *testing.T) {
	t.Setenv("DEEPSEEK_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	t.Run("defaults", func(t *testing.T) {
		t.Setenv("DEEPSEEK_MAX_SUB_TURNS", "")
		t.Setenv("DEEPSEEK_DEFAULT_DEADLINE_MS", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MaxSubTurns != 400 {
			t.Errorf("MaxSubTurns = %d, want 400", cfg.MaxSubTurns)
		}
		if cfg.DefaultDeadlineMS != 3_600_000 {
			t.Errorf("DefaultDeadlineMS = %d, want 3600000", cfg.DefaultDeadlineMS)
		}
	})

	t.Run("environment overrides", func(t *testing.T) {
		t.Setenv("DEEPSEEK_MAX_SUB_TURNS", "600")
		t.Setenv("DEEPSEEK_DEFAULT_DEADLINE_MS", "7200000")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MaxSubTurns != 600 {
			t.Errorf("MaxSubTurns = %d, want 600", cfg.MaxSubTurns)
		}
		if cfg.DefaultDeadlineMS != 7_200_000 {
			t.Errorf("DefaultDeadlineMS = %d, want 7200000", cfg.DefaultDeadlineMS)
		}
	})
}
