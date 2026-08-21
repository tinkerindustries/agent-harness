package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// spyTransport counts every RoundTrip it sees and delegates to next, so a
// test can tell whether a client's requests actually passed through a
// wrapper installed by WithTransportWrapper rather than going straight to
// the underlying http.Transport.
type spyTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	hits int
}

func (s *spyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hits++
	s.mu.Unlock()
	return s.next.RoundTrip(req)
}

func (s *spyTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// TestWithTransportWrapperCoversEveryCaller pins docs/GEMINI-INTEGRATION.md
// §2's third reason not to use the official Go SDK — "internal/httplog needs
// the transport" — for all three of this package's HTTP-issuing methods, not
// just Interact: cmd/harness builds one *gemini.Client per process
// (withGeminiHTTPLog) and hands it to both the vision tools (Interact,
// through Runner.Gemini) and the agentic coding seam
// (StreamChatCompletion/CreateChatCompletion, through Runner.ClientFor) —
// docs/GEMINI-INTEGRATION.md §7 Phase 5's routing. WithTransportWrapper
// mutates c.httpClient.Transport in place (client.go), and every method here
// sends through that same *http.Client, so a wrapper installed once at
// construction has to catch every one of them or prod's httplog capture
// (internal/httplog, cmd/harness's withGeminiHTTPLog) would be silently
// blind to whichever caller it missed.
func TestWithTransportWrapperCoversEveryCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if probe.Stream {
			// Both Interact and StreamChatCompletion always send stream:true
			// and read the same SSE vocabulary (geminitest.Stream is shared
			// by both surfaces, stream.go's own package doc).
			w.Write([]byte(geminitest.Answer("ok", "")))
			return
		}
		// CreateChatCompletion sends stream:false and reads a plain JSON body.
		w.Write([]byte(`{"id":"v1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer srv.Close()

	var spy *spyTransport
	c := NewClient(srv.URL,
		WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }),
		WithTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
			spy = &spyTransport{next: next}
			return spy
		}),
	)
	if spy == nil {
		t.Fatal("WithTransportWrapper's wrap function was never called")
	}

	ctx := context.Background()
	intent := wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}}

	if _, err := c.CreateChatCompletion(ctx, intent); err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	if got := spy.count(); got != 1 {
		t.Errorf("after CreateChatCompletion, spy transport saw %d request(s), want 1", got)
	}

	ch, err := c.StreamChatCompletion(ctx, intent)
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	for range ch {
	}
	if got := spy.count(); got != 2 {
		t.Errorf("after StreamChatCompletion, spy transport saw %d request(s), want 2", got)
	}

	if _, _, err := c.Interact(ctx, "gemini-3.7-flash", "system", "question", nil); err != nil {
		t.Fatalf("Interact: %v", err)
	}
	if got := spy.count(); got != 3 {
		t.Errorf("after Interact, spy transport saw %d request(s), want 3", got)
	}
}
