package claudemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeClaudeMD creates path with the given content, making parents.
func writeClaudeMD(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const validClaudeMD = "# myrepo\n\nInstructions for working in this repository.\n"

// A queue-driven run clones each repository into its own subdirectory of the
// workspace, so the CLAUDE.md sits one level down.
func TestDiscoverClonedRepoLayout(t *testing.T) {
	ws := t.TempDir()
	writeClaudeMD(t, filepath.Join(ws, "myrepo", "CLAUDE.md"), validClaudeMD)

	col := Discover(ws)
	if len(col.Files) != 1 {
		t.Fatalf("got %d files, want 1: %+v", len(col.Files), col.Files)
	}
	got := col.Files[0]
	if got.Path != "myrepo/CLAUDE.md" {
		t.Errorf("path = %q, want myrepo/CLAUDE.md", got.Path)
	}
	if got.Content != validClaudeMD {
		t.Errorf("content = %q, want %q", got.Content, validClaudeMD)
	}
	if got.Truncated {
		t.Errorf("a small file should not be truncated")
	}
}

// A CLI run points the workspace straight at a checkout, so the CLAUDE.md
// sits at the workspace root.
func TestDiscoverWorkspaceIsCheckout(t *testing.T) {
	ws := t.TempDir()
	writeClaudeMD(t, filepath.Join(ws, "CLAUDE.md"), validClaudeMD)

	col := Discover(ws)
	if len(col.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(col.Files))
	}
	if col.Files[0].Path != "CLAUDE.md" {
		t.Errorf("path = %q, want CLAUDE.md", col.Files[0].Path)
	}
}

// Only a root CLAUDE.md counts. AGENTS.md, .claude/CLAUDE.md, and nested
// per-package files are a separate job; dot-directories are skipped the way
// internal/skills skips them.
func TestDiscoverPicksUpRootClaudeMDOnly(t *testing.T) {
	ws := t.TempDir()
	writeClaudeMD(t, filepath.Join(ws, "repo", "CLAUDE.md"), validClaudeMD)
	writeClaudeMD(t, filepath.Join(ws, "repo", "AGENTS.md"), "not this\n")
	writeClaudeMD(t, filepath.Join(ws, "repo", ".claude", "CLAUDE.md"), "not this either\n")
	writeClaudeMD(t, filepath.Join(ws, "repo", "pkg", "CLAUDE.md"), "nested files are a separate job\n")
	writeClaudeMD(t, filepath.Join(ws, ".hidden", "CLAUDE.md"), "dot directories are skipped\n")

	col := Discover(ws)
	if len(col.Files) != 1 {
		t.Fatalf("got %d files, want only repo/CLAUDE.md: %+v", len(col.Files), col.Files)
	}
	if col.Files[0].Path != "repo/CLAUDE.md" {
		t.Errorf("path = %q, want repo/CLAUDE.md", col.Files[0].Path)
	}
}

// Two repositories in one workspace can both carry one; both are listed in a
// stable order by workspace-relative path.
func TestDiscoverSortsByPath(t *testing.T) {
	ws := t.TempDir()
	writeClaudeMD(t, filepath.Join(ws, "zeta", "CLAUDE.md"), validClaudeMD)
	writeClaudeMD(t, filepath.Join(ws, "alpha", "CLAUDE.md"), validClaudeMD)

	col := Discover(ws)
	if len(col.Files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(col.Files), col.Files)
	}
	if col.Files[0].Path != "alpha/CLAUDE.md" || col.Files[1].Path != "zeta/CLAUDE.md" {
		t.Errorf("order is not path-sorted: %q then %q", col.Files[0].Path, col.Files[1].Path)
	}
}

// Discovery never fails a run: a missing workspace and a directory that
// happens to be named CLAUDE.md both yield no entry and no error.
func TestDiscoverSkipsUnusable(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "repo", "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if col := Discover(filepath.Join(t.TempDir(), "does-not-exist")); len(col.Files) != 0 {
		t.Errorf("missing workspace yielded %+v", col.Files)
	}
	if col := Discover(ws); len(col.Files) != 0 {
		t.Errorf("got %d files, want 0: %+v", len(col.Files), col.Files)
	}
}

