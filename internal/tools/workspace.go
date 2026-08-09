package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapesWorkspace is returned when a path resolves outside the
// workspace root, including via a symlink.
type ErrEscapesWorkspace struct {
	Path string
	Root string
}

func (e *ErrEscapesWorkspace) Error() string {
	return fmt.Sprintf("path %q escapes workspace root %q", e.Path, e.Root)
}

// ResolvePath resolves userPath against root and rejects any result outside
// it, checked after symlink resolution (docs/TOOLS.md, "Execution rules").
// A relative userPath is joined onto root, the ordinary case. An absolute
// userPath is used as given rather than nested under root a second time —
// Read's schema documents "absolute or workspace-relative", and a model
// handed an absolute workspace path in the opening message routinely
// echoes it back verbatim; treating that as relative would silently
// double the prefix and report a real file as missing. Either way the
// result must still resolve inside root or it is rejected as an escape.
// The path need not exist: ResolvePath walks up to the deepest existing
// ancestor to resolve symlinks there, then joins the remaining components
// literally, so a tool can validate a path it is about to create.
func ResolvePath(root, userPath string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	root, err = evalSymlinksBestEffort(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}

	var candidate string
	if filepath.IsAbs(userPath) {
		candidate = filepath.Clean(userPath)
	} else {
		candidate = filepath.Clean(filepath.Join(root, userPath))
	}

	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", err
	}

	if !withinRoot(root, resolved) {
		return "", &ErrEscapesWorkspace{Path: userPath, Root: root}
	}
	return resolved, nil
}

// withinRoot reports whether p is root itself or a descendant of it,
// comparing cleaned absolute paths with an explicit separator boundary so
// "/ws-evil" is never mistaken for a child of "/ws".
func withinRoot(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// resolveExistingPrefix walks p's ancestors from the top, resolving
// symlinks at each level that exists, and rejoins any trailing components
// that do not exist yet literally. This lets ResolvePath validate a path a
// tool is about to create without requiring it to exist first.
func resolveExistingPrefix(p string) (string, error) {
	if _, err := os.Lstat(p); err == nil {
		return evalSymlinksBestEffort(p)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	parent, base := filepath.Dir(p), filepath.Base(p)
	if parent == p {
		// Reached the filesystem root without finding an existing ancestor.
		return p, nil
	}
	resolvedParent, err := resolveExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, base), nil
}

func evalSymlinksBestEffort(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return resolved, nil
}
