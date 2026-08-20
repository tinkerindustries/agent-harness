// Package mcpclient is the client side of MCP support (docs/MCP.md): the
// consumer of internal/store's mcp_servers rows, and the implementation of
// the internal/tools.MCPProvider seam that internal/tools declares and the
// executor (internal/session, Phase 3) calls at run start and at
// tool-dispatch time.
//
// Definitions is a pure function of the stored snapshot: no server is
// dialled to build the tool array a session freezes onto its row
// (docs/MCP.md, "The tool array is built from a stored snapshot, never from
// a live connection"). Call, and the probe Refresh runs, are the only paths
// that open a connection — one cached *mcp.ClientSession per server, closed
// and redialled when the server's connection configuration changes
// underneath it or a liveness ping fails.
//
// Split by concern: this file holds the Manager type, the connection
// cache, Definitions, Call, and Close; naming.go is QualifyToolName and
// QualifyToolNames (docs/MCP.md, "Naming"); dial.go is the real dialer —
// stdio via mcp.CommandTransport, http via mcp.StreamableClientTransport —
// and the child-stderr-to-log wiring; content.go flattens a
// *mcp.CallToolResult into tools.MCPContent (docs/MCP.md, "Calling");
// refresh.go is Refresh, the probe docs/MCP.md's "Probing" section
// describes.
//
// Depends on: internal/store (the mcp_servers rows), internal/tools (the
// MCPProvider seam and its MCPContent/MCPImage/MCPServerOf vocabulary),
// internal/wire (the tool array shape the request head carries), and the
// MCP Go SDK (github.com/modelcontextprotocol/go-sdk/mcp).
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// pingTimeout bounds the liveness check the connection cache runs on a
// cached session before handing it back to a caller, so a server that has
// quietly died doesn't turn one Call into a wait for the full context
// deadline before the redial even starts.
const pingTimeout = 5 * time.Second

// Manager is the Manager the internal/tools.MCPProvider seam is implemented
// against (docs/MCP.md). One Manager is shared by every session in the
// process: the configuration it reads is global (docs/MCP.md, "Configuration
// is global and lives in the database"), and the connection cache is what
// lets two sessions calling the same server's tools in close succession
// reuse one live process or HTTP session instead of paying a fresh dial
// each time.
type Manager struct {
	// Store is where every enabled server's configuration and last-probed
	// tool snapshot come from.
	Store *store.Store

	// Dial overrides how a session is opened, for tests. Nil uses the real
	// transports (dial.go's defaultDial): stdio via mcp.CommandTransport,
	// http via mcp.StreamableClientTransport.
	Dial func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error)

	mu    sync.Mutex
	conns map[string]*cachedConn
}

// cachedConn is one live session the connection cache is holding, plus the
// fingerprint of the configuration it was dialled against — the value
// connFingerprint compares a fresh read of the row to, to decide whether the
// cached session still matches what the operator has configured.
type cachedConn struct {
	session     *mcpsdk.ClientSession
	fingerprint string
}

// New builds a Manager reading through st, with the real dialer.
func New(st *store.Store) *Manager {
	return &Manager{
		Store: st,
		conns: make(map[string]*cachedConn),
	}
}

// emptyObjectSchema is what an empty or unparseable tool input schema
// becomes in the array offered to the model: a tool definition the provider
// will reject outright is worse than one that simply takes no arguments.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Definitions implements tools.MCPProvider. It never dials: it reads
// ListEnabledMCPServers and, for each server in name order, appends the
// tools from that server's stored snapshot in the order the snapshot holds
// them — both orderings pinned by internal/store so the array is
// deterministic across calls (docs/MCP.md, "Naming", "Ordering is
// deterministic"). It is safe to call on every run start, including a run
// with no MCP servers configured at all, in which case it returns a nil
// slice and an empty map.
func (m *Manager) Definitions(ctx context.Context) ([]wire.Tool, map[string]bool, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return nil, nil, err
	}
	readOnly := make(map[string]bool, len(servers))
	var out []wire.Tool
	for _, srv := range servers {
		// Recorded for every enabled server, even one contributing no
		// tools (an unprobed server, or one whose last probe returned an
		// empty list) — the executor's permission gate needs to know
		// whether a server *would* allow a readonly call even before any
		// tool of its has been dispatched.
		readOnly[srv.Name] = srv.AllowReadOnly
		for _, t := range srv.Tools {
			out = append(out, wire.Tool{
				Type: "function",
				Function: wire.ToolFunction{
					Name:        t.QualifiedName,
					Description: toolDescription(srv.Name, t),
					Parameters:  toolParameters(t.InputSchema),
				},
			})
		}
	}
	return out, readOnly, nil
}

// toolDescription returns t's own description, or — for a tool a server
// reported with no description at all — a synthesised one naming its
// server, since an empty description tells the model nothing about what a
// tool that reached it under an unfamiliar name even does.
func toolDescription(server string, t store.MCPToolSnapshot) string {
	if t.Description != "" {
		return t.Description
	}
	return fmt.Sprintf("A tool provided by the %q MCP server.", server)
}

