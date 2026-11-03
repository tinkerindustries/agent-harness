package guard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// RecentCommandCount is how many earlier bash commands State carries.
	RecentCommandCount = 3
	// RecentCommandRunes is where a recent command is cut.
	RecentCommandRunes = 200
	recentEllipsis     = "…"

	branchTimeout = 2 * time.Second
)

// StateFileSystem is FileSystem plus the modification time the origin of a
// script a bash command wrote is judged by.
type StateFileSystem interface {
	FileSystem
	// ModTime reports when the file at path was last modified. ok is false
	// when the file does not exist.
	ModTime(path string) (t time.Time, ok bool)
}

// ModTime is StateFileSystem.ModTime for the real disk.
func (OSFileSystem) ModTime(p string) (time.Time, bool) {
	info, err := os.Stat(p)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// Mutation is a file the session's own tools wrote successfully.
type Mutation struct {
	// Path is absolute and resolved the way the file tools resolve it.
	Path string
	// Created is true when the call made the file and false when it changed a
	// file that was already there.
	Created bool
}

// Inputs is everything State needs that is not on disk.
type Inputs struct {
	// Command is the bash call's command, unchanged.
	Command string
	// Workdir is the call's workdir argument, or empty.
	Workdir string
	// Directory is the session's harness.cwd.
	Directory string
	// Workspace is the root the file tools resolve paths against.
	Workspace string
	// Commands are the session's earlier bash commands, oldest first.
	Commands []string
	// Mutations are the writes with a successful result, in the order they
	// happened. A failed write is not in the list.
	Mutations []Mutation
	// Pending are the paths of writes that have no result yet: the other
	// calls of the tool round the bash call belongs to.
	Pending []string
	// SessionStart is when the session began. A file modified after it and
	// absent from Mutations was written by something other than the file tools.
	SessionStart time.Time
	// Repositories are the roots package script resolution falls back to. Nil
	// takes Repositories(Directory).
	Repositories []string
}

// StateBuilder turns Inputs and the disk into the State a bash call is checked
// with.
type StateBuilder struct {
	FS StateFileSystem
	// Branch names the branch checked out in dir. Nil runs git.
	Branch func(ctx context.Context, dir string) string
	// Fields names the state fields to fill in; the rest stay empty and cost
	// nothing, which matters for package_scripts, whose resolution reads
	// files. Nil fills them all. Bundle.StateFields is the set a bundle needs.
	Fields FieldSet
}

// Build fills the State for one bash call.
func (b StateBuilder) Build(ctx context.Context, in Inputs) State {
	fsys := b.FS
	if fsys == nil {
		fsys = OSFileSystem{}
	}
	branch := b.Branch
	if branch == nil {
		branch = GitBranch
	}
	effective := EffectiveDirectory(in.Directory, in.Workdir)
	repos := in.Repositories
	if repos == nil {
		repos = Repositories(in.Directory)
	}
	st := State{WorkingDirectory: in.Directory, ProposedCommand: in.Command}
	if b.Fields.wants(FieldBranch) {
		st.Branch = branch(ctx, in.Directory)
	}
	if b.Fields.wants(FieldRecentCommands) {
		st.RecentCommands = RecentCommands(in.Commands)
	}
	if b.Fields.wants(FieldScriptsRun) {
		st.ScriptsRun = b.scripts(fsys, in, effective)
	}
	if b.Fields.wants(FieldPackageScripts) {
		st.PackageScripts = ResolvePackageScripts(fsys, in.Command, effective, repos)
	}
	return st
}

func (b StateBuilder) scripts(fsys StateFileSystem, in Inputs, effective string) []ScriptEntry {
	var out []ScriptEntry
	for _, script := range ExecutedScripts(in.Command) {
		abs := absoluteScript(effective, script)
		origin, readable := scriptOrigin(fsys, in, abs)
		entry := ScriptEntry{Path: script, Origin: origin}
		if origin != OriginExisting && readable {
			if text, err := fsys.ReadText(abs); err == nil {
				entry.Content = &text
			}
		}
		out = append(out, entry)
	}
	return out
}

// scriptOrigin classifies the file at abs. readable is false when a write
// that has not landed yet may be changing it.
func scriptOrigin(fsys StateFileSystem, in Inputs, abs string) (origin string, readable bool) {
	real := resolveSymlinks(abs)
	same := func(p string) bool { return samePath(p, abs) || samePath(p, real) }
	for _, p := range in.Pending {
		if same(p) {
			if _, exists := fsys.ModTime(abs); exists {
				return OriginChangedThisSession, false
			}
			return OriginWrittenThisSession, false
		}
	}
	for _, m := range in.Mutations {
		if !same(m.Path) {
			continue
		}
		if origin == "" {
			origin = OriginChangedThisSession
			if m.Created {
				origin = OriginWrittenThisSession
			}
		}
	}
	if origin != "" {
		return origin, true
	}
	if t, ok := fsys.ModTime(abs); ok && !in.SessionStart.IsZero() && !t.Before(in.SessionStart) {
		return OriginChangedThisSession, true
	}
	return OriginExisting, true
}

// resolveSymlinks follows symlinks in the part of p that exists, the way the
// file tools resolve the paths MutationTargets reports.
func resolveSymlinks(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func absoluteScript(effective, script string) string {
	if filepath.IsAbs(script) {
		return filepath.Clean(script)
	}
	return filepath.Clean(filepath.Join(effective, script))
}

// EffectiveDirectory is the directory a bash call runs in: its workdir
// (relative to the session's directory) or else the session's directory.
func EffectiveDirectory(directory, workdir string) string {
	if workdir == "" {
		return directory
	}
	if filepath.IsAbs(workdir) {
		return filepath.Clean(workdir)
	}
	return filepath.Clean(filepath.Join(directory, workdir))
}

// Repositories are the roots package script resolution falls back to when a
// worktree has gone: the directory up to "/.claude/worktrees/", or the
// directory itself.
func Repositories(directory string) []string {
	if directory == "" {
		return nil
	}
	if i := strings.Index(directory, "/.claude/worktrees/"); i > 0 {
		return []string{directory[:i]}
	}
	return []string{directory}
}

// RecentCommands keeps the last RecentCommandCount commands, oldest first,
// each cut to RecentCommandRunes characters with an ellipsis.
func RecentCommands(commands []string) []string {
	if len(commands) > RecentCommandCount {
		commands = commands[len(commands)-RecentCommandCount:]
	}
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		if r := []rune(c); len(r) > RecentCommandRunes {
			c = string(r[:RecentCommandRunes]) + recentEllipsis
		}
		out = append(out, c)
	}
	return out
}

// GitBranch names the branch checked out in dir, or "" when dir is not in a
// repository, git cannot be run, or HEAD is detached.
func GitBranch(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--short", "-q", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
