package kimi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmptyKeyFailsBeforeSending pins the same contract internal/deepseek
// tests: a provider that returns an empty key fails the request locally with
// ErrNoAPIKey before anything is sent, so the operator sees the fix (set one
// from the settings screen or PUT /api/settings/kimi.api_key) rather than
// Kimi's 401.
func TestEmptyKeyFailsBeforeSending(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", WithAPIKeyProvider(func() (string, error) {
		return "", nil
	}))
	_, err := c.ListModels(context.Background())
	if err != ErrNoAPIKey {
		t.Fatalf("ListModels error = %v, want ErrNoAPIKey", err)
	}
	if hit {
		t.Fatal("request reached the server despite an empty key")
	}
}

// TestAPIKeyProviderCalledPerRequest pins that the provider is consulted on
// every request, so a key changed in the database is picked up without
// rebuilding the client, and that it wins over the string passed to
// NewClient.
func TestAPIKeyProviderCalledPerRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-kimi-one" {
			t.Errorf("Authorization = %q, want Bearer sk-kimi-one", got)
		}
		fmt.Fprintln(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "constructor-key", WithAPIKeyProvider(func() (string, error) {
		calls++
		return "sk-kimi-one", nil
	}))
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if calls != 1 {
		t.Fatalf("provider called %d time(s), want 1", calls)
	}
}

// TestNewClientFixedKeyStillWorks pins that NewClient's original signature
// keeps its fixed-key behaviour: the string is wrapped in a provider
// internally, so callers that never heard of providers are unchanged.
func TestNewClientFixedKeyStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fixed-key" {
			t.Errorf("Authorization = %q, want Bearer fixed-key", got)
		}
		fmt.Fprintln(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "fixed-key")
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
}
