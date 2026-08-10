package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	managedBlockStart = "# --- harness worktree env: managed by `harness worktree init`; edits below are overwritten ---"
	managedBlockEnd   = "# --- end harness worktree env ---"
)

// managedBlock renders the .env lines this package owns for one worktree's
// allocation. Every value here is also on the descriptor; this is the form
// docker compose and the host-mode binaries actually read (config.go's
// LoadDotEnv, docker-compose.yml's ${VAR:-default} substitutions).
func managedBlock(d Descriptor) string {
	var b strings.Builder
	fmt.Fprintln(&b, managedBlockStart)
	fmt.Fprintf(&b, "COMPOSE_PROJECT_NAME=%s\n", d.Compose.DevProjectName)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "NATS_CLIENT_PORT=%d\n", d.Ports.NATSClient)
	fmt.Fprintf(&b, "NATS_MONITOR_PORT=%d\n", d.Ports.NATSMonitor)
	fmt.Fprintf(&b, "NATS_URL=nats://127.0.0.1:%d\n", d.Ports.NATSClient)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "HARNESS_HTTP_PORT=%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "HARNESS_MCP_PORT=%d\n", d.Ports.HarnessMCP)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "HARNESS_TEST_NATS_PORT=%d\n", d.Ports.TestNATS)
	fmt.Fprintf(&b, "HARNESS_TEST_NATS_URL=nats://127.0.0.1:%d\n", d.Ports.TestNATS)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# For `harness serve` / `harness mcp` run directly on the host (not through")
	fmt.Fprintln(&b, "# docker compose) from inside this worktree.")
	fmt.Fprintf(&b, "DEEPSEEK_HTTP_ADDR=127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_MCP_ADDR=127.0.0.1:%d\n", d.Ports.HarnessMCP)
	fmt.Fprintf(&b, "DEEPSEEK_HARNESS_BASE_URL=http://127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_HARNESS_PUBLIC_URL=http://127.0.0.1:%d\n", d.Ports.HarnessHTTP)
	fmt.Fprintf(&b, "DEEPSEEK_WORKSPACE_ROOT=%s\n", filepath.Join(d.Identity.Path, "workspaces"))
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
		var b strings.Builder
		if len(seed) > 0 {
			b.Write(seed)
			if !strings.HasSuffix(string(seed), "\n") {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
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
		var b strings.Builder
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		b.WriteString(block)
		b.WriteByte('\n')
		return writeFileAtomic(envPath, []byte(b.String()), 0o600)
	}

	newContent := content[:startIdx] + block + content[endIdx+len(managedBlockEnd):]
	return writeFileAtomic(envPath, []byte(newContent), 0o600)
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
