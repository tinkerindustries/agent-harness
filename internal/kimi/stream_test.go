package kimi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/providerhttp"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

func decodeChunk(t *testing.T, raw string) wire.ChatCompletionChunk {
	t.Helper()
	var chunk wire.ChatCompletionChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	return chunk
}

// TestChunkToEventsReasoningNullContent mirrors the DeepSeek case: K3
// streams reasoning_content ahead of content, and delta.content is an
// explicit JSON null while it does, so a reasoning-only frame must produce a
// reasoning delta and no content delta (third_party/kimi-docs/api/chat.md
// "Thinking Mode and Preserved Thinking"; the null-content convention is
// shared wire behaviour, docs/OBSERVED.md).
func TestChunkToEventsReasoningNullContent(t *testing.T) {
	raw := `{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"thinking..."},"finish_reason":null}]}`
	chunk := decodeChunk(t, raw)

	if chunk.Choices[0].Delta.Content != nil {
		t.Fatalf("Delta.Content = %v, want nil (explicit JSON null)", *chunk.Choices[0].Delta.Content)
	}

	events := providerhttp.ChunkToEvents(chunk)
	if len(events) != 1 {
		t.Fatalf("ChunkToEvents() returned %d events, want 1: %+v", len(events), events)
	}
	if events[0].Type != wire.EventReasoningDelta || events[0].Reasoning != "thinking..." {
		t.Errorf("event = %+v, want reasoning delta %q", events[0], "thinking...")
	}
}

func TestChunkToEventsContentDelta(t *testing.T) {
	raw := `{"choices":[{"index":0,"delta":{"content":"hello","reasoning_content":null},"finish_reason":null}]}`
	chunk := decodeChunk(t, raw)

	events := providerhttp.ChunkToEvents(chunk)
	if len(events) != 1 {
		t.Fatalf("ChunkToEvents() returned %d events, want 1: %+v", len(events), events)
	}
	if events[0].Type != wire.EventContentDelta || events[0].Content != "hello" {
		t.Errorf("event = %+v, want content delta %q", events[0], "hello")
	}
}

func TestChunkToEventsToolCallDelta(t *testing.T) {
	raw := `{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_00_x","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`
	chunk := decodeChunk(t, raw)

	events := providerhttp.ChunkToEvents(chunk)
	if len(events) != 1 {
		t.Fatalf("ChunkToEvents() returned %d events, want 1: %+v", len(events), events)
	}
	if events[0].Type != wire.EventToolCallDelta {
		t.Fatalf("event type = %v, want EventToolCallDelta", events[0].Type)
	}
	if events[0].ToolCall.ID != "call_00_x" {
		t.Errorf("ToolCall.ID = %q, want call_00_x", events[0].ToolCall.ID)
	}
}

// TestChunkToEventsUsageFrameWithCachedTokens pins that Kimi's usage chunk —
// the final frame before [DONE], carrying cached_tokens instead of
// DeepSeek's hit/miss pair (third_party/kimi-docs/api/chat.md, openapi.json
// Usage) — decodes into the shared wire.Usage and surfaces as an EventUsage
// whose CachedTokens the client's UsageSplit can map.
func TestChunkToEventsUsageFrameWithCachedTokens(t *testing.T) {
	raw := `{"choices":[{"index":0,"delta":{"content":"","reasoning_content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":12,"total_tokens":312,"cached_tokens":240}}`
	chunk := decodeChunk(t, raw)

	events := providerhttp.ChunkToEvents(chunk)

	var sawFinish, sawUsage bool
	for _, e := range events {
		if e.Type == wire.EventFinish {
			sawFinish = true
			if e.FinishReason != wire.FinishStop {
				t.Errorf("FinishReason = %q, want stop", e.FinishReason)
			}
		}
		if e.Type == wire.EventUsage {
			sawUsage = true
			if e.Usage == nil {
				t.Fatal("EventUsage has nil Usage")
			}
			if e.Usage.PromptTokens != 300 || e.Usage.CachedTokens != 240 {
				t.Errorf("Usage = %+v, want prompt 300 cached 240", e.Usage)
			}
			hit, miss := (&Client{}).UsageSplit(e.Usage)
			if hit != 240 || miss != 60 {
				t.Errorf("UsageSplit = (%d, %d), want (240, 60)", hit, miss)
			}
		}
	}
	if !sawFinish {
		t.Error("no EventFinish produced")
	}
	if !sawUsage {
		t.Error("no EventUsage produced")
	}
}

