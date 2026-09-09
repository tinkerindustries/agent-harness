package deepseek

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// decodeOne runs one frame through the decoder and fails on error.
func decodeOne(t *testing.T, data string) ([]wire.Event, bool) {
	t.Helper()
	events, done, err := decodeResponsesFrame(data)
	if err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return events, done
}

// The two text streams land on the two text events, and an empty delta
// produces nothing rather than an empty event the loop would append.
func TestDecodeTextDeltas(t *testing.T) {
	events, done := decodeOne(t, `{"type":"response.reasoning_text.delta","output_index":0,"delta":"think"}`)
	if done || len(events) != 1 || events[0].Type != wire.EventReasoningDelta || events[0].Reasoning != "think" {
		t.Errorf("reasoning delta = %+v (done=%v)", events, done)
	}

	events, _ = decodeOne(t, `{"type":"response.output_text.delta","output_index":1,"delta":"hello"}`)
	if len(events) != 1 || events[0].Type != wire.EventContentDelta || events[0].Content != "hello" {
		t.Errorf("content delta = %+v", events)
	}

	if events, _ := decodeOne(t, `{"type":"response.output_text.delta","output_index":1,"delta":""}`); len(events) != 0 {
		t.Errorf("an empty delta produced %+v, want nothing", events)
	}
}

// A tool call is announced once and its arguments arrive separately, both
// carrying output_index — which is the whole reason the decoder can stay
// stateless: the assembler keys by that index.
func TestDecodeToolCallAcrossFrames(t *testing.T) {
	asm := wire.NewToolCallAssembler()

	for _, frame := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"List"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\""}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":":\".\"}"}`,
	} {
		events, done := decodeOne(t, frame)
		if done {
			t.Fatalf("frame ended the stream early: %s", frame)
		}
		for _, ev := range events {
			if ev.Type != wire.EventToolCallDelta {
				t.Fatalf("unexpected event %+v from %s", ev, frame)
			}
			asm.Add(ev.ToolCall)
		}
	}

	calls := asm.Finalize()
	if len(calls) != 1 {
		t.Fatalf("assembled %d calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "List" || calls[0].Arguments != `{"path":"."}` {
		t.Errorf("assembled call = %+v", calls[0])
	}
	// The reasoning item's own output_item.added must not have opened a
	// second call at index 0.
	if calls[0].Type != "function" {
		t.Errorf("call type = %q, want function", calls[0].Type)
	}
}

// The terminal event carries usage and the finish reason, and ends the
// stream. Usage comes first, matching the chat completion pump's order.
func TestDecodeTerminalEvent(t *testing.T) {
	events, done := decodeOne(t, `{"type":"response.completed","response":{"status":"completed",`+
		`"output":[{"type":"function_call","call_id":"c"}],`+
		`"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":64},"output_tokens":5,"total_tokens":105}}}`)
	if !done {
		t.Error("response.completed did not end the stream")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want usage then finish: %+v", len(events), events)
	}
	if events[0].Type != wire.EventUsage || events[0].Usage.PromptCacheHitTokens != 64 {
		t.Errorf("usage event = %+v", events[0])
	}
	if events[1].Type != wire.EventFinish || events[1].FinishReason != wire.FinishToolCalls {
		t.Errorf("finish event = %+v", events[1])
	}

	// Truncation is the loop's "length", which is what makes its
	// reasoning-starvation retry fire on this surface too.
	events, done = decodeOne(t, `{"type":"response.incomplete","response":{"status":"incomplete",`+
		`"incomplete_details":{"reason":"max_output_tokens"}}}`)
	if !done || len(events) != 1 || events[0].FinishReason != wire.FinishLength {
		t.Errorf("incomplete = %+v (done=%v)", events, done)
	}
}

// A failure mid-stream arrives as the stream's terminal error carrying the
// provider's own message, and still reports the tokens already spent.
func TestDecodeFailedEvent(t *testing.T) {
	events, done := decodeOne(t, `{"type":"response.failed","response":{"status":"failed",`+
		`"error":{"code":"server_error","message":"upstream exploded"},`+
		`"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"total_tokens":12}}}`)
	if !done {
		t.Error("response.failed did not end the stream")
	}
	if len(events) != 2 || events[0].Type != wire.EventUsage {
		t.Fatalf("want usage then error, got %+v", events)
	}
	if events[1].Type != wire.EventError || !errors.Is(events[1].Err, ErrResponseFailed) {
		t.Fatalf("error event = %+v", events[1])
	}
	for _, want := range []string{"upstream exploded", "server_error"} {
		if !strings.Contains(events[1].Err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, events[1].Err)
		}
	}
}

