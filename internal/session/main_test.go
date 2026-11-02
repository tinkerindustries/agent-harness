package session

import (
	"os"
	"testing"
)

// TestMain keeps the developer's own Claude installation out of the session
// tests. Skills discovery reads CLAUDE_CONFIG_DIR, and a machine with
// personal or plugin skills would otherwise change every opening message
// these tests pin.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-harness-session-test")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CLAUDE_CONFIG_DIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
