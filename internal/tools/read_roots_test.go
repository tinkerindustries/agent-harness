package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillRoot returns a directory outside any test workspace, holding one
// SKILL.md and a companion file, and the executor allowed to read it.
func skillRoot(t *testing.T) (*Executor, string, string) {
	t.Helper()
	e, ws := newTestExecutor(t)
	root := t.TempDir()
	writeFile(t, root, "SKILL.md", "---\nname: demo\ndescription: Demo.\n---\n\nRun scripts/run.sh.\n")
	if err := os.MkdirAll(filepath.Join(root, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "references"), "guide.md", "the guide\n")
	e.ReadRoots = []string{root}
	return e, ws, root
}

// A skill installed outside the workspace is readable once discovery has
// named its directory, and its absolute path is what the model passes back.
func TestReadResolvesUnderAReadRoot(t *testing.T) {
	e, _, root := skillRoot(t)

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: filepath.Join(root, "SKILL.md")}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Run scripts/run.sh.") {
		t.Fatalf("did not read the skill body: %q", res.Content)
	}

	res = execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: filepath.Join(root, "references", "guide.md")}))
	if res.IsError {
		t.Fatalf("unexpected error reading a companion file: %s", res.Content)
	}
}

// The allowance is only for the roots discovery reported. Everything else
// outside the workspace is refused exactly as it was before.
func TestReadRefusesOutsideEveryRoot(t *testing.T) {
	e, _, _ := skillRoot(t)
	elsewhere := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(elsewhere, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: elsewhere}))
	if !res.IsError {
		t.Fatalf("expected an escape refusal, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "escapes workspace root") {
		t.Fatalf("expected the workspace escape error, got: %q", res.Content)
	}
}

// Reading is all a read root buys. A write into a skill directory is still an
// escape, so the model cannot edit the user's installed skills in place.
func TestWriteCannotReachAReadRoot(t *testing.T) {
	e, _, root := skillRoot(t)
	target := filepath.Join(root, "SKILL.md")

	res := execWrite(t.Context(), e, mustJSON(t, writeArgs{FilePath: target, Content: "changed"}))
	if !res.IsError {
		t.Fatalf("expected the write to be refused, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "escapes workspace root") {
		t.Fatalf("expected the workspace escape error, got: %q", res.Content)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Run scripts/run.sh.") {
		t.Fatalf("the skill file was modified: %q", data)
	}
}

// List answers for a skill directory, which is how a model finds the files a
// skill bundles beside its SKILL.md.
func TestListReachesAReadRoot(t *testing.T) {
	e, _, root := skillRoot(t)

	res := execList(t.Context(), e, mustJSON(t, listArgs{Path: root}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "references/") || !strings.Contains(res.Content, "SKILL.md") {
		t.Fatalf("expected the skill directory's entries, got: %q", res.Content)
	}
}

// Glob under a skill directory reports absolute paths, because a path
// relative to that root would not resolve back through Read.
func TestGlobUnderAReadRootReportsAbsolutePaths(t *testing.T) {
	e, _, root := skillRoot(t)

	res := execGlob(t.Context(), e, mustJSON(t, globArgs{Pattern: "**/*.md", Path: root}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	want := filepath.ToSlash(filepath.Join(root, "references", "guide.md"))
	if !strings.Contains(res.Content, want) {
		t.Fatalf("expected %q in the matches, got: %q", want, res.Content)
	}
}

// Grep searches a skill directory too, so a skill that ships reference
// material can be searched the way any other directory is.
func TestGrepReachesAReadRoot(t *testing.T) {
	e, _, root := skillRoot(t)
	e.RG = ""

	res := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "the guide", Path: root, OutputMode: "content"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "the guide") {
		t.Fatalf("expected the match, got: %q", res.Content)
	}
}
