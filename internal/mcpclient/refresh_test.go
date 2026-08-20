package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// pagedTestServer builds an in-process MCP server with n trivial tools and
// a page size of 1, so ListTools never returns them all in a single page —
// Refresh's pagination loop is only genuinely exercised if it follows
// NextCursor rather than trusting the first page.
func pagedTestServer(n int) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "paged-server", Version: "0.0.1"}, &mcpsdk.ServerOptions{PageSize: 1})
	for i := range n {
		name := "tool_" + string(rune('a'+i))
		server.AddTool(&mcpsdk.Tool{
			Name:        name,
			Description: "tool number " + string(rune('a'+i)),
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		})
	}
	return server
}

func dialInMemoryServer(t *testing.T, server *mcpsdk.Server) func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
	t.Helper()
	return func(ctx context.Context, _ store.MCPServer) (*mcpsdk.ClientSession, error) {
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { serverSession.Close() })

		client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
		return client.Connect(ctx, clientTransport, nil)
	}
}

func TestRefreshWritesQualifiedNamesFollowingPagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = dialInMemoryServer(t, pagedTestServer(3))

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("expected pagination to collect all 3 tools, got %d: %+v", len(got.Tools), got.Tools)
	}
	seen := map[string]bool{}
	for _, tool := range got.Tools {
		wantQualified := "mcp__srv__" + tool.Name
		if tool.QualifiedName != wantQualified {
			t.Fatalf("tool %q: QualifiedName = %q, want %q", tool.Name, tool.QualifiedName, wantQualified)
		}
		seen[tool.Name] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct tool names, got %v", seen)
	}
	if got.ProbeError != "" {
		t.Fatalf("expected no probe error, got %q", got.ProbeError)
	}
	if got.ProbedAt == "" {
		t.Fatalf("expected probed_at to be set")
	}

	// GetMCPServer must agree with what Refresh returned.
	reloaded, err := s.GetMCPServer(ctx, "srv")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if len(reloaded.Tools) != 3 {
		t.Fatalf("expected the store row to carry 3 tools, got %+v", reloaded.Tools)
	}
}

func TestRefreshFailureWritesErrorLeavingPreviousSnapshotIntact(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	// Seed a successful probe first, so there is a previous snapshot for a
	// failed Refresh to leave alone.
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "old_tool", QualifiedName: "mcp__srv__old_tool", Description: "d"},
	})

	m := New(s)
	wantErr := errors.New("dial: connection refused")
	m.Dial = func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
		return nil, wantErr
	}

	_, err := m.Refresh(ctx, "srv")
	if err == nil {
		t.Fatalf("expected Refresh to return an error")
	}

	got, getErr := s.GetMCPServer(ctx, "srv")
	if getErr != nil {
		t.Fatalf("GetMCPServer: %v", getErr)
	}
	if got.ProbeError == "" {
		t.Fatalf("expected probe_error to be recorded")
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "old_tool" {
		t.Fatalf("expected the previous snapshot to survive a failed refresh, got %+v", got.Tools)
	}
}

func TestRefreshReturnsReloadedRowAlongsideError(t *testing.T) {
	// Refresh's contract: even on failure, the returned row (not just the
	// error) reflects the new probe_error, so a caller can render both.
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
		return nil, errors.New("boom")
	}

	got, err := m.Refresh(ctx, "srv")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got.ProbeError == "" {
		t.Fatalf("expected the returned row to carry the new probe_error, got %+v", got)
	}
}

func TestRefreshTruncatesHugeErrors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	huge := ""
	for range 2000 {
		huge += "x"
	}
	m.Dial = func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
		return nil, errors.New(huge)
	}

	got, err := m.Refresh(ctx, "srv")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if len(got.ProbeError) >= len(huge) {
		t.Fatalf("expected probe_error to be truncated, got %d bytes", len(got.ProbeError))
	}
}

func TestRefreshUsesFreshDialNotTheCache(t *testing.T) {
	// Refresh must not reuse whatever the connection cache is holding: the
	// whole point is to prove the *current* configuration connects.
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	dials := 0
	m := New(s)
	m.Dial = func(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
		dials++
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		server := pagedTestServer(1)
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { serverSession.Close() })
		client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
		return client.Connect(ctx, clientTransport, nil)
	}

	if _, err := m.Refresh(ctx, "srv"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := m.Refresh(ctx, "srv"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if dials != 2 {
		t.Fatalf("expected 2 dials (one per Refresh), got %d", dials)
	}
}
