// Package mcp is the harness's MCP launch server: it lets an external agent
// harness (Claude Code, Cursor) start and collect deepseek-harness runs by
// publishing work requests to the WORK stream and reading results back over
// the RESULTS stream and the harness's HTTP API (docs/DESIGN.md §4.10).
// `harness serve` mounts it at /mcp on its own HTTP server, handing it
// serve's own JetStream handle and run-control token directly; it never
// opens a SQLite handle, since `harness serve` is the single writer.
//
// This is the opposite direction from the MCP integration docs/DESIGN.md §1
// lists as not built. That entry is about the harness *consuming*
// MCP tools inside its own agent loop, which would put a variable,
// request-dependent tool array in front of the frozen cached prefix (§3.2).
// This package never touches the system prompt or the tool array DeepSeek
// sees.
package mcp

import (
	"net/http"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// serverVersion is the only version string in the repo, and it is what an MCP
// client sees in serverInfo. It is owned by the release commit, which bumps it
// to match the tag (RELEASE.md, "What a release produces") — so feature work
// leaves it alone, however much it changes the MCP surface. Hand-bumping it
// here claims a release that does not exist and collides with the next one.
const serverVersion = "0.25.0"

// Service holds everything the MCP tool and resource handlers need: the
// JetStream context to publish work requests and read results, an HTTP
// client for the harness's read-only API, the configuration that bounds a
// permission mode, and this process's own record of what it has launched.
type Service struct {
	JS         jetstream.JetStream
	Cfg        config.MCPConfig
	HTTPClient *http.Client
	Registry   *Registry

	// Store is where deepseek_agent writes its attachments before
	// publishing (the request carries only the ids, docs/DATA-API.md). It is
	// serve's own handle, passed in rather than opened here — internal/mcp
	// never opens a SQLite handle (ARCHITECTURE.md). Nil refuses a launch
	// that carries attachments.
	Store *store.Store

	// Settings resolves the attachment caps (tools.attachments_max_count,
	// tools.attachments_max_bytes) on every launch, so a limit changed with
	// harness config set applies without a restart.
	Settings *settings.Resolver
}

// NewServer builds the MCP server and registers every tool and resource
// this package exposes. Called once; the same *mcpsdk.Server instance is
// reused for every HTTP request (see Handler).
func (svc *Service) NewServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "deepseek-harness",
		Version: serverVersion,
	}, &mcpsdk.ServerOptions{
		Instructions: "Launch, collect, steer, and stop deepseek-harness agent runs. deepseek_agent starts a run and returns " +
			"immediately; it never blocks for the run to finish. While a run is in flight, deepseek_status " +
			"reports where it is up to; deepseek_steer appends an instruction that reaches the model at the next " +
			"sub-turn boundary — the run does not stop to read it, so a long tool call delays it; deepseek_result " +
			"returns the final outcome once the run is done; deepseek_stop ends a run early and returns immediately " +
			"with whether the stop was accepted. None of deepseek_status, deepseek_result, deepseek_steer, or " +
			"deepseek_stop blocks. Each run works in a fresh directory holding " +
			"the repositories it was launched with.",
	})

	svc.registerLaunchTool(server)
	svc.registerStatusTool(server)
	svc.registerCollectTool(server)
	svc.registerRunsTool(server)
	svc.registerStopTool(server)
	svc.registerSteerTool(server)
	svc.registerResources(server)

	return server
}

// Handler returns the streamable-HTTP handler for this service's MCP
// server. The session is kept (Stateless: false) because initialize is the
// only place the client's own identity arrives: clientInfo is sent by the
// client library itself, not by the model, so it is what makes
// parent_agent_type producer-stamped rather than caller-asserted. The
// launch/collect split (rather than a long-held connection) is still what
// carries a multi-minute run — statefulness buys identity, not run
// continuity. The flip also lets GET and DELETE through (session
// termination) and permits server-to-client requests, which this package
// does not use yet.
func (svc *Service) Handler() http.Handler {
	server := svc.NewServer()
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{Stateless: false})
}
