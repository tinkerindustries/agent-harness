package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
)

// TestInteractHeadersUnchangedAfterTransportMigration is the evidence for
// moving Interact onto chatTransport.Do (docs/GEMINI-INTEGRATION.md §8):
// TestRequestShapePinsTheDoc already pins the body bytes (transport.Do
// forwards the []byte json.Marshal produced untouched, so nothing about the
// migration could change them), and TestAPIKeyRidesInHeader already pins
// x-goog-api-key. What neither test checked before is the full header set
// newRequest used to build directly — this asserts all four together so a
// future change to chatTransport's SetAuth or default headers cannot drift
// Interact's wire behaviour without a test noticing.
func TestInteractHeadersUnchangedAfterTransportMigration(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotGoogKey, gotAccept, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotGoogKey = r.Header.Get("x-goog-api-key")
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"ok"}}}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-secret", nil }))
	if _, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "system", "question", nil); err != nil {
		t.Fatalf("Interact: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1beta/interactions" {
		t.Errorf("path = %q, want /v1beta/interactions", gotPath)
	}
	if gotGoogKey != "gk-secret" {
		t.Errorf("x-goog-api-key = %q, want gk-secret", gotGoogKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty: Interact never sent Bearer and still must not", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream (newRequest's own header, preserved by chatTransport's SetAuth)", gotAccept)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}