// TestStreamAssemblesFullResponse drives a complete SSE exchange through the
// client: role frame, reasoning deltas, content deltas, fragmented tool-call
// arguments, the finish frame, and the usage frame with cached_tokens. The
// assembled events must carry reasoning, content, one tool call whose
// fragments joined, the finish reason, and the usage — the shape the agent
// loop folds into a sub-turn (docs/KIMI-INTEGRATION.md §4.1).
func TestStreamAssemblesFullResponse(t *testing.T) {
	frames := []string{
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"Let me think"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":" about it."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"The answer is","reasoning_content":null},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":" 42.","reasoning_content":null},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_01","type":"function","function":{"name":"List","arguments":"{\"path\":\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"."}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80}}`,
	}
	var body string
	for _, f := range frames {
		body += "data: " + f + "\n\n"
	}
	body += "data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "kimi-k3", Messages: []wire.Message{wire.UserMessage("hi")},
		Effort: wire.EffortHigh, MaxTokens: 48000,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	var reasoning, content string
	assembler := wire.NewToolCallAssembler()
	var finishReason string
	var usage *wire.Usage
	for ev := range events {
		switch ev.Type {
		case wire.EventReasoningDelta:
			reasoning += ev.Reasoning
		case wire.EventContentDelta:
			content += ev.Content
		case wire.EventToolCallDelta:
			assembler.Add(ev.ToolCall)
		case wire.EventFinish:
			finishReason = ev.FinishReason
		case wire.EventUsage:
			usage = ev.Usage
		case wire.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if reasoning != "Let me think about it." {
		t.Errorf("reasoning = %q, want %q", reasoning, "Let me think about it.")
	}
	if content != "The answer is 42." {
		t.Errorf("content = %q, want %q", content, "The answer is 42.")
	}
	calls := assembler.Finalize()
	if len(calls) != 1 {
		t.Fatalf("assembled %d tool calls, want 1", len(calls))
	}
	if calls[0].Name != "List" || calls[0].Arguments != `{"path":"."}` {
		t.Errorf("tool call = %+v, want List with arguments %q", calls[0], `{"path":"."}`)
	}
	if finishReason != wire.FinishToolCalls {
		t.Errorf("finish reason = %q, want %q", finishReason, wire.FinishToolCalls)
	}
	if usage == nil {
		t.Fatal("no usage frame received")
	}
	hit, miss := c.UsageSplit(usage)
	if hit != 80 || miss != 20 {
		t.Errorf("UsageSplit = (%d, %d), want (80, 20) from cached_tokens 80 of prompt 100", hit, miss)
	}
}

// TestUsageSplit maps Kimi's single cached_tokens figure onto cache-hit and
// cache-miss the way the cost model reads them: hit is cached_tokens itself,
// miss is the rest of the prompt. When cached_tokens is absent or zero the
// whole prompt bills as a miss; a nil usage maps to zeroes
// (docs/KIMI-INTEGRATION.md §2).
func TestUsageSplit(t *testing.T) {
	c := NewClient("http://unused.invalid", "test-key")
	cases := []struct {
		name     string
		usage    *wire.Usage
		wantHit  int
		wantMiss int
	}{
		{"cached present", &wire.Usage{PromptTokens: 1000, CachedTokens: 900}, 900, 100},
		{"cached zero", &wire.Usage{PromptTokens: 1000, CachedTokens: 0}, 0, 1000},
		{"cached absent", &wire.Usage{PromptTokens: 1000}, 0, 1000},
		{"cached equals prompt", &wire.Usage{PromptTokens: 1000, CachedTokens: 1000}, 1000, 0},
		{"nil usage", nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, miss := c.UsageSplit(tc.usage)
			if hit != tc.wantHit || miss != tc.wantMiss {
				t.Errorf("UsageSplit(%+v) = (%d, %d), want (%d, %d)", tc.usage, hit, miss, tc.wantHit, tc.wantMiss)
			}
		})
	}
}

// TestIsReasoningStarvedAndRepairArgumentsAreNoOps pins that Kimi's two
// quirk repairs do nothing: the starved-retry and brace-repair behaviours
// exist for DeepSeek (docs/OBSERVED.md), and this phase has nothing observed
// about Kimi to repair (docs/KIMI-INTEGRATION.md §4.1).
func TestIsReasoningStarvedAndRepairArgumentsAreNoOps(t *testing.T) {
	c := NewClient("http://unused.invalid", "test-key")
	if c.IsReasoningStarved(wire.FinishLength, "") {
		t.Error("IsReasoningStarved(length, \"\") = true, want false: Kimi has no observed starvation quirk")
	}
	args := `{"path":".`
	repaired, ok := c.RepairArguments(wire.FinishToolCalls, args)
	if ok || repaired != args {
		t.Errorf("RepairArguments = (%q, %v), want (%q, false): Kimi has no observed brace quirk", repaired, ok, args)
	}
}
