package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
)

// reviewScreenshotClient builds a Gemini client pointed at srv, so a test
// can drive the tool end to end without reaching the real API.
func reviewScreenshotClient(srv *httptest.Server) *gemini.Client {
	return gemini.NewClient(srv.URL, gemini.WithAPIKeyProvider(func() (string, error) {
		return "gk-test", nil
	}))
}

func TestReviewScreenshotNoClientConfigured(t *testing.T) {
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "shot.png", "x")
	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"shot.png"},
		Question:   "what is wrong?",
	})
	if !res.IsError {
		t.Fatalf("expected an error result with no Gemini client, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no Gemini client configured") {
		t.Fatalf("error should name the missing client, got: %s", res.Content)
	}
}

// TestReviewScreenshotConfinesPaths asserts an image path that resolves
// outside the workspace is refused with an error result, exactly as the
// other path-taking tools do, and that a missing file names itself.
func TestReviewScreenshotConfinesPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	})))

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"../outside.png"},
		Question:   "what is wrong?",
	})
	if !res.IsError || !strings.Contains(res.Content, "escapes workspace") {
		t.Fatalf("expected an escape refusal, got: %s", res.Content)
	}

	res = runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"missing.png"},
		Question:   "what is wrong?",
	})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("expected a not-found error, got: %s", res.Content)
	}
}

// TestReviewScreenshotRefusesBadExtensions pins the PNG/JPEG/WebP allowlist:
// anything else is refused with an error naming what was rejected, and the
// refusal is an error result the model can read, not a panic.
func TestReviewScreenshotRefusesBadExtensions(t *testing.T) {
	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shot.gif", "notes.txt", "shot.PNG.bak"} {
		writeFile(t, e.Workspace, name, "not really an image")
	}
	for _, name := range []string{"shot.gif", "notes.txt", "shot.PNG.bak"} {
		res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
			ImagePaths: []string{name},
			Question:   "what is wrong?",
		})
		if !res.IsError || !strings.Contains(res.Content, "PNG, JPEG, and WebP") {
			t.Fatalf("expected an extension refusal for %s, got: %s", name, res.Content)
		}
	}
}

// TestReviewScreenshotRefusesOversizeFile pins the per-file byte cap: an
// over-limit file is refused with an error naming the actual limit, and the
// refusal happens before any request is sent.
func TestReviewScreenshotRefusesOversizeFile(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	big := make([]byte, reviewScreenshotMaxBytes+1)
	if err := os.WriteFile(filepath.Join(e.Workspace, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"big.png"},
		Question:   "what is wrong?",
	})
	want := fmt.Sprintf("over the %d-byte per-file limit", reviewScreenshotMaxBytes)
	if !res.IsError || !strings.Contains(res.Content, want) {
		t.Fatalf("expected a size refusal naming the limit %d, got: %s", reviewScreenshotMaxBytes, res.Content)
	}
	if hit {
		t.Fatal("request was sent despite an over-limit file")
	}
}

// TestReviewScreenshotRefusesTooManyImages pins the 4-image cap, named in
// the error so the model can route around it.
func TestReviewScreenshotRefusesTooManyImages(t *testing.T) {
	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 5)
	for i := range paths {
		paths[i] = "shot.png"
	}
	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: paths,
		Question:   "what is wrong?",
	})
	if !res.IsError || !strings.Contains(res.Content, "at most 4 images") {
		t.Fatalf("expected an image-count refusal, got: %s", res.Content)
	}
}

