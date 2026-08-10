// Package mcp is the harness's MCP launch server: it lets an external agent
// harness (Claude Code, Cursor) start and collect deepseek-harness runs by
// publishing work requests to the WORK stream and reading results back over
// the RESULTS stream and the harness's read-only HTTP API
// (docs/DESIGN.md §4.10). It holds a NATS connection and nothing else — no
// SQLite handle, since `harness serve` is the single writer.
//
// This is the opposite direction from the MCP integration docs/DESIGN.md §1
// lists as out of scope for v1. That entry is about the harness *consuming*
// MCP tools inside its own agent loop, which would put a variable,
// request-dependent tool array in front of the frozen cached prefix (§3.2).
// This package never touches the system prompt or the tool array DeepSeek
// sees; it runs as its own process (`harness mcp`) on its own port.
package mcp

import (
	"net/http"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// serverVersion is this package's own version, independent of the harness
// binary's — the MCP protocol surface (tool and resource names) can move at
// a different pace than the agent loop it launches.
const serverVersion = "0.6.0"

// Service holds everything the MCP tool and resource handlers need: the
// JetStream context to publish work requests and read results, an HTTP
// client for the harness's read-only API, the configuration that bounds a
// permission mode, and this process's own record of what it has launched.
type Service struct {
	JS         jetstream.JetStream
	Cfg        config.MCPConfig
	HTTPClient *http.Client
	Registry   *Registry
}

// NewServer builds the MCP server and registers every tool and resource
// this package exposes. Called once; the same *mcpsdk.Server instance is
// reused for every HTTP request (see Handler).
func (svc *Service) NewServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "deepseek-harness",
		Version: serverVersion,
	}, &mcpsdk.ServerOptions{
		Instructions: "Launch and collect deepseek-harness agent runs. deepseek_agent starts a run and returns " +
			"immediately; it never blocks for the run to finish. While a run is in flight, deepseek_status " +
			"reports where it is up to; deepseek_result returns the final outcome once the run is done. " +
			"Neither deepseek_status nor deepseek_result blocks. Each run works in a fresh directory holding " +
			"the repositories it was launched with.",
	})

	svc.registerLaunchTool(server)
	svc.registerStatusTool(server)
	svc.registerCollectTool(server)
	svc.registerRunsTool(server)
	svc.registerResources(server)

	return server
}

// Handler returns the streamable-HTTP handler for this service's MCP
// server. Stateless mode is used because every tool call here is a
// self-contained request/response — the launch/collect split (rather than a
// long-held connection) is what carries a multi-minute run, not an MCP
// session — so there is nothing worth keeping session state for.
func (svc *Service) Handler() http.Handler {
	server := svc.NewServer()
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true})
}
