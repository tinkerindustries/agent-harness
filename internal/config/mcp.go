package config

import (
	"fmt"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

const (
	// defaultMCPAddr binds loopback only, same reasoning as
	// defaultHTTPAddr: this port starts sessions that run Bash as root
	// inside the workspace mount, so it is sensitive even before anyone
	// reads a transcript through it.
	defaultMCPAddr = "127.0.0.1:8090"
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
	// defaultMCPCollectWaitCapMS bounds the collect tool's wait_ms, so a
	// caller cannot turn an MCP tool call into an unbounded hold on the
	// HTTP response.
	defaultMCPCollectWaitCapMS = 60_000
	// defaultHarnessBaseURL matches defaultHTTPAddr: the harness's own
	// read-only API, reachable on loopback when both processes run on one
	// host outside compose.
	defaultHarnessBaseURL = "http://127.0.0.1:8080"
)

// MCPConfig is harness mcp's runtime configuration (docs/DESIGN.md's MCP
// launch server). It is loaded separately from Config because the two
// processes are deployed separately — harness mcp holds a NATS connection
// and nothing else, never the SQLite handle serve owns — and share only
// NATS_URL.
type MCPConfig struct {
	// NATSURL is the JetStream server to publish work requests to and read
	// results from.
	NATSURL string
	// HarnessBaseURL is the harness's read-only HTTP API, used for GETs
	// this package makes on the caller's behalf (session list, events).
	// Inside compose this is the harness service's internal address; it is
	// not necessarily reachable from wherever a human's browser runs.
	HarnessBaseURL string
	// HarnessPublicURL is the base URL used to build transcript links
	// handed back to a caller so a human can watch a run. Defaults to
	// HarnessBaseURL, which is correct outside compose (both on loopback)
	// and wrong inside it (HarnessBaseURL is an internal service name a
	// browser cannot resolve) — set it explicitly there.
	HarnessPublicURL string
	// Addr is where harness mcp's streamable HTTP endpoint listens.
	// Loopback by default (see defaultMCPAddr).
	Addr string
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
	// CollectWaitCapMS caps the collect tool's wait_ms parameter.
	CollectWaitCapMS int
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
	collectWaitCapMS, err := envInt("DEEPSEEK_MCP_COLLECT_WAIT_CAP_MS", defaultMCPCollectWaitCapMS)
	if err != nil {
		return MCPConfig{}, err
	}

	base := envOr("DEEPSEEK_HARNESS_BASE_URL", defaultHarnessBaseURL)

	return MCPConfig{
		NATSURL:           envOr("NATS_URL", defaultNATSURL),
		HarnessBaseURL:    base,
		HarnessPublicURL:  envOr("DEEPSEEK_HARNESS_PUBLIC_URL", base),
		Addr:              envOr("DEEPSEEK_MCP_ADDR", defaultMCPAddr),
		PermissionCeiling: ceiling,
		FlashModel:        envOr("DEEPSEEK_MCP_FLASH_MODEL", defaultMCPFlashModel),
		AcceptedWaitMS:    acceptedWaitMS,
		CollectWaitCapMS:  collectWaitCapMS,
	}, nil
}