// TestReviewScreenshotSendsAndReturns pins the end-to-end shape: the request
// carries the images first (the first at high resolution, the rest at
// medium), the question last, the system instruction from the doc's prompt
// skeleton, and the model's text comes back as the tool result.
func TestReviewScreenshotSendsAndReturns(t *testing.T) {
	var got struct {
		Model             string           `json:"model"`
		SystemInstruction string           `json:"system_instruction"`
		Input             []gemini.Content `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"[{\"element\":\"nav\",\"issue\":\"overlaps\"}]"}]}]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "a.png", "first image bytes")
	writeFile(t, root, "b.png", "second image bytes")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png", "b.png"},
		Question:   "what is wrong?",
		Spec:       "the nav should be 64px tall",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	// The answer is re-marshalled indented by formatReviewAnswer (Part 2), so
	// the finding arrives with spaces after the colons.
	if !strings.Contains(res.Content, `"element": "nav"`) {
		t.Fatalf("tool result should carry the model's text, got: %s", res.Content)
	}

	if got.Model != gemini.DefaultModel {
		t.Errorf("model = %q, want %q (the google.vision_model default)", got.Model, gemini.DefaultModel)
	}
	if !strings.Contains(got.SystemInstruction, `"element"`) || !strings.Contains(got.SystemInstruction, `"issue"`) {
		t.Errorf("system instruction should ask for the element/issue/expected/actual JSON shape, got: %s", got.SystemInstruction)
	}
	if len(got.Input) != 5 {
		t.Fatalf("input has %d parts, want 5 (label + image, label + image, question)", len(got.Input))
	}
	if got.Input[0].Type != gemini.ContentTypeText || got.Input[0].Text != "Image 1: a.png" {
		t.Errorf("first part = %+v, want the label for a.png", got.Input[0])
	}
	if got.Input[1].Type != gemini.ContentTypeImage || got.Input[1].Resolution != gemini.ResolutionHigh {
		t.Errorf("second part = %+v, want first image at high", got.Input[1])
	}
	if got.Input[2].Type != gemini.ContentTypeText || got.Input[2].Text != "Image 2: b.png" {
		t.Errorf("third part = %+v, want the label for b.png", got.Input[2])
	}
	if got.Input[3].Type != gemini.ContentTypeImage || got.Input[3].Resolution != gemini.ResolutionMedium {
		t.Errorf("fourth part = %+v, want second image at medium", got.Input[3])
	}
	if got.Input[4].Type != gemini.ContentTypeText {
		t.Fatalf("last part = %+v, want the question last", got.Input[4])
	}
	if !strings.Contains(got.Input[4].Text, "what is wrong?") {
		t.Errorf("question missing from the last part: %s", got.Input[4].Text)
	}
	if !strings.Contains(got.Input[4].Text, "64px tall") {
		t.Errorf("spec should precede the question in the last part: %s", got.Input[4].Text)
	}
}

// TestReviewScreenshotLabelsAreBaseNames pins that each image's label is the
// file's base name, whatever directory the path resolves into — so a finding
// names the file a human can find in the transcript, not a directory
// position.
func TestReviewScreenshotLabelsAreBaseNames(t *testing.T) {
	var labels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []gemini.Content `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		for _, p := range req.Input {
			if p.Type == gemini.ContentTypeText && strings.HasPrefix(p.Text, "Image ") {
				labels = append(labels, p.Text)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"[]"}]}]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	sub := filepath.Join(root, "shots", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, sub, "hero-dark.png", "x")
	writeFile(t, sub, "form.png", "x")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"shots/nested/hero-dark.png", "shots/nested/form.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	want := []string{"Image 1: hero-dark.png", "Image 2: form.png"}
	if len(labels) != len(want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label %d = %q, want %q", i, labels[i], want[i])
		}
	}
}