// A file larger than MaxFileBytes is cut, and the rendered block says so and
// names the path so the agent can Read the rest itself.
func TestDiscoverTruncatesLargeFile(t *testing.T) {
	ws := t.TempDir()
	content := strings.Repeat("0123456789abcdef", 2049) // 32784 bytes > 32 KiB
	writeClaudeMD(t, filepath.Join(ws, "big", "CLAUDE.md"), content)

	col := Discover(ws)
	if len(col.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(col.Files))
	}
	f := col.Files[0]
	if !f.Truncated {
		t.Fatal("expected a truncated file")
	}
	if len(f.Content) > MaxFileBytes {
		t.Errorf("content is %d bytes, want at most %d", len(f.Content), MaxFileBytes)
	}
	rendered := col.Render()
	if !strings.Contains(rendered, "big/CLAUDE.md") {
		t.Errorf("truncation notice should name the path:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Read") {
		t.Errorf("truncation notice should point the agent at the Read tool:\n%s", rendered)
	}
}

// The total budget is MaxTotalBytes across all files. A later file is cut to
// the remaining budget; a file that no budget is left for is dropped, with
// the shortfall stated in the render rather than silently omitted.
func TestDiscoverBoundsTotal(t *testing.T) {
	ws := t.TempDir()
	chunk := strings.Repeat("x", 24*1024) // 24 KiB
	writeClaudeMD(t, filepath.Join(ws, "a", "CLAUDE.md"), chunk)
	writeClaudeMD(t, filepath.Join(ws, "b", "CLAUDE.md"), chunk)
	writeClaudeMD(t, filepath.Join(ws, "c", "CLAUDE.md"), chunk)
	writeClaudeMD(t, filepath.Join(ws, "d", "CLAUDE.md"), chunk)

	col := Discover(ws)
	if len(col.Files) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(col.Files), col.Files)
	}
	total := 0
	for _, f := range col.Files {
		total += len(f.Content)
	}
	if total > MaxTotalBytes {
		t.Errorf("total content is %d bytes, want at most %d", total, MaxTotalBytes)
	}
	if col.Files[2].Path != "c/CLAUDE.md" || !col.Files[2].Truncated {
		t.Errorf("the third file should be cut to the remaining budget: %+v", col.Files[2])
	}
	if len(col.Files[2].Content) != 16*1024 {
		t.Errorf("third file cut to %d bytes, want %d", len(col.Files[2].Content), 16*1024)
	}
	if col.Dropped != 1 {
		t.Errorf("dropped = %d, want 1 (d/CLAUDE.md)", col.Dropped)
	}
	if rendered := col.Render(); !strings.Contains(rendered, "1 further CLAUDE.md file") {
		t.Errorf("render should say what was dropped:\n%s", rendered)
	}
}

// Truncation must never split a multi-byte rune.
func TestTruncateDoesNotSplitRune(t *testing.T) {
	content := strings.Repeat("é", 20000) // 40000 bytes of 2-byte runes
	if got := truncate(content, MaxFileBytes); !utf8.ValidString(got) {
		t.Fatal("truncated content is not valid UTF-8")
	}
}

// An empty collection renders to nothing so the opening message stays
// byte-identical to a run with no CLAUDE.md (docs/CACHE.md).
func TestRenderEmpty(t *testing.T) {
	if got := (Collection{}).Render(); got != "" {
		t.Errorf("render of an empty collection = %q, want empty", got)
	}
}

func TestRenderHeadingsAndLeadIn(t *testing.T) {
	col := Collection{Files: []File{
		{Path: "CLAUDE.md", Content: "# Project\n\nRules.\n"},
		{Path: "myrepo/CLAUDE.md", Content: "# myrepo\n\nOther rules.\n"},
	}}
	got := col.Render()
	for _, want := range []string{"CLAUDE.md", "myrepo/CLAUDE.md", "# Project", "instructions", "Follow"} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q:\n%s", want, got)
		}
	}
	// The lead-in comes first, then a heading per file in path order.
	if !strings.HasPrefix(got, "The repositories cloned into this workspace") {
		t.Errorf("render should open with the lead-in:\n%s", got)
	}
	heading := strings.Index(got, "### CLAUDE.md")
	content := strings.Index(got, "# Project")
	if heading < 0 || content < 0 || heading > content {
		t.Errorf("the root file's heading should precede its content:\n%s", got)
	}
}

// A file whose content does not end in a newline still separates cleanly
// from what follows in the message.
func TestRenderAddsMissingTrailingNewline(t *testing.T) {
	col := Collection{Files: []File{{Path: "CLAUDE.md", Content: "no trailing newline"}}}
	if got := col.Render(); !strings.Contains(got, "no trailing newline\n\n") {
		t.Errorf("render should separate the file from what follows:\n%q", got)
	}
}
