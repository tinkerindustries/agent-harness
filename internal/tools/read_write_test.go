package tools

import (
	"fmt"
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

// TestReadMarksALineLimitedRead proves a read cut short by its line limit says
// so, names the file's real length, and gives the offset to resume from.
// Without the note the numbered lines look exactly like a whole file, and a
// model that believes it has seen the end edits on that belief.
func TestReadMarksALineLimitedRead(t *testing.T) {
	e, root := newTestExecutor(t)
	var body strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	writeFile(t, root, "long.txt", body.String())

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "long.txt", Limit: 10}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[showing lines 1-10 of 50; read again with offset=11 for the rest]") {
		t.Fatalf("expected the cut to be marked with the total and the resume offset, got: %q", res.Content)
	}
	// The note is the only marker; the lines themselves are unchanged.
	if !strings.Contains(res.Content, "10\tline 10") || strings.Contains(res.Content, "line 11\n") {
		t.Fatalf("expected exactly the first ten lines above the note, got: %q", res.Content)
	}
}

// TestReadOffsetIsCarriedIntoTheNote covers resuming: the note has to report
// the window actually shown, not the first ten lines of the file.
func TestReadOffsetIsCarriedIntoTheNote(t *testing.T) {
	e, root := newTestExecutor(t)
	var body strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	writeFile(t, root, "long.txt", body.String())

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "long.txt", Offset: 11, Limit: 10}))
	if !strings.Contains(res.Content, "[showing lines 11-20 of 50; read again with offset=21 for the rest]") {
		t.Fatalf("expected the note to describe the window that was shown, got: %q", res.Content)
	}
}

// TestReadWholeFileCarriesNoNote pins that the note only appears when it is
// true. A file that ends inside the limit is not cut, and saying so would
// train the model to chase a rest that does not exist.
func TestReadWholeFileCarriesNoNote(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "short.txt", "one\ntwo\nthree\n")

	// Exactly at the limit: the last line is shown and the file ends there.
	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "short.txt", Limit: 3}))
	if strings.Contains(res.Content, "showing lines") {
		t.Fatalf("expected no cut note for a file that ends at the limit, got: %q", res.Content)
	}

	res = execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "short.txt"}))
	if strings.Contains(res.Content, "showing lines") {
		t.Fatalf("expected no cut note for a whole file, got: %q", res.Content)
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
