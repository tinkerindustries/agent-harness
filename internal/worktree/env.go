package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	managedBlockStart = "# --- harness worktree env: managed by `harness worktree init`; edits below are overwritten ---"
	managedBlockEnd   = "# --- end harness worktree env ---"
)

// managedKeys are every key the block above sets. Any line defining one of
// these outside the block — most commonly a leftover from the main
// checkout's own .env, which init copies in wholesale — is stripped rather
// than left to shadow the managed value: dotenv parsers disagree on which
// duplicate wins (config.LoadDotEnv keeps the *first*; docker compose's own
// parser keeps the *last*), so two conflicting definitions is a bug either
// way, not a matter of ordering them correctly.
//
// The five NATS keys stay listed for this release even though the managed
// block no longer sets them: stripManagedKeys is what removes them from an
// existing worktree's .env on the next `harness worktree init`, so leaving
// them listed is how stale definitions get cleaned up rather than lingering
// as dead lines that shadow nothing. Drop them from this list a release
// later, once no worktree predating the removal is still in use
// (docs/QUEUE-MIGRATION-PLAN.md §8).
var managedKeys = []string{
	"COMPOSE_PROJECT_NAME",
	"NATS_CLIENT_PORT", "NATS_MONITOR_PORT", "NATS_URL",
	"HARNESS_HTTP_PORT",
	"HARNESS_TEST_NATS_PORT", "HARNESS_TEST_NATS_URL",
	"DEEPSEEK_HTTP_ADDR",
	"DEEPSEEK_HARNESS_BASE_URL", "DEEPSEEK_HARNESS_PUBLIC_URL",
	"DEEPSEEK_WORKSPACE_ROOT", "HARNESS_WORKSPACES", "HARNESS_VITE_PORT",
}

// stripManagedKeys drops every line of content that assigns one of
// managedKeys, leaving comments and everything else untouched. Used on both
// the seed content (a copy of the main checkout's .env) and the material
// outside the markers in an existing worktree .env, so the managed block is
// always the single definition of each of these keys.
func stripManagedKeys(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && slices.Contains(managedKeys, key) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// managedBlock renders the .env lines this package owns for one worktree's
// allocation. Every value here is also on the descriptor; this is the form
// docker compose and the host-mode binaries actually read (config.go's
// LoadDotEnv, docker-compose.yml's ${VAR:-default} substitutions).
func managedBlock(d Descriptor) string {
	var b strings.Builder
	fmt.Fprintln(&b, managedBlockStart)
	fmt.Fprintf(&b, "COMPOSE_PROJECT_NAME=%s\n", d.Compose.DevProjectName)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "HARNESS_HTTP_PORT=%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# For `harness serve` run directly on the host (not through")
	fmt.Fprintln(&b, "# docker compose) from inside this worktree. serve also mounts /mcp")
	fmt.Fprintln(&b, "# on this same address.")
	fmt.Fprintf(&b, "DEEPSEEK_HTTP_ADDR=127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_HARNESS_BASE_URL=http://127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_HARNESS_PUBLIC_URL=http://127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_WORKSPACE_ROOT=%s\n", filepath.Join(d.Identity.Path, "workspaces"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# What docker compose mounts the workspace root at — on both sides of the")
	fmt.Fprintln(&b, "# mount, so a session's paths mean the same thing to the host's docker")
	fmt.Fprintln(&b, "# daemon as they do inside the container (docs/WORKTREES.md, \"Path")
	fmt.Fprintln(&b, "# parity\"). The same directory as DEEPSEEK_WORKSPACE_ROOT above, which is")
	fmt.Fprintln(&b, "# the point: with parity there is only one path to a workspace. Pinned")
	fmt.Fprintln(&b, "# absolutely because the compose default is ${PWD}, the shell's directory.")
	fmt.Fprintf(&b, "HARNESS_WORKSPACES=%s\n", filepath.Join(d.Identity.Path, "workspaces"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# web/vite.config.ts reads this for its own dev server port and to build")
	fmt.Fprintln(&b, "# its /api proxy target from HARNESS_HTTP_PORT above.")
	fmt.Fprintf(&b, "HARNESS_VITE_PORT=%d\n", d.Ports.Vite)
	fmt.Fprint(&b, managedBlockEnd)
	return b.String()
}

// UpsertEnv writes or updates worktreeRoot/.env with d's managed block.
// A first run seeds the file from seedFrom (the main checkout's .env, so
// GITHUB_TOKEN and any DeepSeek credentials carry over) and appends the
// block; a re-run replaces only the text between the markers, leaving
// anything else in the file — including hand edits — untouched.
func UpsertEnv(worktreeRoot string, d Descriptor, seedFrom string) error {
	envPath := filepath.Join(worktreeRoot, ".env")
	block := managedBlock(d)

	existing, err := os.ReadFile(envPath)
	if os.IsNotExist(err) {
		seed, seedErr := os.ReadFile(seedFrom)
		if seedErr != nil && !os.IsNotExist(seedErr) {
			return fmt.Errorf("read %s to seed %s: %w", seedFrom, envPath, seedErr)
		}
		seedContent := strings.TrimRight(stripManagedKeys(string(seed)), "\n")
		var b strings.Builder
		if seedContent != "" {
			b.WriteString(seedContent)
			b.WriteString("\n\n")
		}
		b.WriteString(block)
		b.WriteByte('\n')
		return writeFileAtomic(envPath, []byte(b.String()), 0o600)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", envPath, err)
	}

	content := string(existing)
	startIdx := strings.Index(content, managedBlockStart)
	endIdx := strings.Index(content, managedBlockEnd)
	if startIdx == -1 || endIdx == -1 || endIdx < startIdx {
		// No managed block yet (a hand-written .env, or one predating this
		// tool): append rather than guessing where to splice.
		rest := strings.TrimRight(stripManagedKeys(content), "\n")
		var b strings.Builder
		if rest != "" {
			b.WriteString(rest)
			b.WriteString("\n\n")
		}
		b.WriteString(block)
		b.WriteByte('\n')
		return writeFileAtomic(envPath, []byte(b.String()), 0o600)
	}

	before := strings.TrimRight(stripManagedKeys(content[:startIdx]), "\n")
	after := strings.TrimLeft(stripManagedKeys(content[endIdx+len(managedBlockEnd):]), "\n")
	var b strings.Builder
	if before != "" {
		b.WriteString(before)
		b.WriteString("\n\n")
	}
	b.WriteString(block)
	if after != "" {
		b.WriteString("\n\n")
		b.WriteString(after)
	}
	b.WriteByte('\n')
	return writeFileAtomic(envPath, []byte(b.String()), 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file for %s: %w", path, err)
	}
	return nil
}
