package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	return e, root
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func editArgsJSON(t *testing.T, a editArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEditRequiresPriorRead(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\n\nfunc f() {}\n")

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "func f() {}", NewString: "func g() {}",
	}))
	if !res.IsError {
		t.Fatal("expected an error editing a file that has not been read this session")
	}
	if !containsSubstring(res.Content, "has not been read") {
		t.Fatalf("expected a prior-read error, got: %s", res.Content)
	}
}

func TestEditUniqueMatchApplies(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\n\nfunc f() {}\n")
	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.go"}))

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "func f() {}", NewString: "func g() {}",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package a\n\nfunc g() {}\n" {
		t.Fatalf("unexpected file contents: %q", got)
	}
	// The result must carry the applied diff.
	if !containsSubstring(res.Content, "- func f() {}") || !containsSubstring(res.Content, "+ func g() {}") {
		t.Fatalf("expected an applied diff in the result, got: %s", res.Content)
	}
}

func TestEditRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "x := 1\nx := 1\n")
	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.go"}))

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "x := 1", NewString: "x := 2",
	}))
	if !res.IsError {
		t.Fatal("expected an error for a non-unique match without replace_all")
	}
	if !containsSubstring(res.Content, "matches 2 times") {
		t.Fatalf("expected the match count in the error, got: %s", res.Content)
	}
}

func TestEditReplaceAllAppliesEveryOccurrence(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "x := 1\nx := 1\n")
	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.go"}))

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "x := 1", NewString: "x := 2", ReplaceAll: true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(got) != "x := 2\nx := 2\n" {
		t.Fatalf("unexpected file contents: %q", got)
	}
}

func TestEditRejectsLineNumberPrefix(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\n")
	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.go"}))

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "     1\tpackage a", NewString: "package b",
	}))
	if !res.IsError {
		t.Fatal("expected an error for old_string carrying a line-number prefix")
	}
	if !containsSubstring(res.Content, "line-number prefix") {
		t.Fatalf("expected a targeted line-number-prefix error, got: %s", res.Content)
	}
}

func TestEditZeroMatchesReportsNearestCandidate(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\n\nfunc f() {\n\treturn\n}\n")
	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.go"}))

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "a.go", OldString: "func f() {\n\treturn nil\n}", NewString: "func f() {}",
	}))
	if !res.IsError {
		t.Fatal("expected an error for old_string with no match")
	}
	if !containsSubstring(res.Content, "nearest match") {
		t.Fatalf("expected a nearest-candidate hint, got: %s", res.Content)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsSubstring(s, sub string) bool {
	return strings.Contains(s, sub)
}
