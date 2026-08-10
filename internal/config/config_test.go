package config

import (
	"path/filepath"
	"testing"
)

func TestLoadHTTPLogConfig(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
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
