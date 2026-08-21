package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPromptGolden pins each provider's rendered head against its committed
// golden file, byte for byte — the same guard the tool arrays have
// (internal/tools/definitions_golden_test.go). The head is the prompt
// cache's shared prefix (docs/CACHE.md): a moved byte costs every session
// on that provider a full cache miss, so the head is never changed by
// accident, and a refactor of the prompt builder is only done when this
// test still passes against the goldens captured from the previous build.
//
// The goldens are the choice, not the test: each case carries its own file,
// so a failing assertion names the provider whose head moved, and the two
// files can be regenerated independently.
//
// Gemini's case reads Kimi's golden file rather than a third one:
// renderSystemPromptFor is a pure function of a tool array's names and the
// seesImages capability (prompt.go), and Gemini and Kimi share both — the
// same vision-capable tool array (tools.definitionsVisionCapable) and
// seesImages() true for both (runner.go, docs/GEMINI-INTEGRATION.md §5.7)
// — so the rendered head is not just equal by chance but the same bytes for
// the same reason. There is no Gemini-specific wording in the head: nothing
// in it names a provider, only tool names and the one capability sentence.
func TestPromptGolden(t *testing.T) {
	cases := []struct {
		name   string
		model  string // a model the provider serves, exercising the resolution path
		golden string
	}{
		{"deepseek", "deepseek-v4-pro", "prompt_deepseek.golden.txt"},
		{"kimi", "kimi-k3", "prompt_kimi.golden.txt"},
		{"gemini", "gemini-3.7-flash", "prompt_kimi.golden.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderSystemPromptFor(tc.model, "")
			if err != nil {
				t.Fatalf("RenderSystemPromptFor(%q): %v", tc.model, err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("ReadFile golden: %v", err)
			}
			if got != string(want) {
				t.Fatalf("%s prompt differs from %s:\n--- got ---\n%s\n--- want ---\n%s",
					tc.name, tc.golden, got, want)
			}
		})
	}
}
