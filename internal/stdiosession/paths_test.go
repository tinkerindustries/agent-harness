package stdiosession

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// symlinkOrSkip makes link point at target. Windows refuses to create a
// symlink without Developer Mode or an elevated process, and a directory
// junction is no substitute — filepath.EvalSymlinks has not followed mount
// points since Go 1.23 — so there the check that needs one is skipped rather
// than failed. Anywhere else a failure is a real one.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink: %v (Windows needs Developer Mode or an elevated process to create one)", err)
		}
		t.Fatalf("symlink: %v", err)
	}
}

// TestSameDirFollowsTheFilesystem pins that two spellings of one directory
// compare equal. A run started by a background completion carries the session
// row's workspace, which the harness resolved, so a continuation or a resume
// that names the same directory the way the client spelled it has to be
// accepted rather than refused as a change of directory.
func TestSameDirFollowsTheFilesystem(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)

	if !sameDir(real+sep, real+sep+".") {
		t.Error("a trailing separator and a . component make one directory compare unequal")
	}
	t.Run("through a symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "same-dir")
		symlinkOrSkip(t, real, link)
		if !sameDir(real, link) {
			t.Errorf("sameDir(%q, %q) = false, want true", real, link)
		}
		if !sameDir(link+sep, real+sep+".") {
			t.Error("a trailing separator and a . component make one directory compare unequal")
		}
	})
	if sameDir(real, t.TempDir()) {
		t.Error("two different directories compare equal")
	}
}

// TestResolveDirFallsBackForUnresolvablePaths pins the best-effort half: a
// directory that does not exist yet has no symlinks to follow, so it is
// compared by its cleaned absolute path instead of failing the create. The
// same fallback covers a path filepath.Abs refuses, by returning what it was
// given.
func TestResolveDirFallsBackForUnresolvablePaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	abs, err := filepath.Abs(missing)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveDir(missing); got != abs {
		t.Errorf("resolveDir(%q) = %q, want the absolute path %q", missing, got, abs)
	}

	// Two spellings of the same missing path still compare equal, which is
	// what keeps punctuation out of the decision.
	dotted := filepath.Join(filepath.Dir(missing), ".", filepath.Base(missing))
	if !sameDir(missing, dotted) {
		t.Errorf("sameDir(%q, %q) = false, want true", missing, dotted)
	}
	if sameDir(missing, missing+"-sibling") {
		t.Error("two different missing paths compare equal")
	}
}
