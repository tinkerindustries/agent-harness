package wire

import (
	"encoding/json"
	"testing"
)

// TestRequestSerialisationIsByteStable checks that marshalling the same
// request value twice produces identical bytes. The prompt cache depends on
// retries sending an identical buffer (docs/DESIGN.md §3.2); this is what
// makes that possible given Go's stdlib json encoder.
func TestRequestSerialisationIsByteStable(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "deepseek-v4-pro",
		Messages: []Message{
			SystemMessage("You are a helpful assistant."),
			UserMessage("What is 2+2?"),
		},
		Thinking:        &ThinkingConfig{Type: ThinkingEnabled},
		ReasoningEffort: EffortHigh,
		MaxTokens:       48000,
	}

	a, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal #1: %v", err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal #2: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("serialisation not stable:\n#1: %s\n#2: %s", a, b)
	}
}

// TestAssistantToolCallContentIsEmptyStringNotNull checks the one request
// rule that is easy to get backwards: an assistant message carrying
// tool_calls must serialise content as "" rather than omitting the field or
// emitting null (docs/DESIGN.md §4.4).
func TestAssistantToolCallContentIsEmptyStringNotNull(t *testing.T) {
	msg := Message{
		Role:    RoleAssistant,
		Content: TextContent(""),
		ToolCalls: []ToolCall{
			{ID: "call_00_x", Type: "function", Function: ToolCallFunc{Name: "get_date", Arguments: "{}"}},
		},
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	content, ok := raw["content"]
	if !ok {
		t.Fatalf("content field is absent from %s, want present as \"\"", out)
	}
	if string(content) != `""` {
		t.Errorf("content = %s, want \"\" (empty string, not null)", content)
	}
}

func TestNoToolChoiceField(t *testing.T) {
	req := ChatCompletionRequest{
		Model:     "deepseek-v4-pro",
		Messages:  []Message{UserMessage("hi")},
		MaxTokens: 100,
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	if _, ok := raw["tool_choice"]; ok {
		t.Errorf("request body contains tool_choice, want it entirely absent: %s", out)
	}
}

func TestSystemRoleNotDeveloper(t *testing.T) {
	msg := SystemMessage("be helpful")
	if msg.Role != "system" {
		t.Errorf("SystemMessage role = %q, want %q", msg.Role, "system")
	}
}

// TestMessageWithoutThoughtSignatureUnchanged pins that a Message with no
// ThoughtSignature set serialises to exactly the bytes it did before that
// field existed — the byte-stability guard Phase 3 of
// docs/GEMINI-INTEGRATION.md requires alongside the DeepSeek and Kimi golden
// files, which this field must never disturb.
func TestMessageWithoutThoughtSignatureUnchanged(t *testing.T) {
	reasoning := "Let me check the workspace."
	msg := Message{
		Role:             RoleAssistant,
		Content:          TextContent("I'll check the workspace."),
		ReasoningContent: &reasoning,
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"role":"assistant","content":"I'll check the workspace.","reasoning_content":"Let me check the workspace."}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

// TestMessageWithThoughtSignature checks that a set signature serialises
// alongside reasoning content, verbatim and in the field's own position.
func TestMessageWithThoughtSignature(t *testing.T) {
	reasoning := "Let me check the workspace."
	signature := "EpoGCpcGAXLI2nx/"
	msg := Message{
		Role:             RoleAssistant,
		Content:          TextContent("I'll check the workspace."),
		ReasoningContent: &reasoning,
		ThoughtSignature: &signature,
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"role":"assistant","content":"I'll check the workspace.","reasoning_content":"Let me check the workspace.","thought_signature":"EpoGCpcGAXLI2nx/"}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

// TestMessageWithThoughtSignatureAndNoReasoning checks the case
// docs/OBSERVED.md says is routine: a signature is always present on a
// thought step, but its summary is often absent, so a message can carry a
// signature with no ReasoningContent at all. reasoning_content must stay
// absent rather than appearing as null or "".
func TestMessageWithThoughtSignatureAndNoReasoning(t *testing.T) {
	signature := "context_engineering_is_the_way_to_go"
	msg := Message{
		Role:             RoleAssistant,
		Content:          TextContent("Done."),
		ThoughtSignature: &signature,
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"role":"assistant","content":"Done.","thought_signature":"context_engineering_is_the_way_to_go"}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

func TestMaxTokensAlwaysSerialised(t *testing.T) {
	// max_tokens must be sent explicitly on every request, even when the
	// zero value would otherwise be omitted by an omitempty tag.
	req := ChatCompletionRequest{
		Model:    "deepseek-v4-pro",
		Messages: []Message{UserMessage("hi")},
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	if _, ok := raw["max_tokens"]; !ok {
		t.Errorf("request body has no max_tokens field: %s", out)
	}
}