// An event type this decoder does not read is skipped, not an error: the
// surface defines around twenty and a new one must not break a running
// session. A malformed frame is still an error.
func TestDecodeUnknownAndMalformedFrames(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.content_part.added","output_index":0}`,
		`{"type":"response.web_search_call.searching","output_index":0}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
	} {
		events, done := decodeOne(t, frame)
		if len(events) != 0 || done {
			t.Errorf("%s produced %+v (done=%v), want it skipped", frame, events, done)
		}
	}

	if _, _, err := decodeResponsesFrame(`{"type":`); err == nil {
		t.Error("a malformed frame decoded without error")
	}
}

// End to end over HTTP: the client posts to /responses and turns a scripted
// semantic stream into the loop's events.
func TestResponsesClientStreamsOverHTTP(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = string(b)

		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"type":"response.created","response":{"status":"in_progress"}}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"delta":"hmm"}`,
			`{"type":"response.output_text.delta","output_index":1,"delta":"DONE"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"total_tokens":10}}}`,
		} {
			w.Write([]byte("data: " + frame + "\n\n"))
		}
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "sk-test")
	events, err := c.StreamChatCompletion(context.Background(), probeIntent())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var reasoning, content strings.Builder
	var finish string
	var usage *wire.Usage
	for ev := range events {
		switch ev.Type {
		case wire.EventReasoningDelta:
			reasoning.WriteString(ev.Reasoning)
		case wire.EventContentDelta:
			content.WriteString(ev.Content)
		case wire.EventFinish:
			finish = ev.FinishReason
		case wire.EventUsage:
			usage = ev.Usage
		case wire.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if gotPath != "/responses" {
		t.Errorf("posted to %q, want /responses", gotPath)
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("the request did not ask for a stream: %s", gotBody)
	}
	// No stream_options: this surface does not take one and reports usage
	// on its terminal event instead.
	if strings.Contains(gotBody, "stream_options") {
		t.Errorf("the request carried stream_options: %s", gotBody)
	}
	if reasoning.String() != "hmm" || content.String() != "DONE" {
		t.Errorf("reasoning=%q content=%q", reasoning.String(), content.String())
	}
	if finish != wire.FinishStop || usage == nil || usage.TotalTokens != 10 {
		t.Errorf("finish=%q usage=%+v", finish, usage)
	}
}

// The non-streaming call flattens the output items back into the one
// assistant message the loop's non-streaming callers read.
func TestResponsesClientNonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed",` +
			`"model":"deepseek-v4-flash","output":[` +
			`{"type":"reasoning","content":[{"type":"reasoning_text","text":"thought"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},` +
			`{"type":"function_call","call_id":"call_9","name":"List","arguments":"{}"}],` +
			`"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":8},"output_tokens":4,"total_tokens":24}}`))
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "sk-test")
	resp, err := c.CreateChatCompletion(context.Background(), probeIntent())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Content.String() != "answer" {
		t.Errorf("content = %q", msg.Content.String())
	}
	if msg.ReasoningContent == nil || *msg.ReasoningContent != "thought" {
		t.Errorf("reasoning = %v", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_9" || msg.ToolCalls[0].Function.Name != "List" {
		t.Errorf("tool calls = %+v", msg.ToolCalls)
	}
	if resp.Choices[0].FinishReason != wire.FinishToolCalls {
		t.Errorf("finish = %q, want %q", resp.Choices[0].FinishReason, wire.FinishToolCalls)
	}
	if resp.Usage.PromptCacheHitTokens != 8 || resp.Usage.PromptCacheMissTokens != 12 {
		t.Errorf("usage split = %d/%d, want 8/12", resp.Usage.PromptCacheHitTokens, resp.Usage.PromptCacheMissTokens)
	}
}

// The quirk repairs and the usage split are the provider's, not the
// surface's: a ResponsesClient answers them the same way a Client does,
// which is what embedding buys and what a future refactor must not lose.
func TestResponsesClientKeepsTheProvidersBehaviour(t *testing.T) {
	c := NewResponsesClient("https://example.invalid", "sk-test")
	if !c.IsReasoningStarved(wire.FinishLength, "") {
		t.Error("IsReasoningStarved does not carry over")
	}
	if c.CacheSlack() != 127 {
		t.Errorf("CacheSlack = %d, want 127", c.CacheSlack())
	}
	hit, miss := c.UsageSplit(&wire.Usage{PromptCacheHitTokens: 5, PromptCacheMissTokens: 6})
	if hit != 5 || miss != 6 {
		t.Errorf("UsageSplit = %d/%d, want 5/6", hit, miss)
	}
}

// A Responses stream ends on a semantic event with a blank line after it, so
// there is always a line in flight when the pump stops reading. The pump
// must release its line reader on the way out; without that every stream
// leaks a goroutine and holds its connection open (internal/providerhttp,
// PumpStreamWith's `stopped`).
func TestCompletedResponsesStreamStrandsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
		// Frames after the terminal event, which a well-behaved server may
		// still be flushing when the pump decides it is finished.
		for i := 0; i < 50; i++ {
			w.Write([]byte(": keep-alive\n\n"))
		}
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "sk-test")
	events, err := c.StreamChatCompletion(context.Background(), probeIntent())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range events {
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pumpGoroutines() == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%d pump frame(s) still on the stack after the stream ended", pumpGoroutines())
}
