package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// drainEvents collects every event pumpChatEvents (via StreamChatCompletion)
// sends until the channel closes, which it always does — a stream's last
// event is always EventFinish or EventError (client.go's own doc comment on
// StreamChatCompletion).
func drainEvents(t *testing.T, ch <-chan wire.Event) []wire.Event {
	t.Helper()
	var out []wire.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("timed out waiting for the event stream to close")
		}
	}
}

func newStreamServer(t *testing.T, sseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseBody))
	}))
}

// TestStreamChatCompletionToolCall runs the real capture from Phase 2 —
// testdata/stream-tools-function-call.sse, one thought step carrying a
// signature followed by one function_call step — through
// StreamChatCompletion and checks the assembled events, using
// wire.ToolCallAssembler exactly as internal/session/turn.go's own stream
// loop does (docs/GEMINI-INTEGRATION.md §5.4, "Reuse wire.ToolCallAssembler
// if Phase 2 showed it fits").
func TestStreamChatCompletionToolCall(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-tools-function-call.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := newStreamServer(t, string(raw))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("weather?")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)

	var sawSignature string
	assembler := wire.NewToolCallAssembler()
	var usage *wire.Usage
	var finishReason string
	for _, ev := range events {
		switch ev.Type {
		case wire.EventThoughtSignatureDelta:
			sawSignature = ev.ThoughtSignature
		case wire.EventToolCallDelta:
			assembler.Add(ev.ToolCall)
		case wire.EventUsage:
			usage = ev.Usage
		case wire.EventFinish:
			finishReason = ev.FinishReason
		case wire.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}

	if sawSignature == "" {
		t.Error("no thought signature delta observed")
	}
	calls := assembler.Finalize()
	if len(calls) != 1 {
		t.Fatalf("assembled %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "call_3345913" || calls[0].Name != "get_weather" {
		t.Errorf("call = %+v, want id call_3345913, name get_weather", calls[0])
	}
	if calls[0].Arguments != `{"location":"Hobart, Tasmania"}` {
		t.Errorf("arguments = %s, want the fixture's JSON string", calls[0].Arguments)
	}
	if finishReason != wire.FinishToolCalls {
		t.Errorf("finish reason = %q, want %q (a function_call step was present, status was never consulted)", finishReason, wire.FinishToolCalls)
	}
	if usage == nil || usage.CompletionTokens != 19+51 {
		t.Errorf("usage = %+v, want completion 70 (output 19 + thought 51, billed at the output rate)", usage)
	}
}

// TestStreamChatCompletionParallelCalls runs
// testdata/stream-parallel-calls.sse: one thought step (index 0) carrying
// the turn's only signature, then three function_call steps (indices 1-3),
// none of them carrying one — the shape docs/OBSERVED.md's "parallel-call
// signature rule" measured, and the index keying
// wire.ToolCallAssembler needs to keep the three calls apart.
func TestStreamChatCompletionParallelCalls(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-parallel-calls.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := newStreamServer(t, string(raw))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("weather in three cities?")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)

	signatures := 0
	assembler := wire.NewToolCallAssembler()
	for _, ev := range events {
		switch ev.Type {
		case wire.EventThoughtSignatureDelta:
			signatures++
		case wire.EventToolCallDelta:
			assembler.Add(ev.ToolCall)
		case wire.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if signatures != 1 {
		t.Errorf("saw %d thought signature deltas, want exactly 1 for the whole turn", signatures)
	}
	calls := assembler.Finalize()
	if len(calls) != 3 {
		t.Fatalf("assembled %d tool calls, want 3: %+v", len(calls), calls)
	}
	wantLocations := []string{"Hobart, Australia", "Perth, Australia", "Darwin, Australia"}
	for i, want := range wantLocations {
		var args struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal([]byte(calls[i].Arguments), &args); err != nil {
			t.Fatalf("call %d arguments %q: %v", i, calls[i].Arguments, err)
		}
		if args.Location != want {
			t.Errorf("call %d location = %q, want %q", i, args.Location, want)
		}
	}
}

// TestStreamChatCompletionSSEFramedError runs
// testdata/stream-error-corrupted-signature.sse — an SSE-framed error frame
// with no interaction.created or step frames at all, exactly the shape
// docs/OBSERVED.md's "the error body itself is a finding" describes for a
// bad thought signature — through StreamChatCompletion and checks the sole
// event is an EventError carrying the real message, not an opaque decode
// failure.
func TestStreamChatCompletionSSEFramedError(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-error-corrupted-signature.sse")
	if err != nil {
		t.Fatal(err)
	}
	// The live measurement found this SSE-framed body arrives on a plain
	// HTTP 400 (docs/OBSERVED.md), which is what parseAgenticAPIError, the
	// pre-stream path, must handle; a stub server reproduces exactly that
	// rather than a 200 whose stream later carries the error, since that is
	// what was actually observed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(raw)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, err = c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Corrupted thought signature.") {
		t.Fatalf("error = %v, want it to carry the real message from the SSE-framed body", err)
	}
	apiErr, ok := IsAPIError(err)
	if !ok {
		t.Fatalf("error = %v (%T), want an *APIError", err, err)
	}
	if apiErr.Code != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", apiErr.Code)
	}
}