// toolParameters returns schema unchanged when it is a well-formed JSON
// value, and emptyObjectSchema when schema is empty, literally JSON null
// (a nil json.RawMessage round-trips through the store as "null", never as
// zero bytes, so both spellings of "nothing here" need the same fallback),
// or fails to parse.
func toolParameters(schema json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || string(trimmed) == "null" || !json.Valid(trimmed) {
		return emptyObjectSchema
	}
	return schema
}

// Call implements tools.MCPProvider. toolName is an already-qualified
// "mcp__<server>__<tool>" name, as Definitions offered it; Call resolves it
// back to (server, the server's own tool name) by scanning every enabled
// server's stored snapshot for a tool whose QualifiedName matches exactly —
// not by parsing the name apart, since QualifyToolName's collision and
// truncation handling is not reversible. A name with no such match — an
// unknown tool, or one whose server has since been disabled or had that
// tool renamed out from under it — is an error naming toolName.
func (m *Manager) Call(ctx context.Context, toolName string, args json.RawMessage) (tools.MCPContent, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return tools.MCPContent{}, err
	}

	var srv store.MCPServer
	var serverToolName string
	found := false
	for _, s := range servers {
		for _, t := range s.Tools {
			if t.QualifiedName == toolName {
				srv = s
				serverToolName = t.Name
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: unknown or disabled MCP tool %q", toolName)
	}

	var arguments any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return tools.MCPContent{}, fmt.Errorf("mcpclient: decode arguments for %q: %w", toolName, err)
		}
	}

	sess, err := m.session(ctx, srv)
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: connect to %q: %w", srv.Name, err)
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: serverToolName, Arguments: arguments})
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: call %q on %q: %w", serverToolName, srv.Name, err)
	}
	return flatten(res), nil
}

// session returns a live, working session for srv: the cached one, if its
// fingerprint still matches srv's current configuration and a quick ping
// succeeds, otherwise a fresh dial that replaces whatever was cached. It
// never holds m.mu across a network call — Ping and the dial itself both
// run with the lock released, so one slow server cannot block Definitions,
// Call, or Close against every other server sharing this Manager.
func (m *Manager) session(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
	fp := connFingerprint(srv)

	m.mu.Lock()
	cached := m.conns[srv.Name]
	m.mu.Unlock()

	if cached != nil && cached.fingerprint == fp {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := cached.session.Ping(pingCtx, nil)
		cancel()
		if err == nil {
			return cached.session, nil
		}
		m.dropCached(srv.Name, cached)
	} else if cached != nil {
		// The row's connection configuration has moved on since this
		// session was dialled — an edited server takes effect on its next
		// call rather than needing a restart.
		m.dropCached(srv.Name, cached)
	}

	sess, err := m.dial(ctx, srv)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.conns[srv.Name] = &cachedConn{session: sess, fingerprint: fp}
	m.mu.Unlock()
	return sess, nil
}

// dropCached removes name's cache entry if it is still exactly cur — a
// concurrent call may already have replaced or removed it — and, if it was
// removed here, closes the session it held.
func (m *Manager) dropCached(name string, cur *cachedConn) {
	m.mu.Lock()
	same := m.conns[name] == cur
	if same {
		delete(m.conns, name)
	}
	m.mu.Unlock()
	if same {
		_ = cur.session.Close()
	}
}

// dial opens a fresh session for srv, applying dialTimeout when ctx carries
// no deadline of its own.
func (m *Manager) dial(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	dial := m.Dial
	if dial == nil {
		dial = defaultDial
	}
	return dial(ctx, srv)
}

// Close closes every session the connection cache is holding, for
// shutdown. It does not touch the store.
func (m *Manager) Close() error {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[string]*cachedConn)
	m.mu.Unlock()

	var errs []error
	for _, c := range conns {
		if err := c.session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// connFingerprint summarises the parts of srv's configuration a dial
// depends on — transport, command, args, env, url, headers — as a
// deterministic string. session compares this against the fingerprint a
// cached connection was dialled with to decide whether an edit to the row
// invalidates that connection; encoding/json orders map keys, so two calls
// against equal configuration always produce the same string regardless of
// map iteration order.
func connFingerprint(srv store.MCPServer) string {
	fp := struct {
		Transport string
		Command   string
		Args      []string
		Env       map[string]string
		URL       string
		Headers   map[string]string
	}{
		Transport: srv.Transport,
		Command:   srv.Command,
		Args:      srv.Args,
		Env:       srv.Env,
		URL:       srv.URL,
		Headers:   srv.Headers,
	}
	b, err := json.Marshal(fp)
	if err != nil {
		// Args/Env/Headers are always plain strings (ValidateMCPServer),
		// so this cannot fail in practice; if it somehow did, returning a
		// value that never matches a previous fingerprint just forces a
		// redial, which is safe.
		return fmt.Sprintf("unmarshalable:%v", err)
	}
	return string(b)
}
