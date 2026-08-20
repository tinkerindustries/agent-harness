package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// maxProbeErrorLen bounds how much of a probe failure SaveMCPProbe writes
// to probe_error, so a misbehaving server that dumps a stack trace or an
// HTML error page to its stderr or response body cannot bloat the row past
// what the /mcp screen has any use for.
const maxProbeErrorLen = 500

// Refresh forces a fresh dial of name — bypassing the connection cache
// entirely, since the point of Refresh is to prove the server's *current*
// configuration actually connects, not that some earlier connection is
// still alive — lists its tools to the end of pagination, qualifies their
// names, and records the outcome with SaveMCPProbe (docs/MCP.md,
// "Probing").
//
// On success it returns the freshly reloaded row. On failure it still
// returns the reloaded row, now carrying the new probe_error, alongside the
// error — so a caller (the /mcp screen, by way of internal/httpapi) can
// render both what went wrong and the row as it now stands, including
// whatever tool snapshot survived from the last successful probe
// (docs/MCP.md, "The tool array is built from a stored snapshot").
func (m *Manager) Refresh(ctx context.Context, name string) (store.MCPServer, error) {
	srv, err := m.Store.GetMCPServer(ctx, name)
	if err != nil {
		return store.MCPServer{}, err
	}

	snapshot, probeErr := m.probe(ctx, srv)
	if probeErr != nil {
		if err := m.Store.SaveMCPProbe(ctx, name, nil, truncateProbeError(probeErr), time.Now().UTC()); err != nil {
			return store.MCPServer{}, err
		}
		reloaded, err := m.Store.GetMCPServer(ctx, name)
		if err != nil {
			return store.MCPServer{}, err
		}
		return reloaded, probeErr
	}

	if err := m.Store.SaveMCPProbe(ctx, name, snapshot, "", time.Now().UTC()); err != nil {
		return store.MCPServer{}, err
	}
	return m.Store.GetMCPServer(ctx, name)
}

// probe dials srv fresh, lists its tools to the end of pagination, and
// returns them as store snapshots with QualifiedName already computed —
// the one place that computation happens, so every reader of tools_json
// afterwards treats it as data rather than re-deriving it (internal/store's
// MCPToolSnapshot.QualifiedName doc comment).
func (m *Manager) probe(ctx context.Context, srv store.MCPServer) ([]store.MCPToolSnapshot, error) {
	sess, err := m.dial(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	var raw []*mcpsdk.Tool
	cursor := ""
	for {
		res, err := sess.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		raw = append(raw, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	names := make([]string, len(raw))
	for i, t := range raw {
		names[i] = t.Name
	}
	qualified := QualifyToolNames(srv.Name, names)

	snapshot := make([]store.MCPToolSnapshot, len(raw))
	for i, t := range raw {
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcpclient: encode input schema for tool %q: %w", t.Name, err)
		}
		snapshot[i] = store.MCPToolSnapshot{
			Name:          t.Name,
			QualifiedName: qualified[i],
			Description:   t.Description,
			InputSchema:   schema,
		}
	}
	return snapshot, nil
}

// truncateProbeError renders err's message, cut to maxProbeErrorLen bytes
// on a rune boundary so the stored string stays valid UTF-8.
func truncateProbeError(err error) string {
	s := err.Error()
	if len(s) <= maxProbeErrorLen {
		return s
	}
	b := []byte(s)[:maxProbeErrorLen]
	for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b) + "… (truncated)"
}