// TestStreamChatCompletionMidStreamError covers the other shape the task
// calls out: an error event arriving after a 200 response has already
// started streaming, which pumpChatEvents' own per-frame switch must catch
// rather than only the pre-stream status check parseAgenticAPIError runs.
func TestStreamChatCompletionMidStreamError(t *testing.T) {
	body := "event: interaction.created\ndata: {\"interaction\":{\"id\":\"v1\",\"status\":\"in_progress\"},\"event_type\":\"interaction.created\"}\n\n" +
		"event: step.start\ndata: {\"index\":0,\"step\":{\"type\":\"thought\"},\"event_type\":\"step.start\"}\n\n" +
		"event: error\ndata: {\"error\":{\"message\":\"Corrupted thought signature.\",\"code\":\"invalid_request\"},\"event_type\":\"error\"}\n\n"
	srv := newStreamServer(t, body)
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one terminal EventError", events)
	}
	if events[0].Type != wire.EventError {
		t.Fatalf("event = %+v, want EventError", events[0])
	}
	if !strings.Contains(events[0].Err.Error(), "Corrupted thought signature.") {
		t.Errorf("error = %v, want the mid-stream error's message", events[0].Err)
	}
}

// TestStreamChatCompletionTruncatedStream pins that a stream cut off with
// no interaction.completed and no error frame — the connection just ends —
// surfaces as an error rather than silently returning nothing, the same
// rule decodeStream enforces for Interact.
func TestStreamChatCompletionTruncatedStream(t *testing.T) {
	body := "event: interaction.created\ndata: {\"interaction\":{\"id\":\"v1\",\"status\":\"in_progress\"},\"event_type\":\"interaction.created\"}\n\n" +
		"event: step.start\ndata: {\"index\":0,\"step\":{\"type\":\"model_output\"},\"event_type\":\"step.start\"}\n\n"
	srv := newStreamServer(t, body)
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)
	if len(events) != 1 || events[0].Type != wire.EventError {
		t.Fatalf("events = %+v, want exactly one EventError", events)
	}
}

// TestStreamChatCompletionPlainText runs testdata/stream-simple.sse — a
// thought step with a signature and no tool involved, then a model_output
// answer split across two text deltas — and checks the assembled content
// and finish reason.
func TestStreamChatCompletionPlainText(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-simple.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := newStreamServer(t, string(raw))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.5-flash", Messages: []wire.Message{wire.UserMessage("primary colors?")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)

	var content strings.Builder
	var finishReason string
	var sawSignature bool
	for _, ev := range events {
		switch ev.Type {
		case wire.EventContentDelta:
			content.WriteString(ev.Content)
		case wire.EventThoughtSignatureDelta:
			sawSignature = true
		case wire.EventFinish:
			finishReason = ev.FinishReason
		case wire.EventError:
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}
	if want := "The three primary colors are red, yellow, and blue."; content.String() != want {
		t.Errorf("content = %q, want %q", content.String(), want)
	}
	if !sawSignature {
		t.Error("no thought signature delta observed, want one even for a no-tool answer")
	}
	if finishReason != wire.FinishStop {
		t.Errorf("finish reason = %q, want %q: no function_call step appeared", finishReason, wire.FinishStop)
	}
}

// TestStreamChatCompletionIdleTimeout pins the watchdog: a server that opens
// the connection and never sends a frame ends the stream with
// ErrIdleTimeout rather than hanging the loop forever.
func TestStreamChatCompletionIdleTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c := NewClient(srv.URL,
		WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }),
		WithChatIdleTimeout(20*time.Millisecond))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)
	if len(events) != 1 || events[0].Type != wire.EventError || !errors.Is(events[0].Err, ErrIdleTimeout) {
		t.Fatalf("events = %+v, want a single ErrIdleTimeout error", events)
	}
}

