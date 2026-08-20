package session

import (
	"context"
	"log"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// resolveMCPDefinitions reads r.MCP's tool array and per-server read-only
// map, the way Run and Resume both need to (docs/MCP.md, "Resolution
// happens once per run"). Nil MCP (no client configured, the CLI without
// one wired, or every existing test) returns nothing, silently: MCP support
// is additive, and a session with none configured must behave exactly as
// it always has. A read error — the mcp_servers table could not be read —
// is logged and treated the same as nil rather than failing the run
// (docs/MCP.md: "A failure reading MCP definitions logs and proceeds with
// none").
func (r *Runner) resolveMCPDefinitions(ctx context.Context, sessionID string) ([]wire.Tool, map[string]bool) {
	if r.MCP == nil {
		return nil, nil
	}
	defs, readOnly, err := r.MCP.Definitions(ctx)
	if err != nil {
		log.Printf("session: resolve MCP definitions for %s: %v", sessionID, err)
		return nil, nil
	}
	return defs, readOnly
}
