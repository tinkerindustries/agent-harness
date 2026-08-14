package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
)

// TestHandleListModels pins GET /api/models to the one model table the
// harness actually runs on (internal/provider): the endpoint answers exactly
// provider.KnownModels(), so the browser's dropdowns can never disagree with
// what a work request validates against — the same invariant
// TestPromptNamesExactlyTheToolArray holds for the system prompt's tool
// inventory. kimi-k3 is asserted by name because it is the model this
// endpoint was added for (docs/KIMI-INTEGRATION.md §4.3): the start forms
// could not run it from the browser before the dropdowns were fed from here.
func TestHandleListModels(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/models")
	if err != nil {
		t.Fatalf("GET /api/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/models status = %d, want 200", resp.StatusCode)
	}

	var body modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /api/models body: %v", err)
	}
	if !reflect.DeepEqual(body.Models, provider.KnownModels()) {
		t.Errorf("models = %v, want provider.KnownModels() = %v", body.Models, provider.KnownModels())
	}
	if !provider.Known("kimi-k3") {
		t.Fatalf("kimi-k3 must be in the provider table for this test to mean anything")
	}
}

// TestHandleListModelsMethodGate pins the endpoint as read-only: the method
// gate (docs/DESIGN.md §4.2) allows GET and HEAD on every path, and a write
// to /api/models is a 405 with an Allow header naming GET and HEAD — a model
// list is data the harness owns, not a resource a client mutates.
func TestHandleListModelsMethodGate(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/api/models", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/models status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

// TestHandleListModelsHead pins that the endpoint answers HEAD like GET, the
// gate's promise for every read path (a health probe can HEAD it).
func TestHandleListModelsHead(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodHead, srv.URL+"/api/models", nil)
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /api/models status = %d, want 200", rec.Code)
	}
}
