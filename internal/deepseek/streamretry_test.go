package deepseek

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

// resetConnection hijacks the response and forces a TCP RST on close — the
// same "read tcp ...: read: connection reset by peer" the production
// incident's terminal error named (docs/OBSERVED.md, 2026-09-11), rather
// than a plain EOF a graceful server close would give the client.
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
// time to run.
func shrinkBackoff(c *Client) {
	c.transport.RetryBase = time.Millisecond
	c.transport.RetryMax = 5 * time.Millisecond
}

// TestStreamChatCompletionRetriesResetBeforeFirstFrame is the production
// incident (docs/OBSERVED.md, 2026-09-11): sub-turn 23 opened a connection,
// DeepSeek accepted it with a 200, and the peer reset the connection 25
// seconds later having sent no SSE frame at all. The first attempt here
// reproduces that shape — a 200 with a hijacked TCP reset before any data —
// and the run must survive it rather than end the turn.
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
	shrinkBackoff(c)

	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
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

// TestStreamChatCompletionDoesNotRetryAfterPartialOutput is the deliberate
// decision on the hard case (docs/DESIGN.md §4.3, "Retrying a stream that
// dies mid-flight"): once a delta has already reached the caller, the same
// reset must not trigger a retry, because a second attempt would send a
// second, independent completion with no way for the caller to tell its
// text apart from the first's.
func TestStreamChatCompletionDoesNotRetryAfterPartialOutput(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Give the already-written frame a moment to actually cross the
		// loopback socket before the reset lands, so the client's read
		// yields the delta before it yields ECONNRESET rather than racing
		// the two on the same connection.
		time.Sleep(20 * time.Millisecond)
		resetConnection(t, w)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	shrinkBackoff(c)

	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	got := drainStreamEvents(t, events)

	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("server saw %d attempt(s), want 1: a stream that already produced output must never be reopened", n)
	}
	if len(got) != 2 || got[0].Type != wire.EventContentDelta || got[0].Content != "partial" || got[1].Type != wire.EventError {
		t.Fatalf("events = %+v, want the partial content delta followed by the connection-reset error, unretried", got)
	}
}

// TestStreamChatCompletionNeverRetriesCancelledContext pins that a caller
// cancelling mid-stream — the operator pressing stop — never triggers a
// retry, even though the stream had produced zero output when it happened.
func TestStreamChatCompletionNeverRetriesCancelledContext(t *testing.T) {
	var attempts int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL, "test-key")
	shrinkBackoff(c)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.StreamChatCompletion(ctx, wire.ChatIntent{
		Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	// Every send downstream of the cancellation is itself guarded by ctx
	// (PumpStreamWith's and RetryStream's own send helpers), so once the
	// caller has cancelled and may stop reading at any moment, the terminal
	// error event can race the cancellation and never arrive — a pre-existing
	// property of that guard, not something this change alters. What this
	// test pins is the one thing that must never happen regardless of that
	// race: a retry.
	got := drainStreamEvents(t, events)

	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("server saw %d attempt(s), want 1: a cancelled context must never be retried", n)
	}
	for _, ev := range got {
		if ev.Type != wire.EventError {
			t.Fatalf("events = %+v, want at most a single error event and nothing that looks like a successful retry", got)
		}
	}
}
