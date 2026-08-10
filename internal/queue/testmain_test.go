package queue

import (
	"os"
	"testing"
)

// TestMain isolates this package's broker tests from every other test
// package running against the same NATS server. internal/mcp and
// internal/worker share that server, and until each package renamed its own
// streams and consumer (queue.IsolateForTest), `go test ./internal/mcp/...
// ./internal/worker/...` raced on the one WORK stream and the
// harness-workers durable. The prefix keeps this package's streams apart
// from theirs.
func TestMain(m *testing.M) {
	IsolateForTest("queue")
	os.Exit(m.Run())
}
