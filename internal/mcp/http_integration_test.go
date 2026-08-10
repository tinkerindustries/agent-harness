package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startTestServer wires svc.Handler() into a real *httptest.Server at
// /mcp, the same path cmd/harness/mcp.go mounts it at, and returns a
// connected MCP client session against it — proving the tool and resource
// registrations actually work over the streamable HTTP transport the go-sdk
// provides, not just as direct Go calls to the handler methods.
func startTestServer(t *testing.T, svc *Service) *mcpsdk.ClientSession {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/mcp", svc.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcpsdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// TestMCPServerListsToolsAndResources is a wire-level smoke test: every
// tool and resource this package registers is actually visible to a real
// MCP client speaking streamable HTTP, under the exact names docs/DESIGN.md
// promises a caller.
func TestMCPServerListsToolsAndResources(t *testing.T) {
	_, js := connectOrSkip(t)
	svc := newIntegrationService(t, js)
	cs := startTestServer(t, svc)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	wantTools := map[string]bool{"deepseek_agent": false, "deepseek_status": false, "deepseek_result": false, "deepseek_runs": false, "deepseek_stop": false, "deepseek_steer": false}
	for _, tool := range tools.Tools {
		if _, ok := wantTools[tool.Name]; ok {
			wantTools[tool.Name] = true
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Fatalf("expected tool %q to be listed", name)
		}
	}

	resources, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	sawSessions := false
	for _, r := range resources.Resources {
		if r.URI == "harness://sessions" {
			sawSessions = true
		}
	}
	if !sawSessions {
		t.Fatalf("expected harness://sessions to be listed, got %+v", resources.Resources)
	}
}

// TestMCPServerCallToolLaunchOverHTTP drives deepseek_agent through the real
// transport end to end and checks the queued outcome comes back with the
// structured content a caller would parse.
func TestMCPServerCallToolLaunchOverHTTP(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)
	cs := startTestServer(t, svc)

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "deepseek_agent",
		Arguments: map[string]any{
			"description":     "http smoke test",
			"prompt":          "do nothing",
			"repos":           []any{map[string]any{"url": "https://example.com/org/app.git"}},
			"permission_mode": "readonly",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", contentText(res.Content))
	}
	if len(res.Content) == 0 {
		t.Fatal("expected non-empty content")
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok || !strings.Contains(text.Text, "queued") {
		t.Fatalf("expected queued status in the tool result text, got %+v", res.Content)
	}
}

// TestMCPServerReadSessionTranscriptResourceProxiesHarnessAPI proves the
// resource template calls through to the harness's own read-only API
// (docs/DESIGN.md §4.2) rather than reading anything itself, by pointing
// HarnessBaseURL at a fake server standing in for `harness serve`.
func TestMCPServerReadSessionTranscriptResourceProxiesHarnessAPI(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"events":[{"seq":1,"kind":"content_delta","payload":{"text":"hello from the fake harness"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/sessions/sess-fake"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"sess-fake","model":"deepseek-v4-pro","effort":"high","workspace":"/workspaces/demo","permission_mode":"default","status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	_, js := connectOrSkip(t)
	svc := newIntegrationService(t, js)
	svc.Cfg.HarnessBaseURL = fake.URL
	svc.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	cs := startTestServer(t, svc)

	res, err := cs.ReadResource(context.Background(), &mcpsdk.ReadResourceParams{URI: "harness://session/sess-fake/transcript"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("expected one content block, got %d", len(res.Contents))
	}
	body := res.Contents[0].Text
	if !strings.Contains(body, "hello from the fake harness") {
		t.Fatalf("expected the proxied event content in the transcript, got: %s", body)
	}
	if !strings.Contains(body, "deepseek-v4-pro") {
		t.Fatalf("expected the proxied session metadata in the transcript, got: %s", body)
	}
}

// contentText renders tool-result content for a failure message. %+v on the
// slice prints pointers, which says nothing about why a call failed.
func contentText(content []mcpsdk.Content) string {
	var b strings.Builder
	for _, c := range content {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
