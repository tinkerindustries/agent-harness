package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// MCPToolPrefix is the prefix every tool from an MCP server carries
// (docs/MCP.md, "Naming"): "mcp__" followed by the server name, "__", and
// the tool's own (sanitised) name. It is the convention Claude Code uses,
// chosen deliberately because it is a shape models have already seen.
const MCPToolPrefix = "mcp__"

// MCPImage is one image content block an MCP tool returned.
type MCPImage struct {
	MIMEType string
	Data     []byte
}

// MCPContent is one MCP tool call's raw return, before the executor turns
// it into a Result — flattening text, writing images into the workspace and
// applying the output cap all belong to the executor, which is the half
// that knows where the workspace is (docs/MCP.md, "Calling").
type MCPContent struct {
	Text    string
	Images  []MCPImage
	IsError bool
}

// MCPProvider is the narrow seam the executor reaches configured MCP
// servers through, declared here where it is consumed and implemented by
// internal/mcpclient (docs/MCP.md). It is the same shape as session.Client
// (internal/session/client.go) and internal/httpapi's RunController: a
// narrow interface declared at the consumer, implemented elsewhere, wired
// in cmd/harness.
type MCPProvider interface {
	// Definitions returns the tool array contributed by every enabled
	// server, built from each server's stored snapshot rather than a live
	// connection, plus the per-server read-only allowance the policy
	// freezes at run start. Runner.Run calls this once, when it resolves
	// the frozen array a session's requests all carry.
	Definitions(ctx context.Context) ([]wire.Tool, map[string]bool, error)

	// Call invokes toolName — an already-qualified "mcp__<server>__<tool>"
	// name, as Definitions offered it — with args, the tool call's raw JSON
	// arguments, and returns the server's raw reply for the executor to
	// flatten.
	Call(ctx context.Context, toolName string, args json.RawMessage) (MCPContent, error)
}

// MCPServerOf returns the server name inside an "mcp__<server>__<tool>"
// name, and whether name has that shape at all — false for anything that
// doesn't start with MCPToolPrefix or has nothing after it.
//
// It splits on the *first* "__" after the prefix, so a tool's own name that
// contains "__" (a sanitised name can: every character outside
// [A-Za-z0-9_-] becomes "_", and adjacent illegal characters collapse into
// what looks like a delimiter) never confuses which part is the server. The
// server name grammar (internal/store/mcp.go's mcpServerNameRE) does not
// itself forbid "__", so a server deliberately named with one would still
// split at its first occurrence rather than its own boundary — an
// unlikely operator choice this package does not try to guard against.
func MCPServerOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, MCPToolPrefix)
	if !ok {
		return "", false
	}
	server, tool, found := strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", false
	}
	return server, true
}
