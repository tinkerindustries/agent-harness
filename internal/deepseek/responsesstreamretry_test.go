package deepseek

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The three tests in streamretry_test.go pin RetryStream's behaviour on
// /chat/completions. These pin it on /responses — the surface
// `harness stdio-session` actually runs DeepSeek on, and the one the retry
// wiring was missing. Without it a single dead connection ends the run, and
// a session's whole history goes with it.

// TestResponsesStreamRetriesIdleTimeoutBeforeFirstFrame is the production
// failure: a session at 378K prompt tokens opened sub-turn 228, DeepSeek
// accepted the request with a 200 and then sent nothing at all. The idle
// watchdog fired at its 120-second mark and the run ended, 125.8 seconds
// after the sub-turn started — one idle window, no retry, despite
// MaxRetries being 4 and the idle sentinel being exactly what RetryStream
// retries on. The turn had already driven the worktree's tests and smoke
// suite green; all of it was lost.
//
// The first attempt here reproduces that shape — a 200 that holds the
// connection open past the idle timeout without writing a frame — and the
// run must survive it. It fails if the server sees one attempt, or if the
// idle error reaches the caller.
func TestResponsesStreamRetriesIdleTimeoutBeforeFirstFrame(t *testing.T) {
	var attempts int32
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if n == 1 {
			// Silent past the idle timeout: the watchdog, not the peer, is
			// what ends this attempt.
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Write([]byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"hello"}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "test-key", WithIdleTimeout(50*time.Millisecond))
	shrinkBackoff(c.Client)

	events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	got := drainStreamEvents(t, events)

	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Fatalf("server saw %d attempt(s), want 2 (the idle attempt plus the retry)", n)
	}
	var content string
	for _, ev := range got {
		if ev.Type == wire.EventError {
			t.Fatalf("unexpected error event reached the caller: %v (the idle timeout should have been retried invisibly)", ev.Err)
		}
		if ev.Type == wire.EventContentDelta {
			content += ev.Content
		}
	}
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
}

// TestResponsesStreamRetriesResetBeforeFirstFrame is the same connection
// reset streamretry_test.go pins on /chat/completions, against /responses.
func TestResponsesStreamRetriesResetBeforeFirstFrame(t *testing.T) {
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
		w.Write([]byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"hello"}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "test-key")
	shrinkBackoff(c.Client)

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
	for _, ev := range got {
		if ev.Type == wire.EventError {
			t.Fatalf("unexpected error event reached the caller: %v", ev.Err)
		}
	}
}

// TestResponsesStreamDoesNotRetryAfterPartialOutput holds the /responses
// path to the same limit as the other three: a stream that has already sent
// a delta is never reopened, because a second attempt would produce a
// second, independent completion the caller cannot tell from the first.
func TestResponsesStreamDoesNotRetryAfterPartialOutput(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Let the frame cross the loopback socket before the reset, so the
		// client reads the delta before it reads ECONNRESET.
		time.Sleep(20 * time.Millisecond)
		resetConnection(t, w)
	}))
	defer srv.Close()

	c := NewResponsesClient(srv.URL, "test-key")
	shrinkBackoff(c.Client)

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
