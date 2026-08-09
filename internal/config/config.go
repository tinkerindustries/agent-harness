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
	defaultBaseURL    = "https://api.deepseek.com"
	defaultModel      = "deepseek-v4-pro"
	defaultEffort     = "high"
	defaultMaxTokens  = 48000
	defaultPriceTable = "configs/prices.json"
)

// Config is the harness's runtime configuration, read from the environment.
type Config struct {
	APIKey         string
	BaseURL        string
	Model          string
	Effort         string
	Thinking       bool
	MaxTokens      int
	PriceTablePath string
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

	return Config{
		APIKey:         apiKey,
		BaseURL:        envOr("DEEPSEEK_BASE_URL", defaultBaseURL),
		Model:          envOr("DEEPSEEK_MODEL", defaultModel),
		Effort:         envOr("DEEPSEEK_EFFORT", defaultEffort),
		Thinking:       thinking,
		MaxTokens:      maxTokens,
		PriceTablePath: envOr("DEEPSEEK_PRICE_TABLE", defaultPriceTable),
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
