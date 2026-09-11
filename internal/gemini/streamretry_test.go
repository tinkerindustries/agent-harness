package gemini

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

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

// A minimal step.delta stream: one text step producing "hello", then
// interaction.completed and the done sentinel.
const streamRetrySuccessBody = "event: step.start\n" +
	"data: {\"index\":0,\"step\":{\"type\":\"text\"}}\n\n" +
	"event: step.delta\n" +
	"data: {\"index\":0,\"delta\":{\"type\":\"text\",\"text\":\"hello\"}}\n\n" +
	"event: interaction.completed\n" +
	"data: {\"interaction\":{\"id\":\"x1\",\"status\":\"completed\"}}\n\n" +
	"event: done\n" +
	"data: [DONE]\n\n"

// TestStreamChatCompletionRetriesResetBeforeFirstFrame proves Gemini's
// client gets the same mid-stream retry internal/deepseek and internal/kimi
// do, even though it supplies its own pump (pumpChatEvents, reading a frame
// vocabulary providerhttp.Transport.PumpStream knows nothing about) rather
// than the shared one — the wiring is a method value handed to
// providerhttp.Transport.RetryStream directly (client.go's own doc comment
// on StreamChatCompletion), and this is what proves that adapter-free wiring
// actually works end to end rather than only type-checking.
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
		w.Write([]byte(streamRetrySuccessBody))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	c.chatTransport.RetryBase = time.Millisecond
	c.chatTransport.RetryMax = 5 * time.Millisecond

	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	got := drainEvents(t, events)

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
