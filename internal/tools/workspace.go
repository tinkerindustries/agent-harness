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

// scratchDir is the workspace subdirectory that holds what a run produces
// but does not deliver: screenshots, attachments, scratch scripts. It is
// named here rather than at each use because two rules now key off it — the
// Screenshot tool's output confinement and the scratch-relative reading
// below.
const scratchDir = "scratch"

// resolveScratchRelative resolves userPath as though it were relative to the
// workspace's scratch directory rather than to the workspace root. The join
// happens before resolution, so filepath.Join cleans any traversal in
// userPath first and the result cannot climb out of scratch/ by spelling
// "../"; whatever comes back is still checked against the workspace root by
// ResolvePath.
//
// Only meaningful for a relative userPath. An absolute path is a statement
// about where the file is, and re-rooting one would be guessing against what
// the caller said.
func resolveScratchRelative(root, userPath string) (string, error) {
	return ResolvePath(root, filepath.Join(scratchDir, userPath))
}

// resolveImagePath resolves a path an image-reading tool was handed, taking a
// relative path that names nothing as scratch-relative on a second attempt.
//
// The models this harness runs write a screenshot to "after/01.png" and then
// hand that same string back to Glance, Ground, or Detect, because that is
// how the path reads in their own previous tool call — and the file is at
// scratch/after/01.png, since Screenshot puts every relative path there
// (resolveScreenshotOutput). Refusing the read teaches nothing the model does
// not already believe it did right; it just costs a sub-turn per image. So a
// path that resolves to nothing gets tried once more under scratch/.
//
// Existence is the whole test, and the original resolution is what comes back
// when neither exists — the caller's own "file not found: <userPath>" is a
// better error than one naming a scratch path the caller never asked for.
// A path that does exist is never second-guessed, so "scratch/a.png" resolves
// exactly as before and can never become "scratch/scratch/a.png".
func resolveImagePath(root, userPath string) (string, error) {
	path, err := ResolvePath(root, userPath)
	if err != nil || filepath.IsAbs(userPath) {
		return path, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return path, nil
	}
	alt, altErr := resolveScratchRelative(root, userPath)
	if altErr != nil {
		return path, nil
	}
	if _, statErr := os.Stat(alt); statErr == nil {
		return alt, nil
	}
	return path, nil
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
