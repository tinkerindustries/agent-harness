package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func testIntent() wire.ChatIntent {
	return wire.ChatIntent{
		Model: ModelSonnet5,
		Items: []wire.Item{
			wire.SystemItem("you are a coding agent"),
			wire.UserItem("say hi"),
		},
		Effort:    "high",
		MaxTokens: 1024,
	}
}

func sseResponse(frames ...string) string {
	return strings.Join(frames, "\n\n") + "\n\n"
}

func TestClientSendsRequiredHeaders(t *testing.T) {
	var gotAuth, gotVersion, gotBeta, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`),
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	for range events {
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "ak-test" {
		t.Errorf("x-api-key = %q", gotAuth)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if gotBeta != thinkingBindingBeta {
		t.Errorf("anthropic-beta = %q", gotBeta)
	}
}

func TestStreamChatCompletionEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello!"}}`),
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	var content strings.Builder
	var sawProviderBlocks, sawUsage, sawFinish bool
	var finishReason string
	for e := range events {
		switch e.Type {
		case wire.EventContentDelta:
			content.WriteString(e.Content)
		case wire.EventProviderBlocks:
			sawProviderBlocks = true
			var blocks []rawContentBlock
			if err := json.Unmarshal(e.ProviderBlocks, &blocks); err != nil {
				t.Fatalf("decode provider blocks: %v", err)
			}
			if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "Hello!" {
				t.Errorf("provider blocks = %+v", blocks)
			}
		case wire.EventUsage:
			sawUsage = true
			if e.Usage.CompletionTokens != 5 {
				t.Errorf("CompletionTokens = %d, want 5", e.Usage.CompletionTokens)
			}
		case wire.EventFinish:
			sawFinish = true
			finishReason = e.FinishReason
		case wire.EventError:
			t.Fatalf("unexpected error event: %v", e.Err)
		}
	}
	if content.String() != "Hello!" {
		t.Errorf("content = %q", content.String())
	}
	if !sawProviderBlocks || !sawUsage || !sawFinish {
		t.Errorf("saw provider blocks=%v usage=%v finish=%v, want all true", sawProviderBlocks, sawUsage, sawFinish)
	}
	if finishReason != wire.FinishStop {
		t.Errorf("FinishReason = %q, want %q", finishReason, wire.FinishStop)
	}
}

func TestStreamChatCompletionPauseTurnResumesInternally(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			fmt.Fprint(w, sseResponse(
				frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
				frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`),
				frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`),
				frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
				frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
				frame("message_stop", `{"type":"message_stop"}`),
			))
			return
		}
		// The second request must carry the first response's blocks as a
		// continuing assistant turn with no new user message.
		var body MessagesRequest
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode resumed request: %v", err)
		}
		if len(body.Messages) == 0 || body.Messages[len(body.Messages)-1].Role != wire.RoleAssistant {
			t.Errorf("resumed request's last message = %+v, want an assistant continuation", body.Messages)
		}
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1}}}`),
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`),
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	var content strings.Builder
	var providerBlocks json.RawMessage
	var usage *wire.Usage
	for e := range events {
		switch e.Type {
		case wire.EventContentDelta:
			content.WriteString(e.Content)
		case wire.EventProviderBlocks:
			providerBlocks = e.ProviderBlocks
		case wire.EventUsage:
			usage = e.Usage
		case wire.EventError:
			t.Fatalf("unexpected error: %v", e.Err)
		}
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Fatalf("server saw %d requests, want 2", requests)
	}
	if content.String() != "done" {
		t.Errorf("content = %q", content.String())
	}
	var blocks []rawContentBlock
	if err := json.Unmarshal(providerBlocks, &blocks); err != nil {
		t.Fatalf("decode provider blocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("provider blocks = %d, want 2 (server_tool_use from request 1, text from request 2): %+v", len(blocks), blocks)
	}
	// Usage sums across both resumed requests.
	if usage == nil || usage.CompletionTokens != 8 {
		t.Errorf("usage = %+v, want CompletionTokens 8 (5+3)", usage)
	}
}

func TestStreamChatCompletionRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal"}},"usage":{"output_tokens":1}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	var gotErr error
	for e := range events {
		if e.Type == wire.EventError {
			gotErr = e.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected an EventError for a refusal")
	}
	var refusal *RefusalError
	if _, ok := gotErr.(*RefusalError); !ok {
		t.Fatalf("err = %T (%v), want *RefusalError", gotErr, gotErr)
	}
	_ = refusal
}

func TestStreamChatCompletionRetriesTransientStatus(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`),
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	var content strings.Builder
	for e := range events {
		if e.Type == wire.EventContentDelta {
			content.WriteString(e.Content)
		}
		if e.Type == wire.EventError {
			t.Fatalf("unexpected error: %v", e.Err)
		}
	}
	if content.String() != "ok" {
		t.Errorf("content = %q, want ok after retry", content.String())
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2 (one 503, one 200)", requests)
	}
}

func TestNoAPIKeyErrorsOnTheChannel(t *testing.T) {
	// StreamChatCompletion defers every failure, ErrNoAPIKey included, onto
	// the event channel rather than returning one synchronously — the
	// pause_turn resume loop needs to report a mid-stream failure the same
	// way, so there is one error path rather than two.
	c := NewClient("http://unused.invalid")
	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion returned a synchronous error: %v", err)
	}
	var gotErr error
	for e := range events {
		if e.Type == wire.EventError {
			gotErr = e.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected an EventError for a missing API key")
	}
	if gotErr != ErrNoAPIKey && !strings.Contains(gotErr.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("err = %v, want ErrNoAPIKey or a message naming ANTHROPIC_API_KEY", gotErr)
	}
}

func TestCreateChatCompletionUnary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req MessagesRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			t.Error("CreateChatCompletion must send stream:false")
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(messagesResponse{
			ID:         "msg_1",
			StopReason: "end_turn",
			Content:    []rawContentBlock{{Type: "text", Text: "summary text"}},
			Usage:      &messagesUsage{InputTokens: 100, OutputTokens: 20},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	resp, err := c.CreateChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content.String() != "summary text" {
		t.Errorf("Choices = %+v", resp.Choices)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestUsageSplitSubtractsCacheWriteFromMiss(t *testing.T) {
	c := NewClient("")
	usage := &wire.Usage{PromptTokens: 1000, CachedTokens: 400, CacheWriteTokens: 200}
	hit, miss := c.UsageSplit(usage)
	if hit != 400 {
		t.Errorf("hit = %d, want 400", hit)
	}
	if miss != 400 {
		t.Errorf("miss = %d, want 400 (1000-400-200)", miss)
	}
	if hit, miss := c.UsageSplit(nil); hit != 0 || miss != 0 {
		t.Errorf("UsageSplit(nil) = %d, %d, want 0, 0", hit, miss)
	}
}

func TestClientDefaultsAreNoOps(t *testing.T) {
	c := NewClient("")
	if c.IsReasoningStarved(wire.FinishLength, "") != false {
		t.Error("IsReasoningStarved should always be false")
	}
	if repaired, ok := c.RepairArguments("", "{}"); ok || repaired != "{}" {
		t.Errorf("RepairArguments = %q, %v, want no-op", repaired, ok)
	}
	if c.CacheSlack() <= 0 {
		t.Error("CacheSlack should be a positive placeholder")
	}
}

func TestWithIdleTimeoutOverride(t *testing.T) {
	c := NewClient("", WithIdleTimeout(5*time.Millisecond))
	if c.idleTimeout() != 5*time.Millisecond {
		t.Errorf("idleTimeout() = %v", c.idleTimeout())
	}
}
