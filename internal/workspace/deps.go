package workspace

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A fresh clone has no node_modules, so the project-local binaries a
// repository's own scripts call are absent: `npm run build` dies on
// `tsc: not found` before the session has done anything. Installing after the
// clone removes that, and it is best-effort — a failure here leaves the
// session exactly where it would have been, free to install the dependencies
// itself and see the real error.

// nodeInstallTimeout bounds one install so a large dependency tree cannot eat
// the run's deadline.
const nodeInstallTimeout = 5 * time.Minute

// maxProjectDepth is how far below a repository root a project is looked for.
// This repository keeps its frontend in web/ with no package.json at the root,
// so a root-only search finds nothing.
const maxProjectDepth = 2

// nodeInstallCommands maps a lockfile to the command that honours it, most
// specific first. Running the wrong manager writes a second lockfile into the
// working tree, which would then show up in the session's diff, so the
// lockfile chooses and there is no global preference. package.json is the
// fallback for a project that has committed no lockfile at all.
var nodeInstallCommands = []struct {
	marker  string
	command []string
}{
	{"pnpm-lock.yaml", []string{"pnpm", "install", "--frozen-lockfile"}},
	{"yarn.lock", []string{"yarn", "install"}},
	{"package-lock.json", []string{"npm", "ci"}},
	{"package.json", []string{"npm", "install"}},
}

// skippedDirs are never descended into when looking for a project: build
// output and vendored trees carry package.json files that belong to something
// else.
var skippedDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"third_party":  true,
	"testdata":     true,
	"dist":         true,
	"build":        true,
}

// install is one package manager run: the project directory and the command
// its lockfile chose.
type install struct {
	dir     string
	command []string
}

// installDependencies installs the dependencies of every Node project in a
// freshly cloned repository. It reports nothing: every outcome is logged, and
// no outcome fails the run.
func installDependencies(ctx context.Context, repoRoot string) {
	for _, in := range pendingInstalls(repoRoot) {
		runInstall(ctx, in.dir, in.command)
	}
}

// pendingInstalls is the work installDependencies will do: every Node project
// in the repository that has no node_modules yet, paired with the command to
// populate it.
func pendingInstalls(repoRoot string) []install {
	var pending []install
	for _, dir := range nodeProjectDirs(repoRoot) {
		if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
			continue
		}
		command, ok := nodeInstallCommand(dir)
		if !ok {
			continue
		}
		pending = append(pending, install{dir: dir, command: command})
	}
	return pending
}

// nodeProjectDirs finds the Node projects in a repository. A directory holding
// a package.json is a project and is not descended into: a workspace root
// installs its own members, and descending would run a second install inside
// the tree the first one just populated.
func nodeProjectDirs(root string) []string {
	var found []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			found = append(found, dir)
			return
		}
		if depth == maxProjectDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || skippedDirs[name] {
				continue
			}
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(root, 0)
	return found
}

// nodeInstallCommand picks the install command for one project directory from
// the lockfile it carries.
func nodeInstallCommand(dir string) ([]string, bool) {
	for _, candidate := range nodeInstallCommands {
		if _, err := os.Stat(filepath.Join(dir, candidate.marker)); err == nil {
			return candidate.command, true
		}
	}
	return nil, false
}

func runInstall(ctx context.Context, dir string, command []string) {
	ctx, cancel := context.WithTimeout(ctx, nodeInstallTimeout)
	defer cancel()

	started := time.Now()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	// npm writes progress bars and update notices to a terminal that is not
	// there, and CI=1 is what every one of these managers reads to mean
	// non-interactive.
	cmd.Env = append(os.Environ(), "CI=1", "NPM_CONFIG_FUND=false", "NPM_CONFIG_AUDIT=false")

	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("workspace: %s in %s: %v: %s",
			strings.Join(command, " "), dir, err, tail(string(out), 500))
		return
	}
	log.Printf("workspace: %s in %s: ok in %s", strings.Join(command, " "), dir, time.Since(started).Round(time.Millisecond))
}

// tail keeps the end of a command's output, which is where a package manager
// puts the reason it failed.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
