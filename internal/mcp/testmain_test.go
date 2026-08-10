package mcp

import (
	"os"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// TestMain isolates this package's broker tests from the other test
// packages that share the test NATS server. Until queue.IsolateForTest
// gave each package its own stream and consumer names, internal/worker's
// real pool consumed the requests these integration tests asserted were
// sitting unconsumed on the shared WORK stream, so `go test
// ./internal/mcp/... ./internal/worker/...` failed while either package
// alone passed.
func TestMain(m *testing.M) {
	queue.IsolateForTest("mcp")
	os.Exit(m.Run())
}
