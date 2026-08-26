package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/gemini/geminitest"
)

// TestRequestShapePinsTheDoc asserts the request body follows
// docs/gemini-3.5-flash-ui-review-prompting.md: no temperature/top_p/top_k,
// thinking_level (not thinking_budget), images first and the question last,
// and the per-image resolution on the image parts. The field names come from
// the doc's Sources pages (whats-new-gemini-3.5 and media-resolution), which
// document the /v1beta/interactions surface in snake_case.
func TestRequestShapePinsTheDoc(t *testing.T) {
	var got InteractionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" {
			t.Errorf("path = %q, want /v1beta/interactions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"[]"}}}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, _, err := c.Interact(context.Background(), "gemini-3.5-flash",
		"You are reviewing a web page screenshot against its intended design.",
		"Based on the preceding screenshot, identify all visual discrepancies.",
		[]Image{
			{Data: []byte("first"), MIMEType: "image/png", Resolution: ResolutionHigh, Label: "home-dark.png"},
			{Data: []byte("second"), MIMEType: "image/webp", Resolution: ResolutionMedium, Label: "home-light.png"},
		})
	if err != nil {
		t.Fatalf("Interact: %v", err)
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
	if len(got.Input) != 5 {
		t.Fatalf("input has %d parts, want 5 (2 labels + 2 images + question)", len(got.Input))
	}
	if got.Input[0].Type != ContentTypeText || got.Input[0].Text != "Image 1: home-dark.png" {
		t.Errorf("first part = %+v, want the label text part for the first image", got.Input[0])
	}
	if got.Input[1].Type != ContentTypeImage || got.Input[1].Resolution != ResolutionHigh {
		t.Errorf("second part = %+v, want image at high", got.Input[1])
	}
	if got.Input[2].Type != ContentTypeText || got.Input[2].Text != "Image 2: home-light.png" {
		t.Errorf("third part = %+v, want the label text part for the second image", got.Input[2])
	}
	if got.Input[3].Type != ContentTypeImage || got.Input[3].Resolution != ResolutionMedium {
		t.Errorf("fourth part = %+v, want image at medium", got.Input[3])
	}
	if got.Input[4].Type != ContentTypeText || got.Input[4].Text == "" {
		t.Errorf("last part = %+v, want the question last", got.Input[4])
	}
	if got.Input[1].MIMEType != "image/png" || got.Input[1].Data != base64.StdEncoding.EncodeToString([]byte("first")) {
		t.Errorf("first image bytes not base64-encoded with its mime type: %+v", got.Input[1])
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

// TestInteractLabelsPrecedeImages asserts the label interleaving order: a
// text part carrying "Image N: <label>" sits immediately before each image
// part, in the order the images were given, so the model can name the
// screenshot a finding is about. An image without a label emits no label
// part, so a caller that has nothing to name does not pay for one.
func TestInteractLabelsPrecedeImages(t *testing.T) {
	var got InteractionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"[]"}}}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "question?",
		[]Image{
			{Data: []byte("a"), MIMEType: "image/png", Resolution: ResolutionHigh, Label: "a.png"},
			{Data: []byte("b"), MIMEType: "image/png", Resolution: ResolutionMedium, Label: "b.png"},
			{Data: []byte("c"), MIMEType: "image/png", Resolution: ResolutionMedium},
		})
	if err != nil {
		t.Fatalf("Interact: %v", err)
	}

	var parts []string
	for _, p := range got.Input {
		if p.Type == ContentTypeText {
			parts = append(parts, p.Text)
		}
	}
	want := []string{"Image 1: a.png", "Image 2: b.png", "question?"}
	if len(parts) != len(want) {
		t.Fatalf("text parts = %q, want %q", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("text part %d = %q, want %q", i, parts[i], want[i])
		}
	}

	// The label parts sit immediately before their images, in order: for each
	// of the two labelled images, the part before it must be the matching
	// label; the unlabelled third image emits none.
	imageIdx := 0
	for i, p := range got.Input {
		if p.Type != ContentTypeImage {
			continue
		}
		imageIdx++
		if imageIdx <= 2 {
			if i == 0 || got.Input[i-1].Type != ContentTypeText {
				t.Errorf("image part %d has no label text part immediately before it", i)
			}
		}
	}
	if imageIdx != 3 {
		t.Errorf("image parts = %d, want 3", imageIdx)
	}
}

// TestResponseFormatIsTopLevelAndOptional pins two things about the field:
// it is a top-level request key, not a member of generation_config (the
// API 400s on generation_config.response_mime_type), and it is a request
// option rather than a fixed part of every request — a caller that wants
// prose simply omits it, and the serialised body then contains no
// response_format at all.
func TestResponseFormatIsTopLevelAndOptional(t *testing.T) {
	withFormat := InteractionRequest{
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

	withoutFormat := InteractionRequest{
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
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"ok"}}}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-secret", nil }))
	if _, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}}); err != nil {
		t.Fatalf("Interact: %v", err)
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
	_, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", nil)
	if err != ErrNoAPIKey {
		t.Fatalf("error = %v, want ErrNoAPIKey", err)
	}
	if hit {
		t.Fatal("request reached the server despite an empty key")
	}
}

