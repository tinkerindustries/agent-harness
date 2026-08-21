package providerhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var errNoKeyForTest = errors.New("test: no api key configured")

// newTestTransport builds a Transport whose Retryable treats only 503 as
// transient, RetryBase/RetryMax shrunk so a test exercising real backoff
// sleeps does not take real time to run.
func newTestTransport(baseURL string) *Transport {
	return &Transport{
		BaseURL: baseURL,
		APIKeyProvider: func() (string, error) {
			return "test-key", nil
		},
		HTTPClient: &http.Client{},
		MaxRetries: 4,
		RetryBase:  time.Millisecond,
		RetryMax:   5 * time.Millisecond,
		Retryable: func(code int) bool {
			return code == http.StatusServiceUnavailable
		},
		NoAPIKey:  errNoKeyForTest,
		ErrPrefix: "test",
	}
}

// TestSetAuthNilKeepsBearerDefault pins the seam's central contract: a
// Transport built with no SetAuth — exactly what internal/deepseek's and
// internal/kimi's Transport literals do — still sends Authorization: Bearer
// <key> and nothing under a Gemini-shaped header name.
func TestSetAuthNilKeepsBearerDefault(t *testing.T) {
	var gotAuth, gotGoogKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotGoogKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	resp, err := tr.Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotGoogKey != "" {
		t.Errorf("x-goog-api-key = %q, want empty: SetAuth is nil", gotGoogKey)
	}
}

// TestSetAuthOverridesCredentialHeader pins the other half: a Transport with
// SetAuth set sends whatever that hook wrote, and never falls back to
// Authorization: Bearer behind its back.
func TestSetAuthOverridesCredentialHeader(t *testing.T) {
	var gotAuth, gotGoogKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotGoogKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	tr.SetAuth = func(req *http.Request, apiKey string) {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	resp, err := tr.Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotGoogKey != "test-key" {
		t.Errorf("x-goog-api-key = %q, want %q", gotGoogKey, "test-key")
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty: SetAuth replaces the default entirely", gotAuth)
	}
}

// TestDoRetriesTransientStatusThenSucceeds pins the mechanism every
// provider's Retryable predicate plugs into: a transient status is retried
// with backoff until a non-retryable (here, 200) response arrives.
func TestDoRetriesTransientStatusThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	resp, err := tr.Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if attempts != 2 {
		t.Errorf("server saw %d attempt(s), want 2 (one 503 plus the success)", attempts)
	}
}

// TestDoDoesNotRetryPermanentStatus pins that a status Retryable rejects —
// 400 here, never transient for any provider in this codebase — is returned
// on the first attempt, with no retry burning a turn on it.
func TestDoDoesNotRetryPermanentStatus(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	resp, err := tr.Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if attempts != 1 {
		t.Errorf("server saw %d attempt(s), want 1", attempts)
	}
}

// TestDoStopsAtMaxRetries pins that a status that never stops being
// transient is retried exactly MaxRetries times and then returned as-is —
// MaxRetries+1 total requests, not an unbounded loop.
func TestDoStopsAtMaxRetries(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	tr.MaxRetries = 2
	resp, err := tr.Do(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: the last attempt's response, handed back rather than retried again", resp.StatusCode)
	}
	if attempts != 3 {
		t.Errorf("server saw %d attempt(s), want 3 (MaxRetries=2 retries plus the initial attempt)", attempts)
	}
}

// TestDoContextCancelledDuringBackoffReturnsPromptly pins that a cancelled
// context cuts a backoff sleep short rather than waiting it out: with the
// backoff schedule stretched to an hour, a Do that took anywhere near that
// long would mean sleepBackoff's ctx.Done() case was not actually being
// selected.
func TestDoContextCancelledDuringBackoffReturnsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	tr.RetryBase = time.Hour
	tr.RetryMax = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := tr.Do(ctx, http.MethodGet, "/", nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Do took %v to return after cancellation, want well under the hour-long backoff", elapsed)
	}
}

// The Accept header is the second thing a provider may vary (SetAuth is the
// first), and it is a field rather than something SetAuth reaches over and
// changes, so every header this Transport varies is visible on the struct.
// Empty means "application/json", which is what DeepSeek and Kimi send and
// why neither of them sets it.
func TestAcceptDefaultsToJSON(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept")
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	resp, err := tr.Do(context.Background(), http.MethodPost, "/v1/chat", []byte(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
}

// Gemini sets text/event-stream on every request, streamed or not
// (internal/gemini/client.go), so the override has to reach the wire.
func TestAcceptOverrideReachesTheWire(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept")
	}))
	defer srv.Close()

	tr := newTestTransport(srv.URL)
	tr.Accept = "text/event-stream"
	resp, err := tr.Do(context.Background(), http.MethodPost, "/v1beta/interactions", []byte(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
}
