package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
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

func dialInMemoryServer(t *testing.T, server *mcpsdk.Server) func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
	t.Helper()
	return inMemoryTransportTo(t, server)
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
	m.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
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
	m.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
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
	m.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
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
	m.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
		dials++
		return inMemoryTransportTo(t, pagedTestServer(1))(ctx, srv)
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

// TestRefreshRecordsFailureOnAnExpiredContext pins the reason a probe
// failure is written on a fresh context rather than the caller's. A
// deadline is the most common way a probe fails, and the caller's context
// is expired by definition once it has: writing the reason through that
// same context fails silently, and the row is left saying nothing about a
// probe that plainly did not work. A create whose probe timed out answered
// with an empty probe_error exactly this way.
func TestRefreshRecordsFailureOnAnExpiredContext(t *testing.T) {
	s := openTestStore(t)
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = func(ctx context.Context, srv store.MCPServer) (mcpsdk.Transport, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	// A context that expires while the dial is in flight, which is what the
	// probe budget expiring looks like from in here.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	row, err := m.Refresh(ctx, "srv")
	if err == nil {
		t.Fatal("Refresh with an expiring context returned no error")
	}
	if row.Name != "srv" {
		t.Fatalf("row name = %q, want the row reloaded alongside the error", row.Name)
	}
	if row.ProbeError == "" {
		t.Fatal("probe_error is empty: the failure was not recorded because the write used the expired context")
	}

	// And it is durable, not just present on the returned value.
	stored, err := s.GetMCPServer(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if stored.ProbeError == "" {
		t.Fatal("stored probe_error is empty: the row kept no account of why the probe failed")
	}
}

// instructingTestServer builds an in-process MCP server that sends
// instructions at initialize, the way the official Blender server does —
// one trivial tool, so the server has something to be instructive about.
func instructingTestServer(instructions string) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "instructing-server", Version: "0.0.1"},
		&mcpsdk.ServerOptions{Instructions: instructions})
	server.AddTool(&mcpsdk.Tool{
		Name:        "do_thing",
		Description: "does the thing",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
	})
	return server
}

// TestRefreshStoresServerInstructions is the reason the instructions column
// exists: a server's own prose about how to use it arrives in the
// initialize handshake, not in any tool schema, and before this it was read
// by the SDK and then dropped on the floor.
func TestRefreshStoresServerInstructions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	const text = "NEVER assume missing values - inspect the scene first."
	m := New(s)
	m.Dial = dialInMemoryServer(t, instructingTestServer(text))

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.Instructions != text {
		t.Fatalf("Instructions = %q, want %q", got.Instructions, text)
	}

	// And they must be on the row a later reader sees, not only on the
	// value Refresh happened to return.
	reloaded, err := s.GetMCPServer(ctx, "srv")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if reloaded.Instructions != text {
		t.Fatalf("reloaded Instructions = %q, want %q", reloaded.Instructions, text)
	}
}

// TestRefreshLeavesInstructionsEmptyWhenServerSendsNone pins that a silent
// server stores "" rather than anything synthesised on its behalf — the
// opening message renders no section for it at all.
func TestRefreshLeavesInstructionsEmptyWhenServerSendsNone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = dialInMemoryServer(t, pagedTestServer(2))

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.Instructions != "" {
		t.Fatalf("Instructions = %q, want empty", got.Instructions)
	}
}

// TestRefreshFailureKeepsInstructions extends the invariant the whole table
// is built around to the new column: a probe that cannot connect must not
// strip a session of prose the last working probe read, for exactly the
// reason it must not strip it of tools.
func TestRefreshFailureKeepsInstructions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	const text = "Respect existing structure and naming conventions."
	m := New(s)
	m.Dial = dialInMemoryServer(t, instructingTestServer(text))
	if _, err := m.Refresh(ctx, "srv"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	m.Dial = func(context.Context, store.MCPServer) (mcpsdk.Transport, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	got, err := m.Refresh(ctx, "srv")
	if err == nil {
		t.Fatal("second Refresh: expected the dial failure")
	}
	if got.Instructions != text {
		t.Fatalf("Instructions = %q, want the surviving %q", got.Instructions, text)
	}
	if got.ProbeError == "" {
		t.Fatal("probe_error is empty, want the dial failure recorded")
	}
}

// TestRefreshTruncatesOversizedInstructions bounds what a server can put in
// front of every session: the text is capped, and says that it was.
func TestRefreshTruncatesOversizedInstructions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = dialInMemoryServer(t, instructingTestServer(strings.Repeat("x", maxInstructionsLen+500)))

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(got.Instructions) <= maxInstructionsLen {
		t.Fatalf("len(Instructions) = %d, want the cap plus the marker", len(got.Instructions))
	}
	if !strings.HasSuffix(got.Instructions, "… (truncated)") {
		t.Fatalf("Instructions = %q…, want the truncation marker", got.Instructions[:60])
	}
}

// TestInstructionsSkipsServersWithNone pins the shape Instructions hands
// the renderer: only servers that actually said something, so the opening
// message never grows a heading with nothing under it.
func TestInstructionsSkipsServersWithNone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "chatty", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustCreateServer(t, s, store.MCPServer{Name: "quiet", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustCreateServer(t, s, store.MCPServer{Name: "off", Transport: store.MCPTransportStdio, Command: "unused", Enabled: false})

	if err := s.SaveMCPProbe(ctx, "chatty", store.MCPProbe{Instructions: "inspect first"}, "", time.Now().UTC()); err != nil {
		t.Fatalf("save chatty: %v", err)
	}
	if err := s.SaveMCPProbe(ctx, "off", store.MCPProbe{Instructions: "never read"}, "", time.Now().UTC()); err != nil {
		t.Fatalf("save off: %v", err)
	}

	got, err := New(s).Instructions(ctx)
	if err != nil {
		t.Fatalf("Instructions: %v", err)
	}
	want := map[string]string{"chatty": "inspect first"}
	if len(got) != len(want) || got["chatty"] != want["chatty"] {
		t.Fatalf("Instructions = %v, want %v", got, want)
	}
}