// TestCreateChatCompletionUnarySignature pins docs/OBSERVED.md's "Unary
// (stream: false)" finding: the signature sits directly on the step object
// rather than in a delta, which CreateChatCompletion must read into
// Message.ThoughtSignature the same way the streaming path's assembled
// EventThoughtSignatureDelta feeds it (docs/GEMINI-INTEGRATION.md, "Phase
// 3's note on unary mode").
func TestCreateChatCompletionUnarySignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatInteractionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("CreateChatCompletion must send stream:false")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "v1_unary",
			"status": "completed",
			"steps": [
				{"type": "thought", "signature": "unary-sig-123"},
				{"type": "function_call", "id": "call_9", "name": "get_weather", "arguments": {"location": "Hobart"}}
			],
			"usage": {"total_tokens": 100, "total_input_tokens": 80, "total_cached_tokens": 20, "total_output_tokens": 10, "total_thought_tokens": 10}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	resp, err := c.CreateChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("weather?")}})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.ThoughtSignature == nil || *msg.ThoughtSignature != "unary-sig-123" {
		t.Errorf("thought signature = %v, want unary-sig-123 read directly off the step, not a delta", msg.ThoughtSignature)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"location": "Hobart"}` {
		t.Errorf("tool calls = %+v, want get_weather with the object's bytes carried straight through", msg.ToolCalls)
	}
	if resp.Choices[0].FinishReason != wire.FinishToolCalls {
		t.Errorf("finish reason = %q, want %q", resp.Choices[0].FinishReason, wire.FinishToolCalls)
	}
	if resp.Usage == nil || resp.Usage.CachedTokens != 20 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want cached 20, completion 20 (output 10 + thought 10)", resp.Usage)
	}
}

// TestCreateChatCompletionPlainTextFinish pins the model_output-only case:
// no tool calls, so FinishReason is FinishStop and Content carries the
// concatenated text.
func TestCreateChatCompletionPlainTextFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"v1","status":"completed","steps":[
			{"type":"thought","signature":"sig"},
			{"type":"model_output","content":[{"type":"text","text":"Summary: "},{"type":"text","text":"done."}]}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	resp, err := c.CreateChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("summarise")}})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content.String() != "Summary: done." {
		t.Errorf("content = %q, want %q", msg.Content.String(), "Summary: done.")
	}
	if resp.Choices[0].FinishReason != wire.FinishStop {
		t.Errorf("finish reason = %q, want %q", resp.Choices[0].FinishReason, wire.FinishStop)
	}
}

// TestUsageToWireMatchesTokenSplit pins that usageToWire and Usage.TokenSplit
// cannot silently drift apart: whatever TokenSplit says the billed
// completion and cache split are is exactly what UsageSplit, fed the wire.Usage
// usageToWire built, must report back.
func TestUsageToWireMatchesTokenSplit(t *testing.T) {
	u := &Usage{TotalTokens: 200, TotalInputTokens: 150, TotalCachedTokens: 60, TotalOutputTokens: 30, TotalThoughtTokens: 20}
	wantHit, wantMiss, wantCompletion, wantReasoning := u.TokenSplit()

	wireUsage := usageToWire(u)
	if wireUsage.CompletionTokens != wantCompletion {
		t.Errorf("CompletionTokens = %d, want %d", wireUsage.CompletionTokens, wantCompletion)
	}
	if wireUsage.CompletionTokensDetails == nil || wireUsage.CompletionTokensDetails.ReasoningTokens != wantReasoning {
		t.Errorf("ReasoningTokens = %+v, want %d", wireUsage.CompletionTokensDetails, wantReasoning)
	}

	c := &Client{}
	gotHit, gotMiss := c.UsageSplit(wireUsage)
	if gotHit != wantHit || gotMiss != wantMiss {
		t.Errorf("UsageSplit = (%d, %d), want (%d, %d) from Usage.TokenSplit", gotHit, gotMiss, wantHit, wantMiss)
	}

	if hit, miss := c.UsageSplit(nil); hit != 0 || miss != 0 {
		t.Errorf("UsageSplit(nil) = (%d, %d), want (0, 0)", hit, miss)
	}
}

// TestCacheSlackIsLoose pins that CacheSlack returns the measured value
// (docs/OBSERVED.md, "CacheSlack — measured") rather than a
// since-forgotten placeholder — a regression here is a sign someone changed
// the number without updating the comment that explains it.
func TestCacheSlackIsLoose(t *testing.T) {
	c := &Client{}
	if got := c.CacheSlack(); got != 8192 {
		t.Errorf("CacheSlack() = %d, want the measured 8192", got)
	}
}

// TestIsReasoningStarved pins the DeepSeek-identical predicate: true only
// on FinishLength with no content at all, matching a live measurement that
// max_output_tokens=50 answered 200 with status "incomplete" and 46 output
// tokens against 678 uncapped (finishReasonFor maps "incomplete" onto
// FinishLength). false for FinishLength with any content, and false for
// any other finish reason regardless of content — a truncated-but-nonempty
// answer is not starvation, the same distinction
// internal/deepseek/stream.go's own IsReasoningStarved draws.
func TestIsReasoningStarved(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name         string
		finishReason string
		content      string
		want         bool
	}{
		{"length with no content is starved", wire.FinishLength, "", true},
		{"length with content is not starved", wire.FinishLength, "partial answer", false},
		{"stop with no content is not starved", wire.FinishStop, "", false},
		{"tool_calls is not starved", wire.FinishToolCalls, "", false},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			if got := c.IsReasoningStarved(c2.finishReason, c2.content); got != c2.want {
				t.Errorf("IsReasoningStarved(%q, %q) = %v, want %v", c2.finishReason, c2.content, got, c2.want)
			}
		})
	}
}

// TestRepairArgumentsIsANoOp pins the one genuine absence-of-evidence no-op
// this phase still has — see toolcall.go's own comment for why.
func TestRepairArgumentsIsANoOp(t *testing.T) {
	c := &Client{}
	if repaired, ok := c.RepairArguments(wire.FinishToolCalls, `{"a":1}`); ok || repaired != `{"a":1}` {
		t.Errorf("RepairArguments = (%q, %v), want the input unchanged and ok=false", repaired, ok)
	}
}

// TestFinishReasonForIncompleteStatus pins that a capped generation's
// "incomplete" status becomes FinishLength — through the streaming path
// (a synthetic interaction.completed frame) and directly through
// finishReasonFor, which chatCompletionResponseFromRaw (client.go) also
// calls for the unary path, so both share this one mapping. A pending
// function_call step still wins over status, per the "status is always
// completed, even mid-turn with a pending call" finding: sawFunctionCall
// must take precedence even when status also happens to read "incomplete".
func TestFinishReasonForIncompleteStatus(t *testing.T) {
	if got := finishReasonFor(chatStreamState{status: interactionStatusIncomplete}); got != wire.FinishLength {
		t.Errorf("finishReasonFor(incomplete) = %q, want %q", got, wire.FinishLength)
	}
	if got := finishReasonFor(chatStreamState{status: "completed"}); got != wire.FinishStop {
		t.Errorf("finishReasonFor(completed) = %q, want %q", got, wire.FinishStop)
	}
	if got := finishReasonFor(chatStreamState{sawFunctionCall: true, status: interactionStatusIncomplete}); got != wire.FinishToolCalls {
		t.Errorf("finishReasonFor(pending call + incomplete) = %q, want %q (a pending call outranks status)", got, wire.FinishToolCalls)
	}
}

// TestStreamChatCompletionIncompleteStatus runs a synthetic stream whose
// interaction.completed frame reports status "incomplete" — the shape a
// live max_output_tokens=50 call produced — and checks StreamChatCompletion
// surfaces FinishLength, not FinishStop, so IsReasoningStarved has
// something real to key off once internal/session/turn.go wires this
// client in.
func TestStreamChatCompletionIncompleteStatus(t *testing.T) {
	body := "event: interaction.created\ndata: {\"interaction\":{\"id\":\"v1\",\"status\":\"in_progress\"},\"event_type\":\"interaction.created\"}\n\n" +
		"event: step.start\ndata: {\"index\":0,\"step\":{\"type\":\"model_output\"},\"event_type\":\"step.start\"}\n\n" +
		"event: step.delta\ndata: {\"index\":0,\"delta\":{\"text\":\"partial\",\"type\":\"text\"},\"event_type\":\"step.delta\"}\n\n" +
		"event: step.stop\ndata: {\"index\":0,\"event_type\":\"step.stop\"}\n\n" +
		"event: interaction.completed\ndata: {\"interaction\":{\"id\":\"v1\",\"status\":\"incomplete\"},\"event_type\":\"interaction.completed\"}\n\n" +
		"event: done\ndata: [DONE]\n\n"
	srv := newStreamServer(t, body)
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "gemini-3.7-flash", MaxTokens: 50,
		Messages: []wire.Message{wire.UserMessage("write an essay")},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)
	var finishReason string
	for _, ev := range events {
		if ev.Type == wire.EventFinish {
			finishReason = ev.FinishReason
		}
		if ev.Type == wire.EventError {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}
	if finishReason != wire.FinishLength {
		t.Errorf("finish reason = %q, want %q", finishReason, wire.FinishLength)
	}
}
