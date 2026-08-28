package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeClone fakes a clone: the one file GitHubOwners reads.
func writeClone(t *testing.T, root, name, remote string) {
	t.Helper()
	dir := filepath.Join(root, name, ".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	config := fmt.Sprintf("[remote \"origin\"]\n\turl = %s\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n", remote)
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestGitHubOwnersReadsEveryClone(t *testing.T) {
	root := t.TempDir()
	writeClone(t, root, "app", "https://github.com/SomeOrg/app.git")
	writeClone(t, root, "notes", "git@github.com:mrgeoffrich/notes.git")
	// Not a clone, and a clone of something that is not GitHub: both skipped.
	if err := os.MkdirAll(filepath.Join(root, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeClone(t, root, "vendored", "https://gitlab.com/someone/thing.git")

	got := GitHubOwners(root)
	want := []string{"someorg", "mrgeoffrich"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("GitHubOwners = %v, want %v", got, want)
	}
}

// TestGitHubOwnersDeduplicates pins that a session cloning two repositories
// from one account names that account once — the caller mints a token per
// owner, and asking twice for the same one is wasted work.
func TestGitHubOwnersDeduplicates(t *testing.T) {
	root := t.TempDir()
	writeClone(t, root, "one", "https://github.com/mrgeoffrich/one.git")
	writeClone(t, root, "two", "https://github.com/MrGeoffRich/two.git")

	if got := GitHubOwners(root); fmt.Sprint(got) != fmt.Sprint([]string{"mrgeoffrich"}) {
		t.Errorf("GitHubOwners = %v, want [mrgeoffrich]", got)
	}
}

// TestGitHubOwnersOnNothingIsEmpty pins that the absence of an answer is not
// a failure: an empty workspace, or one with no GitHub clone in it, means gh
// gets no token rather than the run breaking.
func TestGitHubOwnersOnNothingIsEmpty(t *testing.T) {
	if got := GitHubOwners(t.TempDir()); len(got) != 0 {
		t.Errorf("GitHubOwners on an empty workspace = %v, want none", got)
	}
	if got := GitHubOwners(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Errorf("GitHubOwners on a missing directory = %v, want none", got)
	}
}

// TestGitHubOwnerOfCredentialledRemote pins the URL shape a clone made with
// a credential in it carries, which is what an agent that ran `git remote
// set-url` by hand can leave behind.
func TestGitHubOwnerOfCredentialledRemote(t *testing.T) {
	if got := gitHubOwnerOf("https://x-access-token:ghs_secret@github.com/SomeOrg/app.git"); got != "someorg" {
		t.Errorf("gitHubOwnerOf = %q, want someorg", got)
	}
}
