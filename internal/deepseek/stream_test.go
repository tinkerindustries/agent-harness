package deepseek

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func decodeChunk(t *testing.T, raw string) wire.ChatCompletionChunk {
	t.Helper()
	var chunk wire.ChatCompletionChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	return chunk
}

func TestChunkToEventsReasoningNullContent(t *testing.T) {
	// delta.content is an explicit JSON null while reasoning streams, not
	// absent (docs/OBSERVED.md). This must produce a reasoning delta and no
	// content delta at all, not a content delta with an empty string.
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

func TestChunkToEventsUsageRidesFinalContentChunk(t *testing.T) {
	// Usage does not arrive on its own empty-choices chunk, contrary to the
	// vendored docs: it rides on the final chunk, which still has a
	// populated choices array and a finish_reason (docs/OBSERVED.md).
	raw := `{"choices":[{"index":0,"delta":{"content":"","reasoning_content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":293,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":293,"completion_tokens":10,"total_tokens":303}}`
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
			if e.Usage.PromptTokens != 293 {
				t.Errorf("Usage.PromptTokens = %d, want 293", e.Usage.PromptTokens)
			}
		}
		if e.Type == wire.EventContentDelta {
			t.Errorf("got content delta for empty content string, want none")
		}
	}
	if !sawFinish {
		t.Error("no EventFinish produced")
	}
	if !sawUsage {
		t.Error("no EventUsage produced")
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

func TestIsReasoningStarved(t *testing.T) {
	c := NewClient("http://unused.invalid", "test-key")
	cases := []struct {
		name         string
		finishReason string
		content      string
		want         bool
	}{
		{"length with empty content", wire.FinishLength, "", true},
		{"length with content", wire.FinishLength, "partial answer", false},
		{"stop with empty content", wire.FinishStop, "", false},
		{"stop with content", wire.FinishStop, "answer", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.IsReasoningStarved(tc.finishReason, tc.content); got != tc.want {
				t.Errorf("IsReasoningStarved(%q, %q) = %v, want %v", tc.finishReason, tc.content, got, tc.want)
			}
		})
	}
}
