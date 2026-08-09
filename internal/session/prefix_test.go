package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// The frozen head of every session must be byte-identical regardless of what
// a work request asked for. Concurrency only makes the prompt cache cheaper
// while the head is genuinely shared, so anything per-request that reaches
// the system prompt or the tool schema costs the whole prefix
// (docs/CACHE.md, docs/DESIGN.md §3.2).
func TestFrozenPrefixIsIdenticalAcrossRequests(t *testing.T) {
	if got, want := RenderSystemPrompt(), RenderSystemPrompt(); got != want {
		t.Fatal("RenderSystemPrompt is not stable across calls")
	}

	first, err := json.Marshal(tools.Definitions())
	if err != nil {
		t.Fatalf("encode tool schema: %v", err)
	}
	for i := 0; i < 8; i++ {
		next, err := json.Marshal(tools.Definitions())
		if err != nil {
			t.Fatalf("encode tool schema: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("tool schema is not byte-stable across calls:\n first: %s\n  next: %s", first, next)
		}
	}
}

// Everything specific to a run belongs in the opening user message, where it
// appends rather than divides the prefix. This asserts the split holds: the
// workspace, the task, and the result schema all appear there and none of
// them appear in the system prompt.
func TestPerRequestDataStaysOutOfTheSystemPrompt(t *testing.T) {
	const (
		workspace = "/tmp/some-unlikely-workspace-path-42"
		task      = "an unlikely task string 42"
	)
	schema := json.RawMessage(`{"type":"object","properties":{"unlikelyField42":{"type":"string"}}}`)

	sys := RenderSystemPrompt()
	opening := RenderOpeningMessage(workspace, task, schema, "", "")

	for _, needle := range []string{workspace, task, "unlikelyField42"} {
		if !strings.Contains(opening, needle) {
			t.Errorf("opening message should carry %q, it does not", needle)
		}
		if strings.Contains(sys, needle) {
			t.Errorf("system prompt carries per-request value %q; it must stay in the opening message", needle)
		}
	}
}
