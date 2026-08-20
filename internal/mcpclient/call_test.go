package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// newTestMCPServer builds a small in-process MCP server exercising the
// content shapes Manager.Call and flatten must handle: plain text, an
// image, and a declared tool error.
func newTestMCPServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "0.0.1"}, nil)

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
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: args.Text}},
		}, nil
	})

	server.AddTool(&mcpsdk.Tool{
		Name:        "argcheck",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		text := "no arguments"
		if len(req.Params.Arguments) > 0 {
			text = "arguments: " + string(req.Params.Arguments)
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, nil
	})

	server.AddTool(&mcpsdk.Tool{
		Name:        "make_image",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{0xde, 0xad, 0xbe, 0xef}}},
		}, nil
	})

	server.AddTool(&mcpsdk.Tool{
		Name:        "fail",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "deliberately failed"}},
			IsError: true,
		}, nil
	})

	return server
}

// inMemoryDial connects a fresh in-process instance of newTestMCPServer for
// every dial, ignoring srv — used to inject a Manager.Dial that never
// touches the network, exercising Call end-to-end over a real (if
// in-memory) MCP session.
func inMemoryDial(t *testing.T) func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
	t.Helper()
	return func(ctx context.Context, _ store.MCPServer) (*mcpsdk.ClientSession, error) {
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		server := newTestMCPServer()
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { serverSession.Close() })

		client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
		return client.Connect(ctx, clientTransport, nil)
	}
}

func TestManagerCallFlattensText(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "echo", QualifiedName: "mcp__srv__echo", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryDial(t)
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__echo", json.RawMessage(`{"text":"hello there"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "hello there" {
		t.Fatalf("Text = %q, want %q", got.Text, "hello there")
	}
	if got.IsError {
		t.Fatalf("expected IsError false")
	}
}

func TestManagerCallEmptyArgumentsSendNoValues(t *testing.T) {
	// nil, "", and "null" must all be treated as "the caller supplied no
	// argument values" — decoded to a nil Go value rather than an error —
	// and not as malformed JSON. What crosses the wire from there is the
	// SDK's own concern: ClientSession.CallTool substitutes {} for a nil
	// Arguments itself ("avoid sending nil over the wire"), so a tool that
	// takes no parameters still receives a well-formed empty object rather
	// than an absent field.
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "argcheck", QualifiedName: "mcp__srv__argcheck", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryDial(t)
	t.Cleanup(func() { m.Close() })

	for _, args := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`null`)} {
		got, err := m.Call(ctx, "mcp__srv__argcheck", args)
		if err != nil {
			t.Fatalf("Call(%q): %v", args, err)
		}
		if got.Text != "arguments: {}" {
			t.Fatalf("Call(%q): Text = %q, want %q", args, got.Text, "arguments: {}")
		}
	}
}

func TestManagerCallImageResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "make_image", QualifiedName: "mcp__srv__make_image", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryDial(t)
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__make_image", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(got.Images) != 1 || got.Images[0].MIMEType != "image/png" {
		t.Fatalf("Images = %+v", got.Images)
	}
}

func TestManagerCallIsErrorResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "fail", QualifiedName: "mcp__srv__fail", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryDial(t)
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__fail", nil)
	if err != nil {
		t.Fatalf("Call: %v (a tool-level error must not be a Go error)", err)
	}
	if !got.IsError {
		t.Fatalf("expected IsError true, got %+v", got)
	}
	if got.Text != "deliberately failed" {
		t.Fatalf("Text = %q", got.Text)
	}
}

func TestManagerCallUnknownToolName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "echo", QualifiedName: "mcp__srv__echo", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryDial(t)

	_, err := m.Call(ctx, "mcp__srv__does_not_exist", nil)
	if err == nil {
		t.Fatalf("expected an error for an unknown tool name")
	}
	if !strings.Contains(err.Error(), "mcp__srv__does_not_exist") {
		t.Fatalf("error %q should name the unknown tool", err)
	}
}

func TestManagerCallToolOnDisabledServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "echo", QualifiedName: "mcp__srv__echo", Description: "d"},
	})
	if err := s.SetMCPServerEnabled(ctx, "srv", false); err != nil {
		t.Fatalf("disable server: %v", err)
	}

	m := New(s)
	m.Dial = inMemoryDial(t)

	_, err := m.Call(ctx, "mcp__srv__echo", nil)
	if err == nil {
		t.Fatalf("expected an error for a tool on a disabled server")
	}
}

// TestManagerCallOverStreamableHTTP is the one Call test that goes over a
// real network transport rather than the injectable Dial, so the
// streamable-HTTP path in dial.go (mcp.StreamableClientTransport plus the
// header round tripper) is genuinely exercised end to end, headers
// included.
func TestManagerCallOverStreamableHTTP(t *testing.T) {
	server := newTestMCPServer()
	var gotHeader string
	handler := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		gotHeader = r.Header.Get("X-Test-Auth")
		return server
	}, nil)
	// ts.Close() waits for every open connection to close, including the
	// client's standalone SSE stream — which only closes once Manager.Close
	// tears down the cached session. defer runs LIFO, so this order (m.Close
	// deferred *after* ts.Close) makes m.Close run first; t.Cleanup would
	// run too late, since Go's own defers inside the test body all run
	// before any Cleanup does.
	ts := httptest.NewServer(handler)
	defer ts.Close()

	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{
		Name:      "httpsrv",
		Transport: store.MCPTransportHTTP,
		URL:       ts.URL,
		Headers:   map[string]string{"X-Test-Auth": "topsecret"},
		Enabled:   true,
	})
	mustSaveProbe(t, s, "httpsrv", []store.MCPToolSnapshot{
		{Name: "echo", QualifiedName: "mcp__httpsrv__echo", Description: "d"},
	})

	m := New(s) // no Dial override: exercises defaultDial's http branch
	defer m.Close()

	got, err := m.Call(ctx, "mcp__httpsrv__echo", json.RawMessage(`{"text":"over http"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "over http" {
		t.Fatalf("Text = %q, want %q", got.Text, "over http")
	}
	if gotHeader != "topsecret" {
		t.Fatalf("expected the configured header to reach the server, got %q", gotHeader)
	}
}
