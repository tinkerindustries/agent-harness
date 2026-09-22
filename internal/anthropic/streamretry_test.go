package anthropic

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// resetConnection hijacks w's connection and closes it hard (SetLinger(0)),
// producing a genuine net.Error on the client's read — the same technique
// internal/gemini/streamretry_test.go uses, and the shape
// providerhttp.isRetryableStreamErr's net.Error branch is written for.
func resetConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	conn.Close()
}

// shrinkBackoff points a test Client's retry schedule at millisecond scale,
// so a test exercising RetryStream's real backoff sleep does not take real
// time to run — the same helper internal/gemini's and internal/deepseek's
// own streamretry_test.go files carry for their clients.
func shrinkBackoff(c *Client) {
	c.transport.RetryBase = time.Millisecond
	c.transport.RetryMax = 5 * time.Millisecond
}

// TestStreamChatCompletionRetriesResetBeforeFirstFrame proves this client
// gets the same mid-stream retry internal/deepseek, internal/kimi and
// internal/gemini do: a connection that resets after a 200 but before
// producing any SSE output is retried invisibly, on a fresh request, rather
// than ending the sub-turn (client.go's streamOneRound, wired onto
// providerhttp.Transport.RetryStream).
func TestStreamChatCompletionRetriesResetBeforeFirstFrame(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			resetConnection(t, w)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseResponse(
			frame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`),
			frame("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	shrinkBackoff(c)

	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	var content string
	for e := range events {
		if e.Type == wire.EventError {
			t.Fatalf("unexpected error event reached the caller: %v (the reset should have been retried invisibly)", e.Err)
		}
		if e.Type == wire.EventContentDelta {
			content += e.Content
		}
	}

	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Fatalf("server saw %d attempt(s), want 2 (the reset attempt plus the retry)", n)
	}
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
}

// TestStreamChatCompletionRetryDuringPauseTurnRoundTwo proves the retry
// applies per round rather than only to the first request in the pause_turn
// loop: the second round's own connection resets once and is reopened with
// the same resumed request body, and the loop still produces one logical
// response.
func TestStreamChatCompletionRetryDuringPauseTurnRoundTwo(t *testing.T) {
	var requests int32
	var secondRoundAttempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		if n == 1 {
			w.WriteHeader(http.StatusOK)
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
		// Every request from here on is round two (the pause_turn
		// continuation); the first of those resets, the second succeeds.
		if atomic.AddInt32(&secondRoundAttempts, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			resetConnection(t, w)
			return
		}
		w.WriteHeader(http.StatusOK)
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
	shrinkBackoff(c)

	events, err := c.StreamChatCompletion(context.Background(), testIntent())
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	var content string
	var sawFinish bool
	for e := range events {
		switch e.Type {
		case wire.EventError:
			t.Fatalf("unexpected error event: %v", e.Err)
		case wire.EventContentDelta:
			content += e.Content
		case wire.EventFinish:
			sawFinish = true
		}
	}
	if content != "done" {
		t.Errorf("content = %q, want %q", content, "done")
	}
	if !sawFinish {
		t.Error("expected an EventFinish once the pause_turn loop ended")
	}
	if n := atomic.LoadInt32(&secondRoundAttempts); n != 2 {
		t.Fatalf("second round saw %d attempt(s), want 2 (the reset attempt plus the retry)", n)
	}
}
