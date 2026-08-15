package config

import (
	"fmt"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

const (
	// defaultMCPPermissionCeiling imposes no restriction. permission_mode
	// is mandatory on every launch, so a caller cannot reach "full" without
	// naming it; the ceiling is an operator's opt-in limit on top of that.
	// Set it to readonly to refuse write runs from this server entirely.
	defaultMCPPermissionCeiling = "full"
	// defaultMCPFlashModel mirrors defaultFlashModel; kept separate so an
	// operator who renamed the harness's flash model can point the "flash"
	// profile at the same one without editing harness config.
	defaultMCPFlashModel = "deepseek-v4-flash"
	// defaultMCPAcceptedWaitMS is how long the launch tool waits for an
	// `accepted` message before reporting "queued" instead of "running".
	defaultMCPAcceptedWaitMS = 3_000
	// defaultHarnessBaseURL matches defaultHTTPAddr: the harness's own
	// read-only API, reachable on loopback when serve runs outside compose.
	defaultHarnessBaseURL = "http://127.0.0.1:8080"
)

// MCPConfig configures the /mcp mount `harness serve` serves on its own HTTP
// server (docs/DESIGN.md's MCP launch server). It is loaded separately from
// Config because it is the profile `harness serve` loads to configure its
// embedded MCP service — there is no standalone MCP process anymore. The MCP
// service never opens the SQLite handle serve owns; it reaches the store
// through serve's HTTP API, which is why its base URL defaults to serve's own
// loopback address.
type MCPConfig struct {
	// HarnessBaseURL is the harness's read-only HTTP API, used for GETs
	// this package makes on the caller's behalf (session list, events).
	// Inside compose this is the harness service's internal address; it is
	// not necessarily reachable from wherever a human's browser runs.
	HarnessBaseURL string
	// HarnessPublicURL is the base URL used to build transcript links
	// handed back to a caller so a human can watch a run. Defaults to
	// HarnessBaseURL, which is correct outside compose (both on loopback)
	// and wrong inside it (HarnessBaseURL is an internal service name a
	// browser cannot resolve) — set it explicitly there. The
	// http.external_url setting (internal/settings.KeyHTTPExternalURL),
	// when set in the settings table, overrides this at startup
	// (cmd/harness/serve.go).
	HarnessPublicURL string
	// PermissionCeiling clamps every launched run's permission mode,
	// regardless of what a caller requests: a request for "full" against a
	// ceiling of "default" runs as "default". This is a ceiling, not just
	// a default — a caller that never names a permission_mode still gets
	// clamped to it, not to whatever the harness's own default happens to
	// be.
	PermissionCeiling string
	// FlashModel is the model the "flash" profile launches
	// (docs/MODELS.md's Task subagent row: flash, high effort).
	FlashModel string
	// AcceptedWaitMS bounds how long the launch tool waits for the
	// `accepted` message before reporting the request as queued rather
	// than running.
	AcceptedWaitMS int
	// ControlToken is the run-control bearer token, handed to the MCP
	// service directly by `harness serve` at startup — the same value the
	// HTTP server itself holds (docs/RUN-CONTROL.md "Authentication").
	// Empty only when set programmatically from a process that skipped the
	// resolution; deepseek_stop then falls back to GET /api/control-token
	// on HarnessBaseURL as defensive code. An empty token from either
	// source is an error, never a request sent without a bearer.
	ControlToken string
}

// LoadMCP reads MCPConfig from the environment. Call config.LoadDotEnv
// first if .env should be consulted.
func LoadMCP() (MCPConfig, error) {
	ceiling := envOr("DEEPSEEK_MCP_PERMISSION_CEILING", defaultMCPPermissionCeiling)
	if !tools.Mode(ceiling).Valid() {
		return MCPConfig{}, fmt.Errorf("DEEPSEEK_MCP_PERMISSION_CEILING: must be readonly or full, got %q", ceiling)
	}

	acceptedWaitMS, err := envInt("DEEPSEEK_MCP_ACCEPTED_WAIT_MS", defaultMCPAcceptedWaitMS)
	if err != nil {
		return MCPConfig{}, err
	}

	base := envOr("DEEPSEEK_HARNESS_BASE_URL", defaultHarnessBaseURL)

	return MCPConfig{
		HarnessBaseURL:    base,
		HarnessPublicURL:  envOr("DEEPSEEK_HARNESS_PUBLIC_URL", base),
		PermissionCeiling: ceiling,
		FlashModel:        envOr("DEEPSEEK_MCP_FLASH_MODEL", defaultMCPFlashModel),
		AcceptedWaitMS:    acceptedWaitMS,
	}, nil
}
