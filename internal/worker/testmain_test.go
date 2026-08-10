package worker

import (
	"os"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// TestMain isolates this package's broker tests from the other test
// packages that share the test NATS server. queue.IsolateForTest renames
// this package's streams and consumer, so its real pool can only ever see
// requests this package published — before that, it consumed internal/mcp's
// test requests off the shared WORK stream and broke mcp's launch-outcome
// assertions whenever the two packages ran together.
func TestMain(m *testing.M) {
	queue.IsolateForTest("worker")
	os.Exit(m.Run())
}
