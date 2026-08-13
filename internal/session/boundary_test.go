package session_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The agent loop speaks to a model provider through the narrow Client seam
// declared in this package and implemented by the provider packages
// (client.go, internal/CLAUDE.md, ARCHITECTURE.md,
// docs/KIMI-INTEGRATION.md §4.1). internal/deepseek must never be a direct
// dependency of the loop: its behaviours — how reasoning is requested, how
// usage maps onto cache hit and miss, the repair quirks — belong behind the
// seam. A direct import would be obvious in review, but a transitive one is
// not, so the check is on the closure rather than the import block, exactly
// like internal/httpapi/boundary_test.go.
func TestSessionDoesNotDependOnTheDeepSeekClient(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	forbidden := map[string]string{
		"github.com/mrgeoffrich/deepseek-harness/internal/deepseek": "the DeepSeek client",
	}
	for _, dep := range strings.Fields(string(out)) {
		if what, ok := forbidden[dep]; ok {
			t.Errorf("internal/session depends on %s (%s); route it through the Client seam instead", dep, what)
		}
	}
}
