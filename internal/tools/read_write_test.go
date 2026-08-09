package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadReturnsLineNumberedContent(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.txt", "one\ntwo\nthree\n")

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "a.txt"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "1\tone") || !strings.Contains(res.Content, "2\ttwo") {
		t.Fatalf("expected cat -n style output, got: %q", res.Content)
	}
}

func TestReadRejectsWorkspaceEscape(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "../../etc/passwd"}))
	if !res.IsError {
		t.Fatal("expected an error reading a path that escapes the workspace")
	}
}

func TestWriteNewFileNeedsNoPriorRead(t *testing.T) {
	e, root := newTestExecutor(t)
	res := execWrite(t.Context(), e, mustJSON(t, writeArgs{FilePath: "new.txt", Content: "hi"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestWriteOverwriteRequiresPriorRead(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "existing.txt", "old")

	res := execWrite(t.Context(), e, mustJSON(t, writeArgs{FilePath: "existing.txt", Content: "new"}))
	if !res.IsError {
		t.Fatal("expected an error overwriting a file that has not been read")
	}

	execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "existing.txt"}))
	res = execWrite(t.Context(), e, mustJSON(t, writeArgs{FilePath: "existing.txt", Content: "new"}))
	if res.IsError {
		t.Fatalf("unexpected error after reading first: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "existing.txt"))
	if string(got) != "new" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestWriteRejectsWorkspaceEscape(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := execWrite(t.Context(), e, mustJSON(t, writeArgs{FilePath: "../escape.txt", Content: "x"}))
	if !res.IsError {
		t.Fatal("expected an error writing a path that escapes the workspace")
	}
}
