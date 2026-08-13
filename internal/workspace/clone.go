// Package workspace prepares the directory a queue-driven run works in: one
// folder per session, holding a clone of every repository the request named
// (docs/DESIGN.md §4.10).
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// Prepare creates root/sessionID with a scratch/ subdirectory and clones
// repos into it, returning the absolute path the session runs against. The
// directory must not already exist: a session id names exactly one run, so an
// existing folder means something else owns it. Each clone then has its Node
// dependencies installed, best-effort (deps.go).
//
// A partly built workspace is left on disk when a clone fails. The run is
// over at that point and the directory is the only record of how far it got.
func Prepare(ctx context.Context, root, sessionID string, repos []queue.Repo) (string, error) {
	if root == "" {
		return "", fmt.Errorf("workspace: no workspace root is configured; set DEEPSEEK_WORKSPACE_ROOT")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return "", fmt.Errorf("workspace: create root %q: %w", absRoot, err)
	}

	dir := filepath.Join(absRoot, sessionID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: create %q: %w", dir, err)
	}

	// scratch is where the model puts files that are not part of the
	// deliverable — a screenshot for ReviewScreenshot, a scratch note, a
	// temporary download (internal/session/prompt.go points it there). It sits
	// beside the clones, never in /tmp (shared by every concurrent session in
	// the container) and never inside a repository (risks being swept into a
	// commit), and it lives and dies with the session directory exactly like a
	// clone does.
	if err := os.MkdirAll(filepath.Join(dir, "scratch"), 0o755); err != nil {
		return "", fmt.Errorf("workspace: create scratch dir in %q: %w", dir, err)
	}

	for _, repo := range repos {
		if err := clone(ctx, dir, repo); err != nil {
			return dir, err
		}
		installDependencies(ctx, filepath.Join(dir, repo.Dir()))
	}
	return dir, nil
}

// clone checks one repository out into parent. Validation (queue.Request)
// has already rejected a URL or branch that git would read as an option or
// as a command to run.
func clone(ctx context.Context, parent string, repo queue.Repo) error {
	branch := repo.BranchOrDefault()
	cmd := exec.CommandContext(ctx, "git", "clone", "--branch", branch, "--", repo.URL, repo.Dir())
	cmd.Dir = parent
	// Without these git waits on a terminal that is not there when a private
	// repository asks for credentials, and the run burns its whole deadline
	// on a prompt nobody can answer.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("workspace: clone %s (branch %s): %w: %s", repo.URL, branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}