// TestReviewScreenshotInstructionDependsOnSpec pins which standard the
// review is held to: a call carrying a spec is told the spec is the only
// standard of correctness, and a call without one is told it cannot know the
// intended design and must report only defects visible on their own terms.
// Both must say an empty list is an answer, and both must ask for the
// confidence field.
func TestReviewScreenshotInstructionDependsOnSpec(t *testing.T) {
	var instruction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SystemInstruction string `json:"system_instruction"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		instruction = req.SystemInstruction
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"[]"}]}]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "shot.png", "image bytes")

	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{name: "with spec", spec: "the rail is 244px wide", want: "The spec is the only standard of correctness."},
		{name: "without spec", spec: "", want: "No design spec was sent with it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instruction = ""
			res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
				ImagePaths: []string{"shot.png"},
				Question:   "what is wrong?",
				Spec:       tc.spec,
			})
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			if !strings.Contains(instruction, tc.want) {
				t.Errorf("instruction should contain %q, got: %s", tc.want, instruction)
			}
			if !strings.Contains(instruction, "empty list is a valid and expected answer") {
				t.Errorf("instruction should state that an empty list is an answer, got: %s", instruction)
			}
			if !strings.Contains(instruction, `"confidence"`) {
				t.Errorf("instruction should ask for a confidence per finding, got: %s", instruction)
			}
		})
	}
}

// TestReviewScreenshotModelProviderIsConsulted pins that the model comes
// from the per-call provider rather than a frozen value, so a model changed
// with harness config set google.vision_model takes effect without a
// restart.
func TestReviewScreenshotModelProviderIsConsulted(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	e.GeminiModel = func() (string, error) { return "gemini-3.6-flash", nil }
	writeFile(t, root, "a.png", "x")

	if res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png"},
		Question:   "what is wrong?",
	}); res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if gotModel != "gemini-3.6-flash" {
		t.Fatalf("model = %q, want gemini-3.6-flash from the provider", gotModel)
	}
}

// TestGeminiUsagePayloadCostsAgainstThePriceTable pins the accounting a
// ReviewScreenshot call contributes to the session: the verified usage
// sample splits to 15 uncached input + 57 completion (thinking billed at
// the output rate), and the cost comes out of the same price table
// DeepSeek's turns use, keyed by the vision model. A nil table or an
// unlisted model costs zero rather than failing the run.
func TestGeminiUsagePayloadCostsAgainstThePriceTable(t *testing.T) {
	prices := &pricing.Table{
		CapturedAt: "2026-08-10",
		Models: map[string]pricing.ModelPrices{
			"gemini-3.5-flash": {InputCacheHitPerMillionUSD: 0.15, InputCacheMissPerMillionUSD: 1.5, OutputPerMillionUSD: 9.0},
		},
	}
	usage := &gemini.Usage{
		TotalTokens:        72,
		TotalInputTokens:   15,
		TotalCachedTokens:  0,
		TotalOutputTokens:  1,
		TotalThoughtTokens: 56,
	}

	payload := geminiUsagePayload(prices, "gemini-3.5-flash", usage)
	if payload == nil {
		t.Fatal("geminiUsagePayload returned nil for a non-nil usage")
	}
	if payload.PromptTokens != 15 || payload.PromptCacheHitTokens != 0 || payload.PromptCacheMissTokens != 15 {
		t.Errorf("input split = %+v, want prompt 15, hit 0, miss 15", payload)
	}
	if payload.CompletionTokens != 57 || payload.ReasoningTokens != 56 {
		t.Errorf("completion/reasoning = %d/%d, want 57/56", payload.CompletionTokens, payload.ReasoningTokens)
	}
	want := float64(15)/1e6*1.5 + float64(57)/1e6*9.0
	if math.Abs(payload.CostUSD-want) > 1e-12 {
		t.Errorf("CostUSD = %.12f, want %.12f", payload.CostUSD, want)
	}

	if p := geminiUsagePayload(nil, "gemini-3.5-flash", usage); p.CostUSD != 0 {
		t.Errorf("CostUSD with a nil price table = %v, want 0", p.CostUSD)
	}
	if p := geminiUsagePayload(prices, "gemini-not-in-table", usage); p.CostUSD != 0 {
		t.Errorf("CostUSD for an unlisted model = %v, want 0", p.CostUSD)
	}
}

// TestReviewScreenshotResultCarriesUsage pins that a successful
// ReviewScreenshot call returns its usage on the tool result, so the runner
// can commit it as a usage event and the session cost total covers the
// Gemini call.
func TestReviewScreenshotResultCarriesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}],"usage":{"total_tokens":72,"total_input_tokens":15,"total_cached_tokens":0,"total_output_tokens":1,"total_thought_tokens":56}}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	e.Prices = &pricing.Table{
		CapturedAt: "2026-08-10",
		Models: map[string]pricing.ModelPrices{
			gemini.DefaultModel: {InputCacheHitPerMillionUSD: 0.15, InputCacheMissPerMillionUSD: 1.5, OutputPerMillionUSD: 9.0},
		},
	}
	writeFile(t, root, "a.png", "x")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.GeminiUsage == nil {
		t.Fatal("a successful ReviewScreenshot call must carry its usage on the result")
	}
	if res.GeminiUsage.CostUSD <= 0 {
		t.Errorf("expected a positive Gemini cost, got %v", res.GeminiUsage.CostUSD)
	}
	if res.GeminiUsage.SubTurn != 0 {
		t.Errorf("SubTurn = %d, want 0 (the runner stamps the sub-turn)", res.GeminiUsage.SubTurn)
	}
}

// mustParseFindings extracts the JSON array embedded in a formatted review
// result — between the leading count line and the trailing truncation note —
// and asserts it parses, which is exactly the property Part 2 is about: the
// array DeepSeek receives must never be cut mid-document again. The decoder
// reads from the first '[' and stops at the array's own closing bracket, so
// the trailing note's brackets cannot confuse it.
func mustParseFindings(t *testing.T, text string) []map[string]string {
	t.Helper()
	start := strings.Index(text, "[")
	if start < 0 {
		t.Fatalf("no JSON array in result: %q", text)
	}
	var out []map[string]string
	if err := json.NewDecoder(strings.NewReader(text[start:])).Decode(&out); err != nil {
		t.Fatalf("the JSON array in the result does not parse: %v\n%s", err, text)
	}
	return out
}

// TestFormatReviewAnswer covers the four shapes Gemini's answer can take: a
// long array that must be capped by dropping whole findings (never by cutting
// bytes), a short array, an empty array, and unparseable prose.
func TestFormatReviewAnswer(t *testing.T) {
	three := `[{"image":"a.png","element":"nav","issue":"overlaps the hero","expected":"64px","actual":"120px","confidence":"high"},{"image":"b.png","element":"footer","issue":"clipped","expected":"visible","actual":"cut","confidence":"medium"},{"image":"a.png","element":"logo","issue":"wrong colour","expected":"#123456","actual":"#654321","confidence":"low"}]`

	t.Run("long array capped by finding count", func(t *testing.T) {
		out, truncated := formatReviewAnswer(three, 260)
		if !truncated {
			t.Fatal("a long array must report truncation")
		}
		if !strings.HasPrefix(out, "3 findings, 1 high confidence\n") {
			t.Errorf("result should lead with the full answer's count line, got: %q", out)
		}
		findings := mustParseFindings(t, out)
		if len(findings) == 0 || len(findings) >= 3 {
			t.Fatalf("capped result should keep some but not all findings, got %d: %s", len(findings), out)
		}
		wantDropped := fmt.Sprintf("dropped %d of 3 findings", 3-len(findings))
		if !strings.Contains(out, wantDropped) {
			t.Errorf("result should name how many findings were dropped (%s), got: %s", wantDropped, out)
		}
		// Whatever survived the cap, each finding is still whole: the parse
		// above succeeded, and the kept ones carry their fields.
		for _, f := range findings {
			if f["image"] == "" || f["element"] == "" {
				t.Errorf("kept finding lost its fields: %v", f)
			}
		}
	})

	t.Run("short array passes through whole", func(t *testing.T) {
		short := `[{"image":"a.png","element":"nav","issue":"overlaps","confidence":"high"}]`
		out, truncated := formatReviewAnswer(short, 200_000)
		if truncated {
			t.Fatal("a short array must not be truncated")
		}
		if !strings.HasPrefix(out, "1 finding, 1 high confidence\n") {
			t.Errorf("count line = %q, want the singular form", out)
		}
		findings := mustParseFindings(t, out)
		if len(findings) != 1 || findings[0]["image"] != "a.png" || findings[0]["confidence"] != "high" {
			t.Errorf("findings = %v, want the one finding intact", findings)
		}
	})

	t.Run("empty array stays a valid answer", func(t *testing.T) {
		out, truncated := formatReviewAnswer(`[]`, 200_000)
		if truncated {
			t.Fatal("an empty array must not report truncation")
		}
		if !strings.HasPrefix(out, "0 findings, 0 high confidence\n") {
			t.Errorf("count line = %q, want 0 findings", out)
		}
		if find := mustParseFindings(t, out); len(find) != 0 {
			t.Errorf("findings = %v, want none", find)
		}
		if strings.Contains(out, "truncated") {
			t.Errorf("empty array must not carry a truncation note: %s", out)
		}
	})

	t.Run("unparseable prose is labelled", func(t *testing.T) {
		prose := "The header overlaps the hero image on narrow screens."
		out, truncated := formatReviewAnswer(prose, 200_000)
		if truncated {
			t.Fatal("short prose must not report truncation")
		}
		if !strings.HasPrefix(out, "Gemini's answer was not a JSON list; unparsed text follows:") {
			t.Errorf("prose must be labelled as unparsed, got: %q", out)
		}
		if !strings.Contains(out, prose) {
			t.Errorf("the raw prose should still reach the model: %s", out)
		}
	})

	t.Run("prose over the cap is cut with the label", func(t *testing.T) {
		prose := strings.Repeat("the header overlaps the hero. ", 50)
		out, truncated := formatReviewAnswer(prose, 120)
		if !truncated {
			t.Fatal("long prose must report truncation")
		}
		if !strings.HasPrefix(out, "Gemini's answer was not a JSON list; unparsed text follows:") {
			t.Errorf("prose must be labelled as unparsed, got: %q", out)
		}
		if !strings.Contains(out, "[truncated:") {
			t.Errorf("the byte cap on prose must stay labelled: %s", out)
		}
	})
}

// TestReviewScreenshotFormatsTheAnswer drives the formatting end to end
// through the tool: the Gemini server returns a raw compact array, and the
// tool result must come back as the count line plus an indented array that
// still parses, with the truncated flag set when the output cap bites.
func TestReviewScreenshotFormatsTheAnswer(t *testing.T) {
	answer := `[{"image":"a.png","element":"nav","issue":"overlaps","confidence":"high"},{"image":"b.png","element":"footer","issue":"clipped","confidence":"medium"}]`
	quoted, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"i","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":` + string(quoted) + `}]}]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	// A small output cap forces the drop-by-count path while still leaving
	// room for one whole finding plus the count line and the drop note.
	e.OutputCap = 200
	writeFile(t, root, "a.png", "x")
	writeFile(t, root, "b.png", "x")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png", "b.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !res.Truncated {
		t.Fatal("a capped answer must set the truncated flag")
	}
	if !strings.HasPrefix(res.Content, "2 findings, 1 high confidence\n") {
		t.Errorf("result should lead with the count line, got: %q", res.Content)
	}
	if find := mustParseFindings(t, res.Content); len(find) == 0 || len(find) >= 2 {
		t.Errorf("capped result should keep some but not all findings, got: %s", res.Content)
	}
}
