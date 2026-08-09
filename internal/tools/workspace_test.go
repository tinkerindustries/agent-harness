package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathWithinWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolvePath(root, "a.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "a.txt"))
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestResolvePathRejectsDotDotEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolvePath(root, "../outside.txt"); err == nil {
		t.Fatal("expected an error for a ../ escape")
	}
}

func TestResolvePathRejectsAbsoluteEscape(t *testing.T) {
	root := t.TempDir()
	// An absolute path outside root is used as given, not silently nested
	// under root, so it is a genuine escape and must be rejected.
	if _, err := ResolvePath(root, "/etc/passwd"); err == nil {
		t.Fatal("expected an error for an absolute path outside the workspace")
	}
}

func TestResolvePathAcceptsAbsolutePathInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root2, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	// A model handed the workspace's absolute path routinely echoes it back
	// verbatim; that must resolve to the real file, not a doubled-up path
	// nested a second time under root.
	got, err := ResolvePath(root, filepath.Join(root2, "a.txt"))
	if err != nil {
		t.Fatalf("unexpected error resolving an absolute in-workspace path: %v", err)
	}
	want := filepath.Join(root2, "a.txt")
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestResolvePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := ResolvePath(root, "escape/secret.txt")
	if err == nil {
		t.Fatal("expected an error resolving a path through a symlink that escapes the workspace")
	}
	var escErr *ErrEscapesWorkspace
	if !asEscapeError(err, &escErr) {
		t.Fatalf("expected ErrEscapesWorkspace, got %v (%T)", err, err)
	}
}

func TestResolvePathAllowsNewFileUnderExistingDir(t *testing.T) {
	root := t.TempDir()
	got, err := ResolvePath(root, "new-file.txt")
	if err != nil {
		t.Fatalf("unexpected error resolving a not-yet-existing path: %v", err)
	}
	root2, _ := filepath.EvalSymlinks(root)
	if !withinRoot(root2, got) {
		t.Fatalf("resolved path %s escaped root %s", got, root)
	}
}

func TestResolvePathAllowsNewNestedDirs(t *testing.T) {
	root := t.TempDir()
	got, err := ResolvePath(root, "a/b/c/new-file.txt")
	if err != nil {
		t.Fatalf("unexpected error resolving nested not-yet-existing path: %v", err)
	}
	root2, _ := filepath.EvalSymlinks(root)
	if !withinRoot(root2, got) {
		t.Fatalf("resolved path %s escaped root %s", got, root)
	}
}

func asEscapeError(err error, target **ErrEscapesWorkspace) bool {
	if e, ok := err.(*ErrEscapesWorkspace); ok {
		*target = e
		return true
	}
	return false
}
