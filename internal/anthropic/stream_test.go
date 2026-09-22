package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// sseBody turns a slice of "event: X\ndata: Y" pairs into one SSE body,
// blank-line delimited, matching the shape
// docs/ANTHROPIC-INTEGRATION.md's fixtures are captured in.
func sseBody(frames ...string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Join(frames, "\n\n") + "\n\n"))
}

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data
}

func collectEvents(t *testing.T, c *Client, body io.ReadCloser) ([]wire.Event, streamResult, error) {
	t.Helper()
	var got []wire.Event
	send := func(e wire.Event) bool {
		got = append(got, e)
		return true
	}
	result, err := c.readSSE(context.Background(), body, send)
	return got, result, err
}

func TestReadSSETextResponse(t *testing.T) {
	c := NewClient("")
	body := sseBody(
		frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}`),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	)

	events, result, err := collectEvents(t, c, body)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	var text strings.Builder
	for _, e := range events {
		if e.Type == wire.EventContentDelta {
			text.WriteString(e.Content)
		}
	}
	if text.String() != "Hello!" {
		t.Errorf("accumulated text = %q", text.String())
	}
	if result.stopReason != "end_turn" {
		t.Errorf("stopReason = %q", result.stopReason)
	}
	if len(result.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(result.blocks))
	}
	var block rawContentBlock
	if err := json.Unmarshal(result.blocks[0], &block); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if block.Type != "text" || block.Text != "Hello!" {
		t.Errorf("block = %+v", block)
	}
	if result.usage == nil || result.usage.PromptTokens != 25 || result.usage.CompletionTokens != 15 {
		t.Errorf("usage = %+v", result.usage)
	}
}

func TestReadSSEToolUse(t *testing.T) {
	c := NewClient("")
	body := sseBody(
		frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":472,"output_tokens":2}}}`),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" \"SF\"}"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	)

	events, result, err := collectEvents(t, c, body)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	assembler := wire.NewToolCallAssembler()
	for _, e := range events {
		if e.Type == wire.EventToolCallDelta {
			assembler.Add(e.ToolCall)
		}
	}
	calls := assembler.Finalize()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "get_weather" {
		t.Errorf("call = %+v", calls[0])
	}
	if calls[0].Arguments != `{"location": "SF"}` {
		t.Errorf("Arguments = %q", calls[0].Arguments)
	}
	if result.stopReason != "tool_use" {
		t.Errorf("stopReason = %q", result.stopReason)
	}
	var block rawContentBlock
	if err := json.Unmarshal(result.blocks[0], &block); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if block.Type != "tool_use" || block.ID != "toolu_1" || block.Name != "get_weather" {
		t.Errorf("block = %+v", block)
	}
	// json.Marshal compacts a json.RawMessage's insignificant whitespace,
	// so the raw block's Input loses the leading space the accumulated
	// partial_json carried, unlike calls[0].Arguments above (never
	// re-marshalled).
	if string(block.Input) != `{"location":"SF"}` {
		t.Errorf("Input = %s", block.Input)
	}
}

func TestReadSSEThinkingAndSignature(t *testing.T) {
	c := NewClient("")
	body := sseBody(
		frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first "}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"second"}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":20}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	)

	events, result, err := collectEvents(t, c, body)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	var reasoning strings.Builder
	for _, e := range events {
		if e.Type == wire.EventReasoningDelta {
			reasoning.WriteString(e.Reasoning)
		}
	}
	if reasoning.String() != "first second" {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if len(result.blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(result.blocks))
	}
	var thinking rawContentBlock
	if err := json.Unmarshal(result.blocks[0], &thinking); err != nil {
		t.Fatalf("decode thinking block: %v", err)
	}
	if thinking.Type != "thinking" || thinking.Thinking != "first second" || thinking.Signature != "sig-abc" {
		t.Errorf("thinking block = %+v", thinking)
	}
}

func TestReadSSEUnknownBlockKeptVerbatim(t *testing.T) {
	c := NewClient("")
	toolResultRaw := `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"x","url":"https://example.com"}]}`
	body := sseBody(
		frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+toolResultRaw+`}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	)
	_, result, err := collectEvents(t, c, body)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(result.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(result.blocks))
	}
	if string(result.blocks[0]) != toolResultRaw {
		t.Errorf("block = %s, want verbatim %s", result.blocks[0], toolResultRaw)
	}
}

func TestReadSSEErrorEvent(t *testing.T) {
	c := NewClient("")
	body := sseBody(
		frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
		frame("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	)
	_, _, err := collectEvents(t, c, body)
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := IsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Type != "overloaded_error" {
		t.Errorf("Type = %q", apiErr.Type)
	}
}

func TestMergeUsagePrefersDeltaOutputTokensAndStartInputTokens(t *testing.T) {
	start := &messagesUsage{InputTokens: 100, CacheReadInputTokens: 50, OutputTokens: 1}
	delta := &messagesUsage{OutputTokens: 250}
	got := mergeUsage(start, delta)
	if got.PromptTokens != 150 {
		t.Errorf("PromptTokens = %d, want 150", got.PromptTokens)
	}
	if got.CompletionTokens != 250 {
		t.Errorf("CompletionTokens = %d, want 250 (delta's cumulative total, not start's partial 1)", got.CompletionTokens)
	}
	if got.CachedTokens != 50 {
		t.Errorf("CachedTokens = %d, want 50", got.CachedTokens)
	}
}
