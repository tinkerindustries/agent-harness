package deepseek

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// pumpGoroutines counts goroutines currently inside
// providerhttp.Transport.PumpStream, the shared SSE pump
// StreamChatCompletion runs on (internal/providerhttp).
func pumpGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "providerhttp.(*Transport).PumpStream")
}

// A caller may cancel its context and stop reading the event channel at the
// same moment. Every send in PumpStream is guarded by ctx so that neither the
// goroutine nor the response body is stranded on an unbuffered send.
func TestCancelledStreamDoesNotStrandPump(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for i := 0; i < 2000; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%s\"}}]}\n\n", strings.Repeat("x", 32))
			flusher.Flush()
			time.Sleep(time.Millisecond)
			if r.Context().Err() != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(srv.URL, "test-key")
	events, err := c.StreamChatCompletion(ctx, wire.ChatIntent{
		Model:     "deepseek-v4-flash",
		Messages:  []wire.Message{wire.UserMessage("hi")},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	<-events // consume one event, then abandon the channel
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pumpGoroutines() == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pumpStream still running %d goroutine(s) after cancel with an unread channel", pumpGoroutines())
}
