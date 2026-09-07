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
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// pingTimeout bounds the liveness check the connection cache runs on a
// cached session before handing it back to a caller, so a server that has
// quietly died doesn't turn one Call into a wait for the full context
// deadline before the redial even starts.
const pingTimeout = 5 * time.Second

// defaultSamplingModel is what a sampling turn runs on when the operator
// has named no other.
const defaultSamplingModel = "deepseek-v4-flash"

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

	// Dial overrides how a server's transport is built, for tests. Nil uses
	// the real ones (dial.go's defaultDial): stdio via mcp.CommandTransport,
	// http via mcp.StreamableClientTransport. It returns a transport rather
	// than a session because the client — and the roots and notification
	// handlers hung off it — belongs to this Manager, so a test that swaps
	// the transport still exercises the real client.
	Dial func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error)

	// Secrets, when set, is handed each server row on its way to a dial and
	// returns the row to dial with. It exists so a caller can keep a
	// credential out of the database: the row is stored without it, this
	// puts it back for the length of one dial, and a state directory left on
	// disk holds no bearer token. The row a caller cannot match is returned
	// unchanged. `harness gemini-session` is the caller — a parent's
	// `mcp_server` tool carries an Authorization header for a loopback
	// server it stood up, and that header has no business outliving the
	// process (docs/STDIO-PROTOCOL.md).
	Secrets func(store.MCPServer) store.MCPServer

	// Sampler runs the model turns servers ask for (docs/MCP.md,
	// "Sampling"). Nil — a caller with none wired, every test among them —
	// means this client cannot sample, and says so to any server that asks.
	// A server must also be allowed to ask at all: see
	// store.MCPServer.AllowSampling.
	Sampler SamplingClient

	// SamplingModel is the model those turns run on. Empty uses
	// defaultSamplingModel: a server's work is side work, and side work
	// runs on flash (docs/MODELS.md).
	SamplingModel string

	// Roots reports the filesystem roots this client is currently working
	// in, offered to every server over `roots/list` (docs/MCP.md, "Roots").
	// Nil declares the capability with an empty list, which is what an
	// operator running no sessions honestly has.
	Roots func(ctx context.Context) []string

	mu    sync.Mutex
	conns map[string]*cachedConn
	// sinks routes a server's progress notifications back to the call they
	// belong to, keyed by the progress token Call issued (notify.go). One
	// entry exists only while one call is in flight.
	sinks map[string]func(string)
}

// cachedConn is one live connection the cache is holding: the session calls
// ride on, the client that owns it, the fingerprint of the configuration it
// was dialled against — the value connFingerprint compares a fresh read of
// the row to, to decide whether the cached session still matches what the
// operator has configured — and the root set the client was last told
// about.
//
// The client is kept because roots live on it, not on the session. A
// workspace appearing or disappearing has to reach a connected server, and
// the protocol's answer is a roots/list_changed notification, not a
// redial: tearing down a `uvx`-launched subprocess every time a session
// starts would cost seconds of process startup to deliver one line of
// bookkeeping.
type cachedConn struct {
	client      *mcpsdk.Client
	session     *mcpsdk.ClientSession
	fingerprint string
	roots       []string
}

// New builds a Manager reading through st, with the real dialer and roots
// backed by the workspace leases st already holds.
//
// Leases are the honest answer to "which directories is this client working
// in": one row per live session's workspace, acquired when a run starts and
// released when it ends (internal/store/leases.go), which is exactly the
// set a server is entitled to know about. It also means roots follow the
// process rather than any one session — a server connected once and shared
// by every session in the process cannot be told a different set per
// caller, and claiming otherwise would be a lie told per tool call.
func New(st *store.Store) *Manager {
	m := &Manager{
		Store: st,
		conns: make(map[string]*cachedConn),
	}
	m.Roots = func(ctx context.Context) []string {
		leases, err := st.ListWorkspaceLeases(ctx)
		if err != nil {
			log.Printf("mcpclient: read workspace leases for roots: %v", err)
			return nil
		}
		out := make([]string, 0, len(leases))
		for _, l := range leases {
			out = append(out, l.Workspace)
		}
		return out
	}
	return m
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

// Instructions implements tools.MCPProvider. Like Definitions it never
// dials: it reads the instructions column of every enabled server's row,
// which the last successful probe wrote from that server's
// InitializeResult (docs/MCP.md, "Probing"). Servers that sent none are
// left out entirely, so the returned map is empty — never a map of empty
// strings — for a registry of servers that say nothing about themselves,
// and nil only when there are no enabled servers at all.
//
// It is a second read of the same table rather than a third return value
// from Definitions because the two are consumed by different halves of a
// run: Definitions feeds the frozen tool array a resumed session must
// reproduce byte for byte, while these feed the opening message, which is
// written once and then lives in the session's own history. Nothing reads
// them together, so nothing needs them read atomically.
func (m *Manager) Instructions(ctx context.Context) (map[string]string, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	for _, srv := range servers {
		if srv.Instructions == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(servers))
		}
		out[srv.Name] = srv.Instructions
	}
	return out, nil
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

	params := &mcpsdk.CallToolParams{Name: serverToolName, Arguments: arguments}
	// A progress token is only worth issuing when something is listening
	// for what comes back: with no sink attached, a server's progress has
	// nowhere to go but the harness log, and asking for it would be asking
	// for traffic nobody reads.
	if sink := callSink(ctx); sink != nil {
		token, release := m.registerSink(sink)
		defer release()
		params.SetProgressToken(token)
	}

	res, err := sess.CallTool(ctx, params)
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
			m.syncRoots(ctx, cached)
			return cached.session, nil
		}
		m.dropCached(srv.Name, cached)
	} else if cached != nil {
		// The row's connection configuration has moved on since this
		// session was dialled — an edited server takes effect on its next
		// call rather than needing a restart.
		m.dropCached(srv.Name, cached)
	}

	c, err := m.dial(ctx, srv)
	if err != nil {
		return nil, err
	}
	c.fingerprint = fp

	m.mu.Lock()
	m.conns[srv.Name] = c
	m.mu.Unlock()
	return c.session, nil
}

