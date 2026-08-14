package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Install writes the shipped skills in src into workspace/skills, the
// directory internal/workspace creates and Discover scans
// (WorkspaceSkillsDir). It returns how many skills it wrote.
//
// Only a directory holding a SKILL.md is a skill: everything else at the top
// level is skipped, which is what lets the source tree carry a README
// explaining itself without that README landing in every workspace. Below a
// skill's own directory the copy is verbatim and recursive, because a skill's
// references and scripts are part of it.
//
// A nil src installs nothing and is not an error — it is how a caller says
// this deployment ships no skills, and how every test that does not care
// about them stays unchanged.
//
// The copy is deliberately not atomic and deliberately not fatal to its
// caller. A skill that fails to write is one entry missing from a catalogue,
// which is a worse session rather than a broken one, so the error names what
// could not be written and the caller logs it (internal/worker) rather than
// failing the run — the same posture Discover takes, where an unreadable
// workspace yields an empty catalogue instead of an error.
func Install(src fs.FS, workspace string) (int, error) {
	if src == nil {
		return 0, nil
	}
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return 0, fmt.Errorf("read shipped skills: %w", err)
	}
	dest := filepath.Join(workspace, WorkspaceSkillsDir)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", dest, err)
	}

	var written int
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// fs paths are always slash-separated, whatever the host separator
		// is, so the source side joins with path and only the destination
		// side with filepath.
		if _, err := fs.Stat(src, path.Join(name, "SKILL.md")); err != nil {
			continue
		}
		if err := copyTree(src, name, filepath.Join(dest, name)); err != nil {
			failures = append(failures, fmt.Errorf("install skill %s: %w", name, err))
			continue
		}
		written++
	}
	return written, errors.Join(failures...)
}

// copyTree copies one skill directory out of the embedded tree. Directories
// are created 0o755 and files written 0o644: the embedded FS reports every
// file as read-only regardless of what the source tree held, so the modes are
// chosen here rather than carried across, and a skill is documentation the
// session reads rather than something it executes.
func copyTree(src fs.FS, root, dest string) error {
	return fs.WalkDir(src, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, root), "/")
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := src.Open(name)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
