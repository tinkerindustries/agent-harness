package kimi

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

func drainStreamEvents(t *testing.T, ch <-chan wire.Event) []wire.Event {
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
			t.Fatal("timed out waiting for the stream to close")
		}
	}
}

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

// TestStreamChatCompletionRetriesResetBeforeFirstFrame proves Kimi's client
// gets the same mid-stream retry internal/deepseek does through the shared
// providerhttp.Transport.RetryStream, guarding against the wiring in the two
// StreamChatCompletion methods drifting apart even though they are
// near-identical by design.
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	c.transport.RetryBase = time.Millisecond
	c.transport.RetryMax = 5 * time.Millisecond

	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "kimi-k3", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	got := drainStreamEvents(t, events)

	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Fatalf("server saw %d attempt(s), want 2 (the reset attempt plus the retry)", n)
	}
	var content string
	for _, ev := range got {
		if ev.Type == wire.EventError {
			t.Fatalf("unexpected error event reached the caller: %v (the reset should have been retried invisibly)", ev.Err)
		}
		if ev.Type == wire.EventContentDelta {
			content += ev.Content
		}
	}
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
}
