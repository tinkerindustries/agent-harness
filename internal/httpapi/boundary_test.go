package httpapi_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The HTTP server does not run sessions and does not pull work; it reads the
// store and asks two narrow seams to start and stop a run (CLAUDE.md,
// ARCHITECTURE.md). A direct import of either package would be obvious in
// review, but a transitive one is not: internal/queue briefly reached
// internal/session to validate a prompt variant, which put the whole agent
// loop inside this package's dependency set without a line of this package
// changing. The check is on the closure rather than the import block for
// exactly that reason.
func TestHTTPServerDoesNotDependOnTheAgentLoop(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	forbidden := map[string]string{
		"github.com/mrgeoffrich/agent-harness/internal/session": "the agent loop",
		"github.com/mrgeoffrich/agent-harness/internal/worker":  "the worker pool",
	}
	for _, dep := range strings.Fields(string(out)) {
		if what, ok := forbidden[dep]; ok {
			t.Errorf("internal/httpapi depends on %s (%s); route it through a seam instead", dep, what)
		}
	}
}
