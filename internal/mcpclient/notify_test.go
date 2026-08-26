package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// progressServer is a server whose one tool reports progress twice before
// answering, against whatever token the caller attached — the shape of any
// long-running MCP tool, a render or a background build.
func progressServer(t *testing.T) *mcpsdk.Server {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "progress-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "slow",
		Description: "takes its time",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		token := req.Params.GetProgressToken()
		for i := 1; i <= 2; i++ {
			_ = req.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      float64(i),
				Total:         4,
				Message:       "step " + string(rune('0'+i)),
			})
		}
		// A beat before answering. Notifications and the response travel
		// the same connection in order, but the SDK dispatches a
		// notification's handler asynchronously, so without this the reply
		// can reach the caller — and release the sink — while the second
		// notification is still queued behind it. That is a real outcome in
		// production too, where the straggler goes to the harness log
		// instead (onProgress); here it would just be a flaky test.
		time.Sleep(50 * time.Millisecond)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "done"}}}, nil
	})
	return server
}

// TestCallStreamsProgressToTheSink is the difference between a transcript
// that shows a long call working and one that looks like a hang: a
// server's progress notifications arrive on the same channel a running
// Bash command's output uses, while the call is still in flight.
func TestCallStreamsProgressToTheSink(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "slow", QualifiedName: "mcp__srv__slow", Description: "d"},
	})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, progressServer(t))
	t.Cleanup(func() { m.Close() })

	var mu sync.Mutex
	var chunks []string
	sink := func(chunk string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, chunk)
	}

	got, err := m.Call(tools.WithStdoutSink(ctx, sink), "mcp__srv__slow", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "done" {
		t.Fatalf("Text = %q, want the tool's answer", got.Text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 2 {
		t.Fatalf("expected two progress chunks, got %d: %v", len(chunks), chunks)
	}
	joined := strings.Join(chunks, "")
	for _, want := range []string{"[srv]", "25%", "50%", "step 1", "step 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress should mention %q, got: %q", want, joined)
		}
	}
}

// TestCallWithoutSinkIssuesNoProgressToken pins that a call nobody is
// watching does not ask its server for progress traffic — a Refresh probe
// or a CLI run with no hub should not make a server narrate itself into a
// log nothing reads.
func TestCallWithoutSinkIssuesNoProgressToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "reports_token", QualifiedName: "mcp__srv__reports_token", Description: "d"},
	})

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "token-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "reports_token",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		text := "none"
		if tok := req.Params.GetProgressToken(); tok != nil {
			text = "token"
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}, nil
	})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, server)
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__reports_token", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "none" {
		t.Fatalf("Text = %q, want no progress token issued", got.Text)
	}
}

// TestRootsAreOfferedToTheServer covers the other direction: a server that
// asks what directories this client is working in gets an answer, as
// file:// URIs, deduplicated and sorted.
//
// It asks the way the current protocol requires. Since 2026-07-28 a server
// may not send roots/list — or sampling, or elicitation — as a standalone
// request while serving one (SEP-2322); it embeds the ask in the result of
// the call it is already handling, and the SDK's client-side middleware
// fulfils it from the client's own state and re-invokes the handler. That
// is the shape every server-to-client interaction in this package now
// takes, which is why the roots this Manager hangs on the client are the
// whole implementation: there is no handler of ours in the path.
func TestRootsAreOfferedToTheServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "what_roots", QualifiedName: "mcp__srv__what_roots", Description: "d"},
	})

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "roots-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "what_roots",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcpsdk.CallToolResult{
				InputRequests: mcpsdk.InputRequestMap{"roots": &mcpsdk.ListRootsParams{}},
			}, nil
		}
		list := req.Params.InputResponses["roots"].(*mcpsdk.ListRootsResult)
		var uris []string
		for _, r := range list.Roots {
			uris = append(uris, r.URI)
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Join(uris, ",")}}}, nil
	})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, server)
	m.Roots = func(context.Context) []string { return []string{"/work/two", "/work/one", "/work/one"} }
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__what_roots", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "file:///work/one,file:///work/two" {
		t.Fatalf("roots = %q, want both workspaces once each, sorted", got.Text)
	}
}

// TestToolListChangedMarksTheRowStale pins the one write a notification
// triggers. Nothing re-probes: the flag is what tells the operator a
// Refresh is worth pressing, and the next successful probe clears it.
func TestToolListChangedMarksTheRowStale(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "echo", QualifiedName: "mcp__srv__echo", Description: "d"},
	})

	server := newTestMCPServer()
	m := New(s)
	m.Dial = inMemoryTransportTo(t, server)
	t.Cleanup(func() { m.Close() })

	// Connect first — a notification only reaches a client that is there
	// to hear it.
	if _, err := m.Call(ctx, "mcp__srv__echo", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	server.AddTool(&mcpsdk.Tool{
		Name:        "brand_new",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})

	var got store.MCPServer
	for range 100 {
		var err error
		if got, err = s.GetMCPServer(ctx, "srv"); err != nil {
			t.Fatal(err)
		}
		if got.Stale {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !got.Stale {
		t.Fatal("a tools/list_changed notification should mark the stored snapshot stale")
	}
	if len(got.Tools) != 1 {
		t.Fatalf("the snapshot itself must not move under a running session, got %+v", got.Tools)
	}

	// And a successful probe clears it.
	if _, err := m.Refresh(ctx, "srv"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	after, err := s.GetMCPServer(ctx, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if after.Stale {
		t.Fatal("a successful probe should clear the stale flag")
	}
}
