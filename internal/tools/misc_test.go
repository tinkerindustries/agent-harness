package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobFindsNestedFiles(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a")
	mustMkdirAll(t, root, "sub")
	writeFile(t, root, "sub/b.go", "package sub")
	writeFile(t, root, "c.txt", "not go")

	res := execGlob(t.Context(), e, mustJSON(t, globArgs{Pattern: "**/*.go"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "sub/b.go") {
		t.Fatalf("expected both go files, got: %s", res.Content)
	}
	if strings.Contains(res.Content, "c.txt") {
		t.Fatalf("did not expect c.txt to match **/*.go, got: %s", res.Content)
	}
}

func TestGrepFindsMatchesAndRespectsOutputMode(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\nfunc Foo() {}\n")
	writeFile(t, root, "b.go", "package b\n")

	files := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo"}))
	if !strings.Contains(files.Content, "a.go") || strings.Contains(files.Content, "b.go") {
		t.Fatalf("expected only a.go in files_with_matches, got: %s", files.Content)
	}

	content := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo", OutputMode: "content"}))
	if !strings.Contains(content.Content, "a.go:2:") {
		t.Fatalf("expected a path:line:text match, got: %s", content.Content)
	}
}

func TestListShowsEntriesAndRespectsIgnore(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "x")
	writeFile(t, root, "a_test.go", "x")
	mustMkdirAll(t, root, "sub")

	res := execList(t.Context(), e, mustJSON(t, listArgs{Path: "."}))
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "sub/") {
		t.Fatalf("expected a.go and sub/, got: %s", res.Content)
	}

	filtered := execList(t.Context(), e, mustJSON(t, listArgs{Path: ".", Ignore: []string{"*_test.go"}}))
	if strings.Contains(filtered.Content, "a_test.go") {
		t.Fatalf("expected a_test.go to be ignored, got: %s", filtered.Content)
	}
}

func TestTodoWriteValidatesAndStores(t *testing.T) {
	e, _ := newTestExecutor(t)
	bad := execTodoWrite(t.Context(), e, mustJSON(t, todoWriteArgs{Todos: []Todo{{Content: "x", Status: "bogus", ActiveForm: "Xing"}}}))
	if !bad.IsError {
		t.Fatal("expected an error for an invalid status")
	}

	good := execTodoWrite(t.Context(), e, mustJSON(t, todoWriteArgs{Todos: []Todo{
		{Content: "Run tests", Status: "in_progress", ActiveForm: "Running tests"},
	}}))
	if good.IsError {
		t.Fatalf("unexpected error: %s", good.Content)
	}
	if len(e.Todos()) != 1 {
		t.Fatalf("expected the todo to be stored, got %d", len(e.Todos()))
	}
}

func TestCompleteValidatesAgainstResultSchema(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.ResultSchema = json.RawMessage(`{
		"type": "object",
		"properties": {"count": {"type": "integer"}},
		"required": ["count"],
		"additionalProperties": false
	}`)

	res, _, ok := e.execComplete(mustJSON(t, completeArgs{Summary: "done", Result: json.RawMessage(`{"count": "not a number"}`)}))
	if ok {
		t.Fatal("expected schema validation to fail")
	}
	if !res.IsError || !strings.Contains(res.Content, "count") {
		t.Fatalf("expected a schema error naming count, got: %s", res.Content)
	}

	res2, payload, ok2 := e.execComplete(mustJSON(t, completeArgs{Summary: "done", Result: json.RawMessage(`{"count": 3}`), Status: "done"}))
	if !ok2 {
		t.Fatalf("expected valid payload to succeed: %s", res2.Content)
	}
	if payload.Summary != "done" || payload.Status != "done" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func mustMkdirAll(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
}
