package deepseek

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// The seam must not move a byte of the wire format: the loop used to build
// wire.ChatCompletionRequest values directly, and requestFromIntent now
// builds them from wire.ChatIntent behind the seam. These two tests pin that
// the translation produces exactly the request the loop used to construct,
// for the two shapes the loop really sends — a thinking sub-turn with tools,
// and a non-thinking compaction summary with neither effort nor tools. The
// golden request-body test in internal/wire pins the struct itself
// (docs/KIMI-INTEGRATION.md §4.2).
func TestRequestFromIntentMatchesDirectBuild(t *testing.T) {
	messages := []wire.Message{
		wire.SystemMessage("You are a helpful assistant."),
		wire.UserMessage("What is in this workspace?"),
	}
	tools := []wire.Tool{
		{Type: "function", Function: wire.ToolFunction{Name: "List", Description: "List a directory", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}

	intent := wire.ChatIntent{
		Model:     "deepseek-v4-pro",
		Messages:  messages,
		Effort:    wire.EffortHigh,
		Thinking:  true,
		MaxTokens: 48000,
		Tools:     tools,
	}
	got := requestFromIntent(intent)
	want := wire.ChatCompletionRequest{
		Model:           "deepseek-v4-pro",
		Messages:        messages,
		Thinking:        &wire.ThinkingConfig{Type: wire.ThinkingEnabled},
		ReasoningEffort: wire.EffortHigh,
		MaxTokens:       48000,
		Tools:           tools,
	}
	assertSameBytes(t, got, want)

	intent.Thinking = false
	got = requestFromIntent(intent)
	want.Thinking = &wire.ThinkingConfig{Type: wire.ThinkingDisabled}
	assertSameBytes(t, got, want)
}

// The compaction summary's shape: thinking disabled, no effort, no tools.
// These omitted fields must stay omitted — an empty reasoning_effort or an
// empty tools array on the wire is a byte change, and the cache prefix
// depends on the bytes (docs/DESIGN.md §3.2).
func TestRequestFromIntentCompactionShape(t *testing.T) {
	intent := wire.ChatIntent{
		Model:     "deepseek-v4-flash",
		Messages:  []wire.Message{wire.UserMessage("summarise")},
		Thinking:  false,
		MaxTokens: 8000,
	}
	got := requestFromIntent(intent)
	want := wire.ChatCompletionRequest{
		Model:     "deepseek-v4-flash",
		Messages:  []wire.Message{wire.UserMessage("summarise")},
		Thinking:  &wire.ThinkingConfig{Type: wire.ThinkingDisabled},
		MaxTokens: 8000,
	}
	assertSameBytes(t, got, want)
}

func assertSameBytes(t *testing.T, got, want wire.ChatCompletionRequest) {
	t.Helper()
	gotB, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal got: %v", err)
	}
	wantB, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal want: %v", err)
	}
	if string(gotB) != string(wantB) {
		t.Fatalf("requestFromIntent moved the wire bytes:\ngot:  %s\nwant: %s", gotB, wantB)
	}
}
