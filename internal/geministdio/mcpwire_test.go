package geministdio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// echoServer builds a tiny in-process MCP server with one "echo" tool, so a
// test can dial it in memory without a real subprocess or a real HTTP
// listener — the same technique internal/mcpclient's own tests use for the
// same reason (no mocking framework; a real *mcp.Client against a real, if
// in-memory, server session).
func echoServer(name string) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: args.Text}}}, nil
	})
	return server
}

// dialInMemory connects a fresh in-process instance of server for every
// dial, ignoring the store row's own transport fields — used to inject an
// MCP manager's Dial with something that never touches the network or a
// real subprocess. It returns the dial function and a closer for the
// server-side sessions it opened, which the caller must defer *before* the
// fixture's own mgr.Close cleanup runs: a real client holds a channel open
// for unsolicited notifications, so closing the server session first blocks
// waiting for a client that has not gone away yet. An explicit defer in the
// test body runs ahead of every t.Cleanup, including the fixture's, which is
// what makes the ordering safe here without reaching into fixture internals.
func dialInMemory(server *mcpsdk.Server) (dial func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error), closeAll func()) {
	var mu sync.Mutex
	var sessions []*mcpsdk.ServerSession
	dial = func(ctx context.Context, _ store.MCPServer) (mcpsdk.Transport, error) {
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		sessions = append(sessions, serverSession)
		mu.Unlock()
		return clientTransport, nil
	}
	closeAll = func() {
		mu.Lock()
		defer mu.Unlock()
		for _, ss := range sessions {
			ss.Close()
		}
	}
	return dial, closeAll
}

// TestStdioMCPEnvNeverReachesTheStore is TestMCPHeadersNeverReachTheStore's
// counterpart for the stdio addition (design §4.3, §6.3): a value the client
// supplies fresh in a stdio mcp_server declaration's env map is a connection
// secret exactly as a bearer header is, and must not be the
// mcp_servers.env column internal/store persists for harness serve's own
// operator-configured servers.
func TestStdioMCPEnvNeverReachesTheStore(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	const secret = "super-secret-value"
	srv := Tool{
		Type: ToolMCPServer, Name: "filesystem",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"},
		Env: map[string]string{"NODE_ENV": secret},
	}
	if err := f.srv.registerServers(context.Background(), []Tool{srv}, map[string]bool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	row, err := f.srv.opts.Store.GetMCPServer(context.Background(), "filesystem")
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if row.Transport != store.MCPTransportStdio {
		t.Errorf("transport = %q, want stdio", row.Transport)
	}
	if row.Command != "npx" {
		t.Errorf("command = %q, want npx", row.Command)
	}
	if len(row.Env) != 0 {
		t.Errorf("the stored row carries env: %v", row.Env)
	}

	db, err := os.ReadFile(filepath.Join(f.stateDir, "session.db"))
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(db, []byte(secret)) {
		t.Error("the env value is on disk in the state directory")
	}

	dialled := f.srv.dialSecrets(row)
	if dialled.Env["NODE_ENV"] != secret {
		t.Errorf("the dial did not get the env back: %v", dialled.Env)
	}
}

// TestMCPServerDeclarationMutualExclusivity pins that a declaration must
// name exactly one transport's fields: url/headers, xor command/args/env.
func TestMCPServerDeclarationMutualExclusivity(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	for _, tc := range []struct {
		name string
		tool Tool
	}{
		{"both url and command", Tool{Type: ToolMCPServer, Name: "srv", URL: "http://127.0.0.1:1/x", Command: "npx"}},
		{"neither", Tool{Type: ToolMCPServer, Name: "srv"}},
		{"headers with no url", Tool{Type: ToolMCPServer, Name: "srv", Headers: map[string]string{"Authorization": "Bearer x"}}},
		{"args with no command", Tool{Type: ToolMCPServer, Name: "srv", Args: []string{"-y"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.srv.registerServers(context.Background(), []Tool{tc.tool}, map[string]bool{})
			if err == nil {
				t.Fatal("expected the declaration to be refused")
			}
		})
	}
}

// TestRawToolCallThroughHTTPAndStdioMCPServers registers one server declared
// with url/headers and one declared with command/args/env, and calls each
// server's own tool by its qualified name — proving both wire shapes reach
// the same probe-and-call path (design §6.3: "the gap is only the wire
// layer"; internal/mcpclient's dialer already speaks both).
func TestRawToolCallThroughHTTPAndStdioMCPServers(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	httpDial, closeHTTP := dialInMemory(echoServer("http-echo"))
	stdioDial, closeStdio := dialInMemory(echoServer("stdio-echo"))
	// All three defers run before the fixture's own t.Cleanup(mgr.Close),
	// and defer is LIFO: the manager's own Close must run first (client
	// sessions), the two server-side closes after, or the server-side
	// Close blocks waiting for a client that has not disconnected yet
	// (dialInMemory's own comment). Deferring mgr.Close() last is what
	// makes it run first; calling it again from the fixture's later
	// t.Cleanup is a no-op over an already-empty connection cache.
	defer closeHTTP()
	defer closeStdio()
	defer f.srv.opts.MCP.Close()
	f.srv.opts.MCP.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
		switch srv.Transport {
		case store.MCPTransportHTTP:
			return httpDial(ctx, srv)
		case store.MCPTransportStdio:
			return stdioDial(ctx, srv)
		default:
			return nil, fmt.Errorf("unexpected transport %q", srv.Transport)
		}
	}

	decls := []Tool{
		{Type: ToolMCPServer, Name: "httpsrv", URL: "http://127.0.0.1:1/unused", Headers: map[string]string{"Authorization": "Bearer unused"}},
		{Type: ToolMCPServer, Name: "stdiosrv", Command: "unused", Args: []string{"--unused"}, Env: map[string]string{"X": "y"}},
	}
	names := map[string]bool{}
	if err := f.srv.registerServers(context.Background(), decls, names); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	defs, _, err := f.srv.opts.MCP.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	var gotHTTP, gotStdio bool
	for _, d := range defs {
		switch d.Function.Name {
		case "mcp__httpsrv__echo":
			gotHTTP = true
		case "mcp__stdiosrv__echo":
			gotStdio = true
		}
	}
	if !gotHTTP {
		t.Errorf("mcp__httpsrv__echo missing from definitions: %+v", defs)
	}
	if !gotStdio {
		t.Errorf("mcp__stdiosrv__echo missing from definitions: %+v", defs)
	}

	httpRes, err := f.srv.opts.MCP.Call(ctx, "mcp__httpsrv__echo", json.RawMessage(`{"text":"hi-http"}`))
	if err != nil {
		t.Fatalf("call http server's tool: %v", err)
	}
	if !strings.Contains(httpRes.Text, "hi-http") {
		t.Errorf("http tool result = %q, want hi-http", httpRes.Text)
	}

	stdioRes, err := f.srv.opts.MCP.Call(ctx, "mcp__stdiosrv__echo", json.RawMessage(`{"text":"hi-stdio"}`))
	if err != nil {
		t.Fatalf("call stdio server's tool: %v", err)
	}
	if !strings.Contains(stdioRes.Text, "hi-stdio") {
		t.Errorf("stdio tool result = %q, want hi-stdio", stdioRes.Text)
	}
}
