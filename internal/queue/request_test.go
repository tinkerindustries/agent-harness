package queue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRequestRoundTrips(t *testing.T) {
	body := `{
		"request_id": "req-1",
		"prompt": "do the thing",
		"workspace": "/tmp/ws",
		"model": "deepseek-v4-flash",
		"effort": "low",
		"permission_mode": "default",
		"deny": ["git push"],
		"result_schema": {"type": "object"},
		"max_sub_turns": 10,
		"deadline_ms": 60000
	}`
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.RequestID != "req-1" || req.Prompt != "do the thing" || req.Workspace != "/tmp/ws" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if req.Model != "deepseek-v4-flash" || req.Effort != "low" || req.PermissionMode != "default" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if len(req.Deny) != 1 || req.Deny[0] != "git push" {
		t.Fatalf("unexpected deny: %+v", req.Deny)
	}
	if req.MaxSubTurns != 10 || req.DeadlineMS != 60000 {
		t.Fatalf("unexpected limits: %+v", req)
	}
}

func TestParseRequestRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseRequest([]byte(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func testRoots(t *testing.T) (roots []string, workspace string) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "project")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return []string{resolvedRoot}, filepath.Join(root, "project")
}

func TestValidateAcceptsWorkspaceUnderRoot(t *testing.T) {
	roots, ws := testRoots(t)
	req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws}
	resolved, err := req.Validate(roots)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if resolved == "" {
		t.Fatal("expected a resolved workspace path")
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	roots, ws := testRoots(t)
	cases := []Request{
		{Prompt: "go", Workspace: ws},
		{RequestID: "req-1", Workspace: ws},
		{RequestID: "req-1", Prompt: "go"},
	}
	for _, req := range cases {
		if _, err := req.Validate(roots); err == nil {
			t.Fatalf("expected validation to fail for %+v", req)
		}
	}
}

func TestValidateRejectsRequestIDWithSubjectMetacharacters(t *testing.T) {
	roots, ws := testRoots(t)
	req := Request{RequestID: "req.1", Prompt: "go", Workspace: ws}
	if _, err := req.Validate(roots); err == nil {
		t.Fatal("expected a request_id containing '.' to be rejected")
	}
}

func TestValidateRejectsWorkspaceOutsideRoots(t *testing.T) {
	roots, _ := testRoots(t)
	other := t.TempDir()
	req := Request{RequestID: "req-1", Prompt: "go", Workspace: other}
	if _, err := req.Validate(roots); err == nil {
		t.Fatal("expected a workspace outside every configured root to be rejected")
	}
}

func TestValidateRejectsWorkspaceWhenNoRootsConfigured(t *testing.T) {
	_, ws := testRoots(t)
	req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws}
	if _, err := req.Validate(nil); err == nil {
		t.Fatal("expected validation to fail closed with no configured roots")
	}
}

func TestValidateRejectsInvalidPermissionMode(t *testing.T) {
	roots, ws := testRoots(t)
	req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws, PermissionMode: "sudo"}
	if _, err := req.Validate(roots); err == nil {
		t.Fatal("expected an invalid permission_mode to be rejected")
	}
}

func TestValidateAcceptsEveryPermissionMode(t *testing.T) {
	roots, ws := testRoots(t)
	for _, mode := range []string{"", "readonly", "default", "full"} {
		req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws, PermissionMode: mode}
		if _, err := req.Validate(roots); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
}

func TestValidateRejectsMalformedResultSchema(t *testing.T) {
	roots, ws := testRoots(t)
	cases := []json.RawMessage{
		json.RawMessage(`not json`),
		json.RawMessage(`["not", "an", "object"]`),
		json.RawMessage(`"a string"`),
	}
	for _, schema := range cases {
		req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws, ResultSchema: schema}
		if _, err := req.Validate(roots); err == nil {
			t.Fatalf("expected result_schema %s to be rejected", schema)
		}
	}
}

func TestValidateAcceptsWellFormedResultSchema(t *testing.T) {
	roots, ws := testRoots(t)
	req := Request{RequestID: "req-1", Prompt: "go", Workspace: ws, ResultSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
	if _, err := req.Validate(roots); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsNegativeLimits(t *testing.T) {
	roots, ws := testRoots(t)
	if _, err := (Request{RequestID: "req-1", Prompt: "go", Workspace: ws, MaxSubTurns: -1}).Validate(roots); err == nil {
		t.Fatal("expected negative max_sub_turns to be rejected")
	}
	if _, err := (Request{RequestID: "req-1", Prompt: "go", Workspace: ws, DeadlineMS: -1}).Validate(roots); err == nil {
		t.Fatal("expected negative deadline_ms to be rejected")
	}
}