// TestInteractReturnsModelText asserts the response steps are
// unwrapped into the model's text, skipping non-output steps.
func TestInteractReturnsModelText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{
			{Type: "thought", Summaries: []string{"thinking..."}},
			{Type: "model_output", Texts: []string{"line one, ", "line two"}},
		}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	got, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}})
	if err != nil {
		t.Fatalf("Interact: %v", err)
	}
	if want := "line one, line two"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// TestInteractEmptyTextIsAnError pins that a response with no text
// (a safety block, say) surfaces as an error rather than an empty tool
// result the model cannot read.
func TestInteractEmptyTextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream(nil, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	if _, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}}); err == nil {
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
	_, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}})
	if err == nil || !strings.Contains(err.Error(), "bad request details") {
		t.Fatalf("error = %v, want it to carry the API message", err)
	}
}

// TestInteractReturnsParsedUsage pins that the usage block of a real
// interactions response is decoded, not dropped: the exact verified shape
// from the follow-up brief, so cost accounting downstream sees the true
// figures.
func TestInteractReturnsParsedUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"[{}]"}}}, `{
			"total_tokens": 72,
			"total_input_tokens": 15,
			"input_tokens_by_modality": [{"modality": "text", "tokens": 15}],
			"total_cached_tokens": 0,
			"total_output_tokens": 1,
			"total_tool_use_tokens": 0,
			"total_thought_tokens": 56,
			"raw_prompt_token": 50
		}`)))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, usage, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", []Image{{Data: []byte("x"), MIMEType: "image/png", Resolution: ResolutionHigh}})
	if err != nil {
		t.Fatalf("Interact: %v", err)
	}
	if usage == nil {
		t.Fatal("usage is nil; the response's usage block was not parsed")
	}
	if usage.TotalTokens != 72 || usage.TotalInputTokens != 15 || usage.TotalCachedTokens != 0 ||
		usage.TotalOutputTokens != 1 || usage.TotalThoughtTokens != 56 || usage.RawPromptToken != 50 {
		t.Errorf("usage = %+v, want the verified sample figures", usage)
	}
	if len(usage.InputTokensByModality) != 1 || usage.InputTokensByModality[0].Modality != "text" || usage.InputTokensByModality[0].Tokens != 15 {
		t.Errorf("input_tokens_by_modality = %+v, want [{text 15}]", usage.InputTokensByModality)
	}
}

// TestUsageTokenSplit pins the two mapping decisions in Usage.TokenSplit
// against the verified sample: total_tokens 72 = input 15 + output 1 +
// thought 56, thinking bills at the output rate, and cached tokens are a
// subset of input (here trivially: cached 0, so uncached input is the whole
// 15).
func TestUsageTokenSplit(t *testing.T) {
	u := &Usage{
		TotalTokens:        72,
		TotalInputTokens:   15,
		TotalCachedTokens:  0,
		TotalOutputTokens:  1,
		TotalThoughtTokens: 56,
	}
	cacheHit, cacheMiss, completion, reasoning := u.TokenSplit()
	if cacheHit != 0 || cacheMiss != 15 {
		t.Errorf("input split = hit %d, miss %d; want 0, 15", cacheHit, cacheMiss)
	}
	if completion != 57 {
		t.Errorf("completion = %d, want 57 (output 1 + thought 56, billed at the output rate)", completion)
	}
	if reasoning != 56 {
		t.Errorf("reasoning = %d, want 56", reasoning)
	}

	// A cached subset of input: 100 input, 40 cached → 60 uncached, and the
	// difference must never go negative on a malformed report.
	u2 := &Usage{TotalInputTokens: 100, TotalCachedTokens: 40, TotalOutputTokens: 5, TotalThoughtTokens: 2}
	hit, miss, comp, _ := u2.TokenSplit()
	if hit != 40 || miss != 60 || comp != 7 {
		t.Errorf("split = hit %d, miss %d, completion %d; want 40, 60, 7", hit, miss, comp)
	}
	u3 := &Usage{TotalInputTokens: 10, TotalCachedTokens: 50}
	if _, miss, _, _ := u3.TokenSplit(); miss != 0 {
		t.Errorf("cached tokens larger than input must clamp the miss to 0, got %d", miss)
	}
}

// TestNoResponseHeaderTimeout pins the transport setting that a review call
// depends on. Interactions are not streamed, so the response headers arrive
// only once the model has finished thinking; a 30s ResponseHeaderTimeout cut
// off six multi-image reviews in one production session
// (docs/reviews/sess-8df2a5f78847c4737b0e86e6e5d069b6.md) and made
// tools.reviewscreenshot_timeout unable to raise the real deadline.
func TestNoResponseHeaderTimeout(t *testing.T) {
	tr, ok := NewClient("").httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", NewClient("").httpClient.Transport)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0: the caller's context is the call's deadline", tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout = 0: connection setup must stay bounded")
	}
}

// TestContextBoundsTheCall proves the deadline the transport no longer
// supplies still exists: a server that never answers ends the call when the
// caller's context expires, rather than hanging.
func TestContextBoundsTheCall(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	_, _, err := c.Interact(ctx, "gemini-3.5-flash", "system", "question", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Interact error = %v, want context.DeadlineExceeded", err)
	}
}
