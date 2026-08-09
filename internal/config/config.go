// Package config loads harness configuration from the environment. Defaults
// follow docs/MODELS.md; nothing here is a substitute for reading that file
// when a default needs to change.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

const (
	defaultBaseURL     = "https://api.deepseek.com"
	defaultModel       = "deepseek-v4-pro"
	defaultFlashModel  = "deepseek-v4-flash"
	defaultEffort      = "high"
	defaultMaxTokens   = 48000
	defaultPriceTable  = "configs/prices.json"
	defaultDataDir     = "data"
	defaultPermission  = "default"
	defaultMaxSubTurns = 100
)

// Config is the harness's runtime configuration, read from the environment.
type Config struct {
	APIKey         string
	BaseURL        string
	Model          string
	FlashModel     string
	Effort         string
	Thinking       bool
	MaxTokens      int
	PriceTablePath string

	// DataDir holds the SQLite database and the disk mirror
	// (docs/DESIGN.md §4.8): <DataDir>/harness.db, <DataDir>/sessions/...
	DataDir string
	// PermissionMode is the default mode for CLI-launched runs: readonly,
	// default, or full (docs/TOOLS.md).
	PermissionMode string
	MaxSubTurns    int
}

// Load reads Config from the environment. Call config.LoadDotEnv first if
// .env should be consulted.
func Load() (Config, error) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return Config{}, errors.New("DEEPSEEK_API_KEY is not set")
	}

	maxTokens := defaultMaxTokens
	if v := os.Getenv("DEEPSEEK_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEEPSEEK_MAX_TOKENS: %w", err)
		}
		maxTokens = n
	}

	thinking := true
	if v := os.Getenv("DEEPSEEK_THINKING"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEEPSEEK_THINKING: %w", err)
		}
		thinking = b
	}

	maxSubTurns := defaultMaxSubTurns
	if v := os.Getenv("DEEPSEEK_MAX_SUB_TURNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEEPSEEK_MAX_SUB_TURNS: %w", err)
		}
		maxSubTurns = n
	}

	return Config{
		APIKey:         apiKey,
		BaseURL:        envOr("DEEPSEEK_BASE_URL", defaultBaseURL),
		Model:          envOr("DEEPSEEK_MODEL", defaultModel),
		FlashModel:     envOr("DEEPSEEK_FLASH_MODEL", defaultFlashModel),
		Effort:         envOr("DEEPSEEK_EFFORT", defaultEffort),
		Thinking:       thinking,
		MaxTokens:      maxTokens,
		PriceTablePath: envOr("DEEPSEEK_PRICE_TABLE", defaultPriceTable),
		DataDir:        envOr("DEEPSEEK_DATA_DIR", defaultDataDir),
		PermissionMode: envOr("DEEPSEEK_PERMISSION_MODE", defaultPermission),
		MaxSubTurns:    maxSubTurns,
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
