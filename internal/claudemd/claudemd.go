// Package claudemd discovers the root CLAUDE.md files shipped inside the
// repositories a run cloned and renders their contents into the opening user
// message.
//
// The harness clones repositories into a per-run workspace but ignores any
// CLAUDE.md they carry, so a session would otherwise start with no project
// instructions. This package finds each repository's root CLAUDE.md and puts
// its text in front of the model, ahead of the skill catalogue and the task.
// Nothing here touches the system prompt or the tool array, so adding a
// CLAUDE.md to a repository does not disturb the cached head (docs/CACHE.md).
package claudemd

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on what reaches the model. The block rides in every request of the
// run, so a repository with a huge CLAUDE.md would otherwise spend real
// tokens per sub-turn restating instructions the model has already seen.
const (
	// MaxFileBytes bounds a single CLAUDE.md read during discovery. A file
	// larger than this is truncated, and the rendered block says so and names
	// the path so the agent can Read the rest itself.
	MaxFileBytes = 32 * 1024
	// MaxTotalBytes bounds the whole block across all files. Files that do
	// not fit are cut to the remaining budget or dropped, with the shortfall
	// stated in the rendered text rather than silently omitted.
	MaxTotalBytes = 64 * 1024
)

// File is one discovered CLAUDE.md.
type File struct {
	// Path is the CLAUDE.md path relative to the workspace root, which is the
	// form the model passes back to Read.
	Path string
	// Content is the file's text, truncated to fit MaxFileBytes and the
	// run's total budget.
	Content string
	// Truncated reports that Content is not the whole file.
	Truncated bool
}

// Collection is the result of one discovery pass.
type Collection struct {
	Files []File
	// Dropped counts CLAUDE.md files found but omitted entirely by
	// MaxTotalBytes.
	Dropped int
}

// Discover scans workspace for root CLAUDE.md files and returns them in a
// stable order. Two layouts are covered. A queue-driven run clones each
// repository into its own subdirectory of the workspace, so the file sits at
// <workspace>/<repo>/CLAUDE.md. A CLI run points the workspace straight at a
// checkout, so it sits one level higher at <workspace>/CLAUDE.md. Both are
// scanned; nothing deeper is, and dot-directories are skipped the way
// internal/skills skips them.
//
// Discovery never fails a run. A workspace that cannot be read, a missing
// file, and a file that cannot be read all yield no entry and no error.
func Discover(workspace string) Collection {
	paths := []string{filepath.Join(workspace, "CLAUDE.md")}
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				paths = append(paths, filepath.Join(workspace, entry.Name(), "CLAUDE.md"))
			}
		}
	}
	// Sorting by path keeps two repositories that both carry a CLAUDE.md in a
	// fixed order, independent of ReadDir's own ordering.
	sort.Strings(paths)

	var found []File
	for _, path := range paths {
		if f, ok := readClaudeMD(workspace, path); ok {
			found = append(found, f)
		}
	}

	col := Collection{}
	used := 0
	for i := range found {
		f := found[i]
		if len(f.Content) > MaxFileBytes {
			f.Content = truncate(f.Content, MaxFileBytes)
			f.Truncated = true
		}
		if remaining := MaxTotalBytes - used; len(f.Content) > remaining {
			f.Content = truncate(f.Content, remaining)
			f.Truncated = true
		}
		if len(f.Content) == 0 {
			col.Dropped++
			continue
		}
		used += len(f.Content)
		col.Files = append(col.Files, f)
	}
	return col
}

// readClaudeMD reads one CLAUDE.md and reports whether it yielded a usable
// file. A missing file, a directory that happens to be named CLAUDE.md, and
// an unreadable file all yield nothing.
func readClaudeMD(workspace, path string) (File, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return File{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, false
	}
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return File{}, false
	}
	return File{Path: filepath.ToSlash(rel), Content: string(data)}, true
}

// truncate cuts s to at most max bytes without splitting a UTF-8 rune.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// Render returns the block for the opening user message, or the empty string
// when nothing was found. An empty result leaves the opening message
// byte-identical to a run with no CLAUDE.md files.
func (c Collection) Render() string {
	if len(c.Files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The repositories cloned into this workspace can carry a CLAUDE.md: the\n")
	b.WriteString("instructions of the repository being worked in, written by whoever\n")
	b.WriteString("maintains it. Follow them while working there.\n\n")
	for _, f := range c.Files {
		b.WriteString("### ")
		b.WriteString(f.Path)
		b.WriteString("\n\n")
		b.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			b.WriteString("\n")
		}
		if f.Truncated {
			b.WriteString("\nThis CLAUDE.md was truncated at ")
			b.WriteString(strconv.Itoa(len(f.Content)))
			b.WriteString(" bytes to keep the block within budget. Read ")
			b.WriteString(f.Path)
			b.WriteString(" in full with the Read tool.\n")
		}
		b.WriteString("\n")
	}
	if c.Dropped > 0 {
		b.WriteString("\n")
		b.WriteString(dropNotice(c.Dropped))
		b.WriteString("\n")
	}
	return b.String()
}

func dropNotice(n int) string {
	plural := "s"
	if n == 1 {
		plural = ""
	}
	return "This workspace holds " + strconv.Itoa(n) + " further CLAUDE.md file" + plural +
		" not shown here. Use Glob for CLAUDE.md if you need one."
}
