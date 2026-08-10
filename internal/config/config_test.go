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

// TestLoadKeepsBootstrapFieldsOnly pins that the run-budget, model, and
// worker defaults are no longer environment variables: Config carries only
// bootstrap fields, and Load succeeds (and ignores) an .env that still names
// the old variables. The moved defaults live in internal/settings' registry,
// and their resolved values are pinned there.
func TestLoadKeepsBootstrapFieldsOnly(t *testing.T) {
	// An operator's old .env naming the moved variables must not break Load,
	// and must not influence Config.
	t.Setenv("DEEPSEEK_MODEL", "deepseek-v4-pro")
	t.Setenv("DEEPSEEK_FLASH_MODEL", "deepseek-v4-flash")
	t.Setenv("DEEPSEEK_EFFORT", "high")
	t.Setenv("DEEPSEEK_MAX_TOKENS", "48000")
	t.Setenv("DEEPSEEK_MAX_SUB_TURNS", "400")
	t.Setenv("DEEPSEEK_DEFAULT_DEADLINE_MS", "3600000")
	t.Setenv("DEEPSEEK_WORKER_POOL_SIZE", "4")
	t.Setenv("DEEPSEEK_MODEL_CONCURRENCY_PRO", "500")
	t.Setenv("DEEPSEEK_MODEL_CONCURRENCY_FLASH", "2500")
	t.Setenv("DEEPSEEK_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Thinking != true {
		t.Errorf("Thinking = %v, want true", cfg.Thinking)
	}
	if cfg.DataDir == "" || cfg.NATSURL == "" || cfg.HTTPAddr == "" || cfg.BaseURL == "" {
		t.Errorf("bootstrap fields must survive Load: %+v", cfg)
	}
}
