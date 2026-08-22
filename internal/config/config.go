// Package config loads harness configuration from the environment. It holds
// the bootstrap values only: the data directory (which locates the database
// the settings themselves live in), the network addresses, the price table
// path, and the workspace root. Everything that used to be a default here —
// models, run budgets, tool limits, worker sizes, retention — now lives in
// the settings registry (internal/settings), so an operator can change it
// from the settings screen without a rebuild.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultBaseURL    = "https://api.deepseek.com"
	defaultPriceTable = "configs/prices.json"
	defaultDataDir    = "data"

	// defaultHTTPAddr binds loopback only. Transcripts carry workspace
	// paths, file contents, and command output, so the port is sensitive
	// even though it is read-only (docs/DESIGN.md §4.2).
	defaultHTTPAddr = "127.0.0.1:8080"
)

// Config is the harness's bootstrap configuration, read from the
// environment. Everything operator-tunable beyond these lives in the
// settings table (internal/settings/registry.go) and is read through a
// resolver; only the four values below stay environment-only, because they
// are bootstrap: DataDir locates the database the settings live in, and a
// bad address typed into a browser takes the service off the network with no
// way back in.
type Config struct {
	BaseURL        string
	PriceTablePath string

	// DataDir holds the SQLite database and the disk mirror
	// (docs/DESIGN.md §4.8): <DataDir>/harness.db, <DataDir>/sessions/...
	DataDir string

	// HTTPLogRoot is where raw HTTP exchanges are captured:
	// <DataDir>/http/<yyyy-mm-dd>/<session_id>/exchanges.jsonl.gz. The log
	// is primary, not derived: nothing rebuilds it, nothing reconstructs it
	// from elsewhere, and nothing prunes the tree. Every sub-turn resends
	// the whole message array, so a long run writes tens of megabytes
	// before compression.
	HTTPLogRoot string
	// HTTPLogEnabled defaults to on. "0" or "false" turns capture off and
	// installs no wrapper on the request path at all.
	HTTPLogEnabled bool

	// WorkspaceRoot is the directory each run's own workspace is created
	// under, one folder per session id holding that run's clones
	// (docs/DESIGN.md §4.10). Empty means every request fails before it
	// reaches the loop: an operator must name a directory the harness may
	// write into rather than it picking one.
	WorkspaceRoot string

	// Thinking is the default thinking-mode toggle for runs that omit it.
	// Unlike the model and effort defaults it stays an environment variable:
	// it is a per-process choice about the request shape, not a limit an
	// operator tunes per installation.
	Thinking bool

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
	thinking := true
	if v := os.Getenv("DEEPSEEK_THINKING"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEEPSEEK_THINKING: %w", err)
		}
		thinking = b
	}

	dataDir := envOr("DEEPSEEK_DATA_DIR", defaultDataDir)

	httpLog := true
	if v := os.Getenv("DEEPSEEK_HTTP_LOG"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEEPSEEK_HTTP_LOG: %w", err)
		}
		httpLog = b
	}

	return Config{
		BaseURL:        envOr("DEEPSEEK_BASE_URL", defaultBaseURL),
		PriceTablePath: envOr("DEEPSEEK_PRICE_TABLE", defaultPriceTable),
		DataDir:        dataDir,
		HTTPLogRoot:    filepath.Join(dataDir, "http"),
		HTTPLogEnabled: httpLog,
		Thinking:       thinking,
		WorkspaceRoot:  os.Getenv("DEEPSEEK_WORKSPACE_ROOT"),
		HTTPAddr:       envOr("DEEPSEEK_HTTP_ADDR", defaultHTTPAddr),
		DevFrontendURL: os.Getenv("DEEPSEEK_DEV_FRONTEND_URL"),
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
