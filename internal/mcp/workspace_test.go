package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkspaceNameResolvesExistingDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "myproject"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveWorkspaceName([]string{root}, "myproject")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(resolvedRoot, "myproject")
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestResolveWorkspaceNameRejectsMissingName(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveWorkspaceName([]string{root}, ""); err == nil {
		t.Fatal("expected an error for an empty workspace name")
	}
}

// TestResolveWorkspaceNameRejectsPaths is the host/container boundary
// docs/DESIGN.md's Safety section calls out: workspace is a NAME, not a
// path, because the caller runs on the host and the harness runs in a
// container and the two disagree about what a path even means. A separator
// anywhere in the value is rejected outright rather than partially
// interpreted.
func TestResolveWorkspaceNameRejectsPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"../escape",
		"..",
		".",
		"sub/inner",
		"/etc/passwd",
		"a\\b",
	}
	for _, name := range cases {
		if _, err := resolveWorkspaceName([]string{root}, name); err == nil {
			t.Fatalf("expected %q to be rejected as a path, not a bare name", name)
		}
	}
}

func TestResolveWorkspaceNameRejectsNonexistent(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveWorkspaceName([]string{root}, "does-not-exist"); err == nil {
		t.Fatal("expected an error for a name with no matching directory")
	}
}

func TestResolveWorkspaceNameRejectsNoConfiguredRoots(t *testing.T) {
	if _, err := resolveWorkspaceName(nil, "anything"); err == nil {
		t.Fatal("expected an error when no workspace roots are configured")
	}
}

// TestResolveWorkspaceNameRejectsSymlinkEscape proves the containment check
// is the same symlink-safe one tools.ResolvePath applies elsewhere: a
// workspace name that is a symlink pointing outside root must be rejected,
// not silently followed to wherever it leads.
func TestResolveWorkspaceNameRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveWorkspaceName([]string{root}, "escape"); err == nil {
		t.Fatal("expected a symlink escaping root to be rejected")
	}
}

func TestListWorkspaceNamesListsSubdirsAcrossRoots(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, dir := range []string{filepath.Join(rootA, "alpha"), filepath.Join(rootA, "beta"), filepath.Join(rootB, "gamma")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file, not a directory, must not be reported as a valid workspace
	// name.
	if err := os.WriteFile(filepath.Join(rootA, "not-a-dir.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listWorkspaceNames([]string{rootA, rootB})
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
