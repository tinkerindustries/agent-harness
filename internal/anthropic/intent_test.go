package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func decodeRequest(t *testing.T, req MessagesRequest) map[string]any {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return out
}

func TestRequestFromIntentSystemPromptAndUserMessage(t *testing.T) {
	intent := wire.ChatIntent{
		Model: ModelSonnet55,
		Items: []wire.Item{
			wire.SystemItem("You are a coding agent."),
			wire.UserItem("List the files."),
		},
		Effort:    "high",
		MaxTokens: 4096,
	}
	req := requestFromIntent(intent)

	if req.Model != ModelSonnet55 {
		t.Errorf("Model = %q, want %q", req.Model, ModelSonnet55)
	}
	if len(req.System) != 1 || req.System[0].Text != "You are a coding agent." {
		t.Fatalf("System = %+v", req.System)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("Messages = %+v, want 1 entry", req.Messages)
	}
	if req.Messages[0].Role != wire.RoleUser {
		t.Errorf("Messages[0].Role = %q", req.Messages[0].Role)
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "List the files." {
		t.Fatalf("blocks = %+v", blocks)
	}
	// Effort default "high" is sent explicitly since intent.Effort is
	// non-empty; requestFromIntent does not special-case it away.
	if req.OutputConfig == nil || req.OutputConfig.Effort != "high" {
		t.Errorf("OutputConfig = %+v", req.OutputConfig)
	}
	// Adaptive thinking is always sent, unconditionally.
	if req.Thinking == nil || req.Thinking.Type != "adaptive" || req.Thinking.Display != "summarized" {
		t.Fatalf("Thinking = %+v", req.Thinking)
	}
	if req.Thinking.BlockBinding == nil || req.Thinking.BlockBinding.PrefixMismatchBehavior != "error" {
		t.Fatalf("BlockBinding = %+v", req.Thinking.BlockBinding)
	}
}

func TestRequestFromIntentToolUseAndResult(t *testing.T) {
	intent := wire.ChatIntent{
		Model: ModelOpus55,
		Items: []wire.Item{
			wire.SystemItem("system"),
			wire.UserItem("run ls"),
			wire.ReasoningItem(""),
			wire.FunctionCallItem("call_1", "Bash", `{"command":"ls"}`),
			wire.FunctionCallOutputItem("call_1", "file.go", ""),
		},
		MaxTokens: 1024,
	}
	req := requestFromIntent(intent)

	if len(req.Messages) != 3 {
		t.Fatalf("Messages = %d entries, want 3 (user, assistant tool_use, user tool_result): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != wire.RoleAssistant {
		t.Fatalf("Messages[1].Role = %q", req.Messages[1].Role)
	}
	var assistantBlocks []ContentBlock
	if err := json.Unmarshal(req.Messages[1].Content, &assistantBlocks); err != nil {
		t.Fatalf("decode assistant content: %v", err)
	}
	if len(assistantBlocks) != 1 || assistantBlocks[0].Type != "tool_use" || assistantBlocks[0].ID != "call_1" || assistantBlocks[0].Name != "Bash" {
		t.Fatalf("assistant blocks = %+v", assistantBlocks)
	}
	if string(assistantBlocks[0].Input) != `{"command":"ls"}` {
		t.Errorf("Input = %s", assistantBlocks[0].Input)
	}

	if req.Messages[2].Role != wire.RoleUser {
		t.Fatalf("Messages[2].Role = %q", req.Messages[2].Role)
	}
	var toolResultBlocks []ContentBlock
	if err := json.Unmarshal(req.Messages[2].Content, &toolResultBlocks); err != nil {
		t.Fatalf("decode tool result content: %v", err)
	}
	if len(toolResultBlocks) != 1 || toolResultBlocks[0].Type != "tool_result" || toolResultBlocks[0].ToolUseID != "call_1" {
		t.Fatalf("tool result blocks = %+v", toolResultBlocks)
	}
	if len(toolResultBlocks[0].Content) != 1 || toolResultBlocks[0].Content[0].Text != "file.go" {
		t.Fatalf("tool result content = %+v", toolResultBlocks[0].Content)
	}
}

func TestRequestFromIntentMultipleToolResultsCollapseIntoOneMessage(t *testing.T) {
	intent := wire.ChatIntent{
		Model: ModelOpus55,
		Items: []wire.Item{
			wire.SystemItem("system"),
			wire.UserItem("do two things"),
			wire.ReasoningItem(""),
			wire.FunctionCallItem("call_1", "Read", `{"path":"a"}`),
			wire.FunctionCallItem("call_2", "Read", `{"path":"b"}`),
			wire.FunctionCallOutputItem("call_1", "contents a", ""),
			wire.FunctionCallOutputItem("call_2", "contents b", ""),
		},
		MaxTokens: 1024,
	}
	req := requestFromIntent(intent)

	if len(req.Messages) != 3 {
		t.Fatalf("Messages = %d entries, want 3: %+v", len(req.Messages), req.Messages)
	}
	var toolResultBlocks []ContentBlock
	if err := json.Unmarshal(req.Messages[2].Content, &toolResultBlocks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(toolResultBlocks) != 2 {
		t.Fatalf("expected both tool results in one user message, got %d blocks", len(toolResultBlocks))
	}
	if toolResultBlocks[0].ToolUseID != "call_1" || toolResultBlocks[1].ToolUseID != "call_2" {
		t.Fatalf("tool result order wrong: %+v", toolResultBlocks)
	}
}

func TestRequestFromIntentRawBlocksReplayedVerbatim(t *testing.T) {
	raw := json.RawMessage(`[{"type":"thinking","thinking":"let me think","signature":"sig123"},{"type":"tool_use","id":"call_9","name":"Bash","input":{"command":"pwd"}}]`)
	reasoningItem := wire.ReasoningItem("let me think")
	reasoningItem.ProviderBlocks = raw

	intent := wire.ChatIntent{
		Model: ModelOpus55,
		Items: []wire.Item{
			wire.SystemItem("system"),
			wire.UserItem("where am I"),
			reasoningItem,
			// fold.go still emits the text/function_call items for the
			// same sub-turn; the raw-blocks path must swallow them.
			wire.FunctionCallItem("call_9", "Bash", `{"command":"pwd"}`),
			wire.FunctionCallOutputItem("call_9", "/home", ""),
		},
		MaxTokens: 1024,
	}
	req := requestFromIntent(intent)

	if len(req.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3 (user, assistant raw, user tool_result): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != wire.RoleAssistant {
		t.Fatalf("Messages[1].Role = %q", req.Messages[1].Role)
	}
	if string(req.Messages[1].Content) != string(raw) {
		t.Errorf("assistant content = %s, want raw blocks verbatim %s", req.Messages[1].Content, raw)
	}
}

func TestSystemMessageForModelDifference(t *testing.T) {
	opus := systemMessageFor(ModelOpus55, "reminder text")
	if opus.role != wire.RoleSystem {
		t.Errorf("Opus 5.5: role = %q, want system", opus.role)
	}
	sonnet := systemMessageFor(ModelSonnet55, "reminder text")
	if sonnet.role != wire.RoleSystem {
		t.Errorf("Sonnet 5.5: role = %q, want system", sonnet.role)
	}
	fable := systemMessageFor(ModelFable51, "reminder text")
	if fable.role != wire.RoleSystem {
		t.Errorf("Fable 5.1: role = %q, want system", fable.role)
	}
	unknown := systemMessageFor("claude-haiku-4-5", "reminder text")
	if unknown.role != wire.RoleUser {
		t.Errorf("unknown model: role = %q, want user", unknown.role)
	}
}

func TestToolsFromWireAppendsServerToolsInFixedOrder(t *testing.T) {
	tools := []wire.Tool{
		{Function: wire.ToolFunction{Name: "Read", Description: "reads a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	out := toolsFromWire(tools)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	td, ok := out[0].(ToolDefinition)
	if !ok || td.Name != "Read" {
		t.Fatalf("out[0] = %#v", out[0])
	}
	ws, ok := out[1].(ServerToolDefinition)
	if !ok || ws.Type != ToolTypeWebSearch {
		t.Fatalf("out[1] = %#v", out[1])
	}
	wf, ok := out[2].(ServerToolDefinition)
	if !ok || wf.Type != ToolTypeWebFetch {
		t.Fatalf("out[2] = %#v", out[2])
	}
}

func TestApplyCacheBreakpoints(t *testing.T) {
	intent := wire.ChatIntent{
		Model: ModelOpus55,
		Items: []wire.Item{
			wire.SystemItem("system"),
			wire.UserItem("hello"),
		},
		MaxTokens: 1024,
		Tools: []wire.Tool{
			{Function: wire.ToolFunction{Name: "Read"}},
		},
	}
	req := requestFromIntent(intent)

	full := decodeRequest(t, req)
	systemArr := full["system"].([]any)
	lastSystem := systemArr[len(systemArr)-1].(map[string]any)
	if _, ok := lastSystem["cache_control"]; !ok {
		t.Error("system block has no cache_control")
	}

	toolsArr := full["tools"].([]any)
	firstTool := toolsArr[0].(map[string]any)
	if _, ok := firstTool["cache_control"]; !ok {
		t.Error("last client-declared tool has no cache_control")
	}

	msgs := full["messages"].([]any)
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	content := lastMsg["content"].([]any)
	lastBlock := content[len(content)-1].(map[string]any)
	if _, ok := lastBlock["cache_control"]; !ok {
		t.Error("last block of last message has no cache_control")
	}
}

func TestRequestFromIntentByteStability(t *testing.T) {
	intent := wire.ChatIntent{
		Model: ModelSonnet55,
		Items: []wire.Item{
			wire.SystemItem("system prompt"),
			wire.UserItem("do the thing"),
		},
		Effort:    "medium",
		MaxTokens: 8192,
		Tools: []wire.Tool{
			{Function: wire.ToolFunction{Name: "Read", Parameters: json.RawMessage(`{"type":"object"}`)}},
		},
	}
	a, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}
	b, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if !bytesEqual(a, b) {
		t.Errorf("two renders of the same intent produced different bytes:\n%s\n%s", a, b)
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func TestDecodeDataURI(t *testing.T) {
	mime, data, ok := decodeDataURI("data:image/png;base64,QUJD")
	if !ok || mime != "image/png" || data != "QUJD" {
		t.Fatalf("decodeDataURI = %q, %q, %v", mime, data, ok)
	}
	if _, _, ok := decodeDataURI("not-a-data-uri"); ok {
		t.Error("expected ok=false for a non-data URI")
	}
}

func TestContentBlocksFromItemWithImage(t *testing.T) {
	content := &wire.ItemContent{Parts: []wire.ItemPart{
		{Type: wire.PartInputText, Text: "a screenshot"},
		{Type: wire.PartInputImage, ImageURL: "data:image/png;base64,QUJD"},
	}}
	blocks := contentBlocksFromItem(content)
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "a screenshot" {
		t.Errorf("blocks[0] = %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != "QUJD" {
		t.Errorf("blocks[1] = %+v", blocks[1])
	}
}

func TestArgumentsObjectRoundTrip(t *testing.T) {
	if got := argumentsToObject(""); string(got) != "{}" {
		t.Errorf("argumentsToObject(\"\") = %s", got)
	}
	if got := argumentsFromObject(nil); got != "{}" {
		t.Errorf("argumentsFromObject(nil) = %s", got)
	}
	obj := json.RawMessage(`{"a":1}`)
	if got := argumentsFromObject(obj); got != `{"a":1}` {
		t.Errorf("argumentsFromObject(obj) = %s", got)
	}
}

func TestOutputConfigOmittedWhenEffortEmpty(t *testing.T) {
	intent := wire.ChatIntent{
		Model:     ModelOpus55,
		Items:     []wire.Item{wire.UserItem("hi")},
		MaxTokens: 100,
	}
	req := requestFromIntent(intent)
	if req.OutputConfig != nil {
		t.Errorf("OutputConfig = %+v, want nil when intent.Effort is empty", req.OutputConfig)
	}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "output_config") {
		t.Errorf("request body contains output_config: %s", b)
	}
}
