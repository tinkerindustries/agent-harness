package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// statusService builds a Service whose status handler can be exercised
// without a NATS connection: handleStatus only reads the harness's read-only
// HTTP API, so JS is left nil on purpose — a nil-pointer panic here would
// itself be a bug (a status call should never touch the queue).
func statusService(t *testing.T, baseURL string) *Service {
	t.Helper()
	return &Service{
		Cfg: config.MCPConfig{
			PermissionCeiling: "full",
			FlashModel:        "test-flash",
			HarnessBaseURL:    baseURL,
			HarnessPublicURL:  "http://127.0.0.1:8080",
		},
		HTTPClient: &http.Client{},
		Registry:   NewRegistry(),
	}
}

func TestHandleStatusRejectsMissingRequestID(t *testing.T) {
	svc := statusService(t, "")
	res, _, err := svc.handleStatus(context.Background(), nil, statusInput{})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing request_id")
	}
}

func TestHandleStatusRendersFromEndpoint(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/requests/req-1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"request_id": "req-1",
			"session_id": "sess-1",
			"status": "running",
			"sub_turn": 4,
			"todos": [
				{"taskId": "1", "subject": "read the task", "description": "read it", "status": "completed", "activeForm": "Reading the task"},
				{"taskId": "2", "subject": "fix the bug", "description": "fix it", "status": "in_progress", "activeForm": "Fixing the bug"}
			],
			"active_form": "Fixing the bug",
			"tool_calls": [{"id": "call-9", "name": "Bash", "arguments": "{\"command\":\"go test ./...\"}"}],
			"usage": {"sub_turn": 4, "prompt_cache_hit_tokens": 10, "prompt_cache_miss_tokens": 200,
				"completion_tokens": 30, "reasoning_tokens": 4, "cost_usd": 0.0042},
			"started_at": "2026-01-01T00:00:00Z",
			"duration_ms": 90000,
			"transcript_url": "/sessions/sess-1"
		}`))
	}))
	defer fake.Close()

	svc := statusService(t, fake.URL)
	res, out, err := svc.handleStatus(context.Background(), nil, statusInput{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if out == nil {
		t.Fatal("expected the parsed status as structured content")
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	for _, want := range []string{
		"status: running",
		"sub_turn: 4",
		"working on: Fixing the bug",
		"[x] read the task",
		"[~] fix the bug",
		"tool in flight: Bash(",
		"cost so far: $0.004200 USD",
		"transcript: http://127.0.0.1:8080/sessions/sess-1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the rendered text to contain %q, got:\n%s", want, text)
		}
	}
}

func TestHandleStatusEndpointNotFound(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fake.Close()

	svc := statusService(t, fake.URL)
	res, _, err := svc.handleStatus(context.Background(), nil, statusInput{RequestID: "never-heard-of"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result when the harness answers 404")
	}
}
