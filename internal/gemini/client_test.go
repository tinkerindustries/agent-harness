package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestShapePinsTheDoc asserts the request body follows
// docs/gemini-3.5-flash-ui-review-prompting.md: no temperature/top_p/top_k,
// thinking_level (not thinking_budget), images first and the question last,
// and the per-image resolution on the image parts. The field names come from
// the doc's Sources pages (whats-new-gemini-3.5 and media-resolution), which
// document the /v1beta/interactions surface in snake_case.
func TestRequestShapePinsTheDoc(t *testing.T) {
	var got GenerateContentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" {
			t.Errorf("path = %q, want /v1beta/interactions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"[]"}]}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, err := c.GenerateContent(context.Background(), "gemini-3.5-flash",
		"You are reviewing a web page screenshot against its intended design.",
		"Based on the preceding screenshot, identify all visual discrepancies.",
		[]Image{
			{Data: []byte("first"), MIMEType: "image/png", Resolution: ResolutionHigh},
			{Data: []byte("second"), MIMEType: "image/webp", Resolution: ResolutionMedium},
		})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	if got.Model != "gemini-3.5-flash" {
		t.Errorf("model = %q, want gemini-3.5-flash", got.Model)
	}
	if got.GenerationConfig == nil {
		t.Fatal("generation_config missing")
	}
	if got.GenerationConfig.ThinkingLevel != ThinkingLevelMedium {
		t.Errorf("thinking_level = %q, want %q", got.GenerationConfig.ThinkingLevel, ThinkingLevelMedium)
	}
	if len(got.Input) != 3 {
		t.Fatalf("input has %d parts, want 3 (2 images + question)", len(got.Input))
	}
	if got.Input[0].Type != ContentTypeImage || got.Input[0].Resolution != ResolutionHigh {
		t.Errorf("first part = %+v, want image at high", got.Input[0])
	}
	if got.Input[1].Type != ContentTypeImage || got.Input[1].Resolution != ResolutionMedium {
		t.Errorf("second part = %+v, want image at medium", got.Input[1])
	}
	if got.Input[2].Type != ContentTypeText || got.Input[2].Text == "" {
		t.Errorf("last part = %+v, want the question last", got.Input[2])
	}
	if got.Input[0].MIMEType != "image/png" || got.Input[0].Data != base64.StdEncoding.EncodeToString([]byte("first")) {
		t.Errorf("first image bytes not base64-encoded with its mime type: %+v", got.Input[0])
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"temperature", "top_p", "top_k", "thinking_budget"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("request body carries %q; the doc forbids it for Gemini 3.x", forbidden)
		}
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "array" {
		t.Errorf("response_format = %+v, want {\"type\":\"array\"}", got.ResponseFormat)
	}
	if !strings.Contains(string(raw), `"response_format":{"type":"array"}`) {
		t.Errorf("request body does not carry top-level response_format, got: %s", raw)
	}
}

// TestResponseFormatIsTopLevelAndOptional pins two things about the field:
// it is a top-level request key, not a member of generation_config (the
// API 400s on generation_config.response_mime_type), and it is a request
// option rather than a fixed part of every request — a caller that wants
// prose simply omits it, and the serialised body then contains no
// response_format at all.
func TestResponseFormatIsTopLevelAndOptional(t *testing.T) {
	withFormat := GenerateContentRequest{
		Model:            "gemini-3.5-flash",
		Input:            []Content{{Type: ContentTypeText, Text: "hello"}},
		GenerationConfig: &GenerationConfig{ThinkingLevel: ThinkingLevelMedium},
		ResponseFormat:   &ResponseFormat{Type: "array"},
	}
	raw, err := json.Marshal(withFormat)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["response_format"]; !ok {
		t.Errorf("response_format is not a top-level request key, got: %s", raw)
	}
	var gen map[string]json.RawMessage
	if err := json.Unmarshal(decoded["generation_config"], &gen); err != nil {
		t.Fatal(err)
	}
	if _, ok := gen["response_format"]; ok {
		t.Errorf("response_format must not live inside generation_config, got: %s", raw)
	}

	withoutFormat := GenerateContentRequest{
		Model: "gemini-3.5-flash",
		Input: []Content{{Type: ContentTypeText, Text: "hello"}},
	}
	raw, err = json.Marshal(withoutFormat)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "response_format") {
		t.Errorf("an omitted ResponseFormat must not serialise, got: %s", raw)
	}
}

// TestAPIKeyRidesInHeader pins that the key is sent in the x-goog-api-key
// header and never in the URL, where it would leak into logs and error
// messages.
func TestAPIKeyRidesInHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "gk-secret" {
			t.Errorf("x-goog-api-key = %q, want gk-secret", got)
		}
		if strings.Contains(r.URL.RawQuery, "key=") || strings.Contains(r.URL.Path, "gk-secret") {
			t.Error("the API key appears in the URL")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-secret", nil }))
	if _, err := c.GenerateContent(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
}

// TestEmptyKeyFailsBeforeSending pins that a provider returning an empty key
// fails the request locally with ErrNoAPIKey before anything is sent, so the
// operator sees the fix rather than Gemini's 400.
func TestEmptyKeyFailsBeforeSending(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "", nil }))
	_, err := c.GenerateContent(context.Background(), "gemini-3.5-flash", "", "hello?", nil)
	if err != ErrNoAPIKey {
		t.Fatalf("error = %v, want ErrNoAPIKey", err)
	}
	if hit {
		t.Fatal("request reached the server despite an empty key")
	}
}

// TestGenerateContentReturnsModelText asserts the response steps are
// unwrapped into the model's text, skipping non-output steps.
func TestGenerateContentReturnsModelText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": "i-1",
			"status": "completed",
			"steps": [
				{"type": "thought", "content": [{"type": "text", "text": "thinking..."}]},
				{"type": "model_output", "content": [{"type": "text", "text": "line one"}, {"type": "text", "text": "line two"}]}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	got, err := c.GenerateContent(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if want := "line one\nline two"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// TestGenerateContentEmptyTextIsAnError pins that a response with no text
// (a safety block, say) surfaces as an error rather than an empty tool
// result the model cannot read.
func TestGenerateContentEmptyTextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i-1","status":"completed","steps":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	if _, err := c.GenerateContent(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}}); err == nil {
		t.Fatal("expected an error for a textless response, got nil")
	}
}

// TestAPIErrorSurfacesTheMessage pins that a non-2xx response is decoded
// from Gemini's {"error": {...}} envelope into a readable error.
func TestAPIErrorSurfacesTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"bad request details","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, err := c.GenerateContent(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}})
	if err == nil || !strings.Contains(err.Error(), "bad request details") {
		t.Fatalf("error = %v, want it to carry the API message", err)
	}
}
