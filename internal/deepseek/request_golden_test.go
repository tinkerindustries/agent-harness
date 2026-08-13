package deepseek_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
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
func goldenRequest(t *testing.T) deepseek.ChatCompletionRequest {
	t.Helper()
	toolDefs := tools.Definitions()
	if len(toolDefs) < 2 {
		t.Fatalf("tools.Definitions() has %d entries, want at least 2 for the golden request", len(toolDefs))
	}
	reasoning := "Let me look at the workspace before answering."
	return deepseek.ChatCompletionRequest{
		Model: "deepseek-v4-pro",
		Messages: []deepseek.Message{
			deepseek.SystemMessage("You are a helpful assistant."),
			deepseek.UserMessage("What is in this workspace?"),
			{
				Role:             deepseek.RoleAssistant,
				Content:          "I'll check the workspace.",
				ReasoningContent: &reasoning,
				ToolCalls: []deepseek.ToolCall{
					{ID: "call_01", Type: "function", Function: deepseek.ToolCallFunc{Name: "List", Arguments: `{"path":"."}`}},
				},
			},
			{
				Role:       deepseek.RoleTool,
				Content:    "README.md\nmain.go",
				ToolCallID: "call_01",
			},
			{
				Role:    deepseek.RoleAssistant,
				Content: "",
				ToolCalls: []deepseek.ToolCall{
					{ID: "call_02", Type: "function", Function: deepseek.ToolCallFunc{Name: "Grep", Arguments: `{"pattern":"TODO"}`}},
				},
			},
		},
		Thinking:        &deepseek.ThinkingConfig{Type: deepseek.ThinkingEnabled},
		ReasoningEffort: deepseek.EffortHigh,
		MaxTokens:       48000,
		Tools:           []deepseek.Tool{toolDefs[0], toolDefs[1]},
		Stream:          true,
		StreamOptions:   &deepseek.StreamOptions{IncludeUsage: true},
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