// syncRoots brings a cached connection's root set up to date with what
// Roots reports now, sending the server a roots/list_changed notification
// if anything moved. A server that never asked for roots ignores it; one
// that did re-reads the list and sees the workspace that appeared since it
// connected.
//
// Failures are silent by design. Roots are an offer, not a dependency: a
// server that cannot be told about a new workspace still answers every tool
// call it answered a moment ago, and turning that into a failed tool call
// would trade a real capability for a bookkeeping detail.
func (m *Manager) syncRoots(ctx context.Context, c *cachedConn) {
	want := m.roots(ctx)
	m.mu.Lock()
	same := equalStrings(c.roots, want)
	if !same {
		c.roots = want
	}
	m.mu.Unlock()
	if same {
		return
	}
	c.client.RemoveRoots() // clears every root, whatever it currently holds
	if len(want) > 0 {
		c.client.AddRoots(rootsOf(want)...)
	}
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

// dial opens a fresh session for srv under dialTimeout, or under the
// caller's own deadline when that is the shorter of the two. Applying it
// even to a context that already has a deadline is what keeps a stuck dial
// legible: the caller's bound expiring first produces nothing but "context
// deadline exceeded", whereas this one expiring says which server was being
// dialled and for how long it was waited on.
func (m *Manager) dial(ctx context.Context, srv store.MCPServer) (*cachedConn, error) {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > dialTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	dial := m.Dial
	if dial == nil {
		dial = defaultDial
	}
	if m.Secrets != nil {
		srv = m.Secrets(srv)
	}
	// The transport and the handshake share one error path: both are
	// "dialling this server did not work", and both fail the same way when
	// the deadline is what ran out.
	c, err := func() (*cachedConn, error) {
		transport, err := dial(ctx, srv)
		if err != nil {
			return nil, err
		}
		return m.connect(ctx, srv, transport)
	}()
	if err != nil {
		// A bare context error names nothing an operator can act on. Say
		// what was being waited for, because the usual causes — a server
		// that never finishes MCP initialisation, a package that will not
		// install, an application it depends on answering slowly — are all
		// invisible from the error alone.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("dialling MCP server %q timed out after %s: it accepted the connection but did not finish MCP initialisation", srv.Name, dialTimeout)
		}
		return nil, err
	}
	return c, nil
}

// connect builds the client every dial rides on — the roots it offers and
// the handlers that receive a server's progress, log, and list-changed
// notifications (notify.go) — and completes the handshake over transport.
func (m *Manager) connect(ctx context.Context, srv store.MCPServer, transport mcpsdk.Transport) (*cachedConn, error) {
	impl := &mcpsdk.Implementation{Name: "deepseek-harness", Version: clientVersion}
	client := mcpsdk.NewClient(impl, m.clientOptions(srv))
	roots := m.roots(ctx)
	if len(roots) > 0 {
		client.AddRoots(rootsOf(roots)...)
	}
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	m.setLogLevel(ctx, srv, sess)
	return &cachedConn{client: client, session: sess, roots: roots}, nil
}

// roots reports the current root set, normalised: never nil, sorted, so
// syncRoots compares like with like rather than redialling on a map
// iteration order.
func (m *Manager) roots(ctx context.Context) []string {
	if m.Roots == nil {
		return nil
	}
	got := m.Roots(ctx)
	out := make([]string, 0, len(got))
	seen := map[string]bool{}
	for _, r := range got {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// rootsOf renders paths as the file:// URIs the protocol requires.
func rootsOf(paths []string) []*mcpsdk.Root {
	out := make([]*mcpsdk.Root, len(paths))
	for i, p := range paths {
		out[i] = &mcpsdk.Root{URI: "file://" + p, Name: filepath.Base(p)}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
