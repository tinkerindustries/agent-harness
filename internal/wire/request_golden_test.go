package wire_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// goldenRequest builds the fully-populated chat completion request whose
// serialised bytes are pinned by TestRequestBodyGolden. It exercises every
// shape the harness really sends: a system message, a user message, an
// assistant message carrying reasoning_content and tool_calls, a tool-result
// message with tool_call_id, an assistant message whose tool_calls ride on
// empty-string content (the case that must serialise as "" rather than being
// absent or null, docs/DESIGN.md §4.4), two entries from the real frozen
// tool array, and every top-level option the request path sets. Anything
// absent here is unprotected by the golden file.
func goldenRequest(t *testing.T) wire.ChatCompletionRequest {
	t.Helper()
	toolDefs := tools.Definitions()
	if len(toolDefs) < 2 {
		t.Fatalf("tools.Definitions() has %d entries, want at least 2 for the golden request", len(toolDefs))
	}
	reasoning := "Let me look at the workspace before answering."
	return wire.ChatCompletionRequest{
		Model: "deepseek-v4-pro",
		Messages: []wire.Message{
			wire.SystemMessage("You are a helpful assistant."),
			wire.UserMessage("What is in this workspace?"),
			{
				Role:             wire.RoleAssistant,
				Content:          "I'll check the workspace.",
				ReasoningContent: &reasoning,
				ToolCalls: []wire.ToolCall{
					{ID: "call_01", Type: "function", Function: wire.ToolCallFunc{Name: "List", Arguments: `{"path":"."}`}},
				},
			},
			{
				Role:       wire.RoleTool,
				Content:    "README.md\nmain.go",
				ToolCallID: "call_01",
			},
			{
				Role:    wire.RoleAssistant,
				Content: "",
				ToolCalls: []wire.ToolCall{
					{ID: "call_02", Type: "function", Function: wire.ToolCallFunc{Name: "Grep", Arguments: `{"pattern":"TODO"}`}},
				},
			},
		},
		Thinking:        &wire.ThinkingConfig{Type: wire.ThinkingEnabled},
		ReasoningEffort: wire.EffortHigh,
		MaxTokens:       48000,
		Tools:           []wire.Tool{toolDefs[0], toolDefs[1]},
		Stream:          true,
		StreamOptions:   &wire.StreamOptions{IncludeUsage: true},
	}
}

// TestRequestBodyGolden pins the exact bytes of a fully-populated chat
// completion request. The head of every request is frozen and shared
// (docs/DESIGN.md §3.2), and the prompt cache depends on identical bytes, so
// the serialised shape is a contract: the golden file was captured from the
// pre-refactor code and must keep passing byte-for-byte as the wire
// vocabulary moves between packages (docs/KIMI-INTEGRATION.md §4.2).
func TestRequestBodyGolden(t *testing.T) {
	got, err := json.Marshal(goldenRequest(t))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "request_body.golden.json"))
	if err != nil {
		t.Fatalf("ReadFile golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("request body differs from golden file:\ngot:  %s\nwant: %s", got, want)
	}
}
