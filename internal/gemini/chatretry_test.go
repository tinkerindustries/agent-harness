package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// TestChatCompletionSendsGoogAPIKeyNoAuthorization pins that the agentic
// path — StreamChatCompletion and CreateChatCompletion, both now routed
// through chatTransport.Do — sends the same x-goog-api-key header Interact
// always has (TestAPIKeyRidesInHeader) and never falls back to
// providerhttp.Transport's Authorization: Bearer default.
func TestChatCompletionSendsGoogAPIKeyNoAuthorization(t *testing.T) {
	var gotGoogKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGoogKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &probe)
		w.Header().Set("Content-Type", "application/json")
		if probe.Stream {
			w.Write([]byte(geminitest.Answer("ok", "")))
			return
		}
		w.Write([]byte(`{"id":"v1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-secret", nil }))
	intent := wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}}

	ch, err := c.StreamChatCompletion(context.Background(), intent)
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	drainEvents(t, ch)

	if gotGoogKey != "gk-secret" {
		t.Errorf("x-goog-api-key = %q, want gk-secret", gotGoogKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty: Gemini never sends Bearer", gotAuth)
	}

	gotGoogKey, gotAuth = "", ""
	if _, err := c.CreateChatCompletion(context.Background(), intent); err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	if gotGoogKey != "gk-secret" {
		t.Errorf("CreateChatCompletion: x-goog-api-key = %q, want gk-secret", gotGoogKey)
	}
	if gotAuth != "" {
		t.Errorf("CreateChatCompletion: Authorization = %q, want empty", gotAuth)
	}
}

// TestStreamChatCompletionRetriesTransientStatusThenSucceeds closes the gap
// docs/GEMINI-INTEGRATION.md §8 recorded: a 503 that would previously have
// failed a Gemini sub-turn outright is now retried once, with the retry
// landing on chatTransport.Do before pumpChatEvents ever starts reading —
// and the eventual 200's stream still decodes normally.
func TestStreamChatCompletionRetriesTransientStatusThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("the answer", "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	c.chatTransport.RetryBase = 1 // shrink real backoff sleeps to keep the test fast
	c.chatTransport.RetryMax = 5

	ch, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	events := drainEvents(t, ch)

	if attempts != 2 {
		t.Fatalf("server saw %d attempt(s), want 2 (one 503 plus the success)", attempts)
	}

	var content strings.Builder
	sawFinish := false
	for _, ev := range events {
		switch ev.Type {
		case wire.EventContentDelta:
			content.WriteString(ev.Content)
		case wire.EventFinish:
			sawFinish = true
		case wire.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if content.String() != "the answer" {
		t.Errorf("content = %q, want %q", content.String(), "the answer")
	}
	if !sawFinish {
		t.Error("no EventFinish observed; the retried stream did not decode to completion")
	}
}

// TestStreamChatCompletionDoesNotRetryPermanentStatus pins that a 400 —
// never transient, per retry.go's isRetryableStatus — is surfaced on the
// first attempt rather than burning retries on a request that will never
// succeed. testdata/stream-error-corrupted-signature.sse is the real
// SSE-framed 400 body docs/OBSERVED.md captured (a bad thought signature),
// reused here rather than duplicated, since parseAgenticAPIError must still
// read it correctly once the request goes through chatTransport.Do.
func TestStreamChatCompletionDoesNotRetryPermanentStatus(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-error-corrupted-signature.sse")
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		w.Write(raw)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, err = c.StreamChatCompletion(context.Background(), wire.ChatIntent{Model: "gemini-3.7-flash", Messages: []wire.Message{wire.UserMessage("hi")}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := IsAPIError(err); !ok {
		t.Fatalf("error = %v (%T), want an *APIError", err, err)
	}
	if attempts != 1 {
		t.Errorf("server saw %d attempt(s), want 1: a 400 must not be retried", attempts)
	}
}
