// Package config loads harness configuration from the environment. Defaults
// follow docs/MODELS.md; nothing here is a substitute for reading that file
// when a default needs to change.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultBaseURL     = "https://api.deepseek.com"
	defaultModel       = "deepseek-v4-pro"
	defaultFlashModel  = "deepseek-v4-flash"
	defaultEffort      = "high"
	defaultMaxTokens   = 48000
	defaultPriceTable  = "configs/prices.json"
	defaultDataDir     = "data"
	defaultMaxSubTurns = 100

	// defaultNATSURL matches docker-compose.yml's default client port.
	defaultNATSURL = "nats://127.0.0.1:4222"
	// defaultWorkerPoolSize is also MaxAckPending on the WORK consumer
	// (docs/DESIGN.md §4.10): the harness pulls only what it can run.
	defaultWorkerPoolSize = 4
	// defaultDeadlineMS is DESIGN.md §4.10's own example value for a work
	// request that omits deadline_ms.
	defaultDeadlineMS = 1_800_000
	// defaultModelConcurrencyPro and defaultModelConcurrencyFlash are the
	// account-wide ceilings docs/MODELS.md measured, not a per-installation
	// choice (docs/MODELS.md, "Concurrency is per-model and account-wide").
	defaultModelConcurrencyPro   = 500
	defaultModelConcurrencyFlash = 2500

	// defaultHTTPAddr binds loopback only. Transcripts carry workspace
	// paths, file contents, and command output, so the port is sensitive
	// even though it is read-only (docs/DESIGN.md §4.2).
	defaultHTTPAddr = "127.0.0.1:8080"
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
	DataDir     string
	MaxSubTurns int

	// NATSURL is the JetStream server harness serve connects to
	// (docs/DESIGN.md §4.10).
	NATSURL string
	// WorkspaceRoot is the directory each run's own workspace is created
	// under, one folder per session id holding that run's clones
	// (docs/DESIGN.md §4.10). Empty means every request fails before it
	// reaches the loop: an operator must name a directory the harness may
	// write into rather than it picking one.
	WorkspaceRoot string
	// WorkerPoolSize is both the worker pool's goroutine budget and the
	// WORK consumer's MaxAckPending, so JetStream stays the flow controller
	// (docs/DESIGN.md §4.10).
	WorkerPoolSize int
	// DefaultDeadlineMS bounds a work request's run when it omits
	// deadline_ms.
	DefaultDeadlineMS int
	// ModelConcurrencyPro and ModelConcurrencyFlash size the per-model
	// semaphore shared across the worker pool (docs/DESIGN.md §4.5,
	// docs/MODELS.md).
	ModelConcurrencyPro   int
	ModelConcurrencyFlash int

	// HTTPAddr is where harness serve's read-only browser surface listens
	// (docs/DESIGN.md §4.2). Loopback by default; widen it deliberately,
	// never by accident.
	HTTPAddr string
	// DevFrontendURL, when set, makes the HTTP server proxy every non-API
	// path to a running Vite dev server instead of serving the embedded
	// build (docs/DESIGN.md §4.8).
	DevFrontendURL string
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

	workerPoolSize, err := envInt("DEEPSEEK_WORKER_POOL_SIZE", defaultWorkerPoolSize)
	if err != nil {
		return Config{}, err
	}
	deadlineMS, err := envInt("DEEPSEEK_DEFAULT_DEADLINE_MS", defaultDeadlineMS)
	if err != nil {
		return Config{}, err
	}
	concurrencyPro, err := envInt("DEEPSEEK_MODEL_CONCURRENCY_PRO", defaultModelConcurrencyPro)
	if err != nil {
		return Config{}, err
	}
	concurrencyFlash, err := envInt("DEEPSEEK_MODEL_CONCURRENCY_FLASH", defaultModelConcurrencyFlash)
	if err != nil {
		return Config{}, err
	}

	return Config{
		APIKey:                apiKey,
		BaseURL:               envOr("DEEPSEEK_BASE_URL", defaultBaseURL),
		Model:                 envOr("DEEPSEEK_MODEL", defaultModel),
		FlashModel:            envOr("DEEPSEEK_FLASH_MODEL", defaultFlashModel),
		Effort:                envOr("DEEPSEEK_EFFORT", defaultEffort),
		Thinking:              thinking,
		MaxTokens:             maxTokens,
		PriceTablePath:        envOr("DEEPSEEK_PRICE_TABLE", defaultPriceTable),
		DataDir:               envOr("DEEPSEEK_DATA_DIR", defaultDataDir),
		MaxSubTurns:           maxSubTurns,
		NATSURL:               envOr("NATS_URL", defaultNATSURL),
		WorkspaceRoot:         os.Getenv("DEEPSEEK_WORKSPACE_ROOT"),
		WorkerPoolSize:        workerPoolSize,
		DefaultDeadlineMS:     deadlineMS,
		ModelConcurrencyPro:   concurrencyPro,
		ModelConcurrencyFlash: concurrencyFlash,
		HTTPAddr:              envOr("DEEPSEEK_HTTP_ADDR", defaultHTTPAddr),
		DevFrontendURL:        os.Getenv("DEEPSEEK_DEV_FRONTEND_URL"),
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// envList splits a comma-separated environment variable, dropping empty
// entries so a trailing comma or unset variable both yield nil rather than
// a slice containing "".
func envList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
