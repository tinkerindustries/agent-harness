package guard

import (
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
)

// mustBundle loads the invented bundle with the edits applied.
func mustBundle(t *testing.T, edits ...func(map[string]any)) *Bundle {
	t.Helper()
	b, err := LoadBundle(guardtest.BundleJSON(edits...))
	if err != nil {
		t.Fatalf("invented bundle: %v", err)
	}
	return b
}

// section returns the nested object at the path, for an edit.
func section(b map[string]any, path ...string) map[string]any {
	cur := b
	for _, p := range path {
		cur = cur[p].(map[string]any)
	}
	return cur
}
