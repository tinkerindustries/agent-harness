package evals_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The judge speaks to a model provider through the narrow Client seam
// declared in this package (judge.go, internal/CLAUDE.md,
// docs/KIMI-INTEGRATION.md §4.1) and implemented by the provider packages.
// internal/deepseek must never be a direct dependency of the evals package:
// its behaviours belong behind the seam, and cmd/harness resolves which
// implementation a judge model gets through internal/provider
// (docs/KIMI-INTEGRATION.md §4.3). A direct import would be obvious in
// review, but a transitive one is not, so the check is on the closure rather
// than the import block, exactly like internal/session/boundary_test.go.
func TestEvalsDoesNotDependOnTheDeepSeekClient(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	forbidden := map[string]string{
		"github.com/mrgeoffrich/agent-harness/internal/deepseek": "the DeepSeek client",
	}
	for _, dep := range strings.Fields(string(out)) {
		if what, ok := forbidden[dep]; ok {
			t.Errorf("internal/evals depends on %s (%s); route it through the Client seam instead", dep, what)
		}
	}
}
