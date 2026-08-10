package worktree

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Root returns the root of the git worktree the current directory is in —
// the linked worktree's own root, not the main checkout's (unlike a plain
// path walk up to the outermost .git, `--show-toplevel` resolves per linked
// worktree).
func Root() (string, error) {
	return gitOutput("rev-parse", "--show-toplevel")
}

// IsMainWorktree reports whether root is the repository's original checkout
// rather than one added with `git worktree add`. For the main worktree,
// --git-dir and --git-common-dir are the same path (.git); for a linked
// worktree, --git-dir points at .git/worktrees/<name> while --git-common-dir
// still points at the main checkout's .git.
func IsMainWorktree(root string) (bool, error) {
	gitDir, err := gitOutputIn(root, "rev-parse", "--git-dir")
	if err != nil {
		return false, err
	}
	commonDir, err := gitOutputIn(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, err
	}
	return gitDir == commonDir, nil
}

// MainRoot returns the main checkout's root, given the root of any of its
// worktrees (linked or main). This is where .claude/worktrees/ and the
// main .env — the seed for a new worktree's .env — live, regardless of
// which linked worktree the caller is standing in.
func MainRoot(root string) (string, error) {
	commonDir, err := gitOutputIn(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	return filepath.Dir(filepath.Clean(commonDir)), nil
}

// CurrentBranch returns the branch checked out at root, or "" for a detached
// HEAD.
func CurrentBranch(root string) (string, error) {
	branch, err := gitOutputIn(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

func gitOutputIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s (in %s): %s", strings.Join(args, " "), dir, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s (in %s): %w", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
