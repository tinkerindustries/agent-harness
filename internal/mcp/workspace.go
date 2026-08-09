package mcp

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// resolveWorkspaceName turns a workspace NAME — a bare directory name, not
// a path — into the absolute container-side path the harness itself will
// accept. The caller runs on the host and the harness runs in a container
// (docs/DESIGN.md), so a host path in the request would name the wrong
// filesystem entirely; a name sidesteps that by construction. It is checked
// against the same roots and the same symlink-safe containment
// (tools.ResolvePath) the harness's own queue.Request.Validate applies, so a
// workspace this resolves is guaranteed to resolve the same way again when
// the published request reaches the worker pool.
func resolveWorkspaceName(roots []string, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("workspace is required")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("workspace %q must be a bare name (see harness://workspaces), not a path", name)
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("no workspace roots are configured; set DEEPSEEK_WORKSPACE_ROOTS")
	}

	var lastErr error
	for _, root := range roots {
		resolved, err := tools.ResolvePath(root, name)
		if err != nil {
			lastErr = err
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			lastErr = fmt.Errorf("workspace %q does not exist under root %q", name, root)
			continue
		}
		return resolved, nil
	}
	return "", fmt.Errorf("workspace %q not found under any configured root: %w", name, lastErr)
}

// listWorkspaceNames enumerates the subdirectories of every configured
// root, deduplicated and sorted, for harness://workspaces. A name absent
// from this list is not necessarily invalid — a directory created after the
// last read of this list, say — but every name present here is guaranteed
// to resolve.
func listWorkspaceNames(roots []string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
