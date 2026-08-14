package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
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
		w.Write([]byte(geminitest.Answer("ok", "")))
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

// A relative path that names nothing is tried again under scratch/, which is
// where Screenshot puts a relative capture. The pairing is the point: the
// model writes "after/01.png", the bytes land in scratch/after/01.png, and it
// hands that same string to the review it makes next — so the read has to
// resolve the way the write did or the round trip costs a sub-turn per image
// (workspace.go resolveImagePath).
func TestReviewScreenshotReadsARelativePathFromScratch(t *testing.T) {
	var got struct {
		Input []gemini.Content `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[]", "")))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	want := testPNGBytes(t, 40, 30)
	if err := os.MkdirAll(filepath.Join(root, "scratch", "after"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch", "after", "01.png"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"after/01.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	var received []byte
	for _, p := range got.Input {
		if p.Type == gemini.ContentTypeImage {
			raw, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				t.Fatalf("received image is not valid base64: %v", err)
			}
			received = raw
		}
	}
	if !bytes.Equal(received, want) {
		t.Fatalf("server received %d bytes, want the %d bytes of scratch/after/01.png", len(received), len(want))
	}
}

// The fallback never shadows a file that is really there: a path that resolves
// to an existing file is sent as named, even when a same-named file also sits
// under scratch/.
func TestReviewScreenshotPrefersTheNamedPathOverScratch(t *testing.T) {
	var got struct {
		Input []gemini.Content `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[]", "")))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	named := testPNGBytes(t, 40, 30)
	decoy := testPNGBytes(t, 12, 8)
	for _, dir := range []string{filepath.Join(root, "shots"), filepath.Join(root, "scratch", "shots")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "shots", "a.png"), named, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch", "shots", "a.png"), decoy, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"shots/a.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, p := range got.Input {
		if p.Type != gemini.ContentTypeImage {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			t.Fatalf("received image is not valid base64: %v", err)
		}
		if !bytes.Equal(raw, named) {
			t.Fatalf("the scratch copy shadowed the named file: got %d bytes, want %d", len(raw), len(named))
		}
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
// over-limit file that cannot be decoded is refused with an error naming the
// actual limit and the reason downscaling was not available, and the refusal
// happens before any request is sent.
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
	if !strings.Contains(res.Content, "could not be downscaled") {
		t.Fatalf("the refusal should name why downscaling was not available, got: %s", res.Content)
	}
	if hit {
		t.Fatal("request was sent despite an over-limit file")
	}
}

// testPNGBytes renders a w×h RGBA image of deterministic noise and encodes
// it as PNG. Random pixels compress poorly, so a large one is a reliably
// over-cap file and a small one is a reliably decodable under-cap file.
func testPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(42))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Intn(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReviewScreenshotDownscalesOversizeFile pins the route around the byte
// cap: an over-cap file that decodes is shrunk preserving aspect ratio until
// the re-encoded bytes fit, that shrunk image is what Gemini receives (under
// the cap), and the result text tells the model it was downscaled and to
// what dimensions.
func TestReviewScreenshotDownscalesOversizeFile(t *testing.T) {
	var got struct {
		Input []gemini.Content `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[]", "")))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	// 2400×2400 of random pixels encodes to ~23 MB — far over the 5 MB cap —
	// and no amount of compression will fit that, so the file must be shrunk.
	big := testPNGBytes(t, 2400, 2400)
	if len(big) <= reviewScreenshotMaxBytes {
		t.Fatalf("test image is %d bytes, want it over the %d-byte cap", len(big), reviewScreenshotMaxBytes)
	}
	if err := os.WriteFile(filepath.Join(root, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"big.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("an over-cap file that decodes must be downscaled, not refused: %s", res.Content)
	}

	// The result says the file was downscaled and names the dimensions.
	const marker = "big.png was downscaled to "
	idx := strings.Index(res.Content, marker)
	if idx < 0 {
		t.Fatalf("result should say the image was downscaled, got: %s", res.Content)
	}
	dims, _, _ := strings.Cut(res.Content[idx+len(marker):], " ")
	var w, h int
	if _, err := fmt.Sscanf(dims, "%dx%d", &w, &h); err != nil {
		t.Fatalf("the downscale note should carry dimensions, got %q: %v", dims, err)
	}
	if w >= 2400 || h >= 2400 || w < 1 || h < 1 {
		t.Fatalf("downscaled dimensions %dx%d should be smaller than the 2400x2400 original", w, h)
	}

	// What Gemini received is the shrunk image: under the cap, smaller than
	// the original, and decoding to exactly the dimensions the note names.
	var received []byte
	for _, p := range got.Input {
		if p.Type == gemini.ContentTypeImage {
			raw, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				t.Fatalf("received image is not valid base64: %v", err)
			}
			received = raw
		}
	}
	if len(received) == 0 {
		t.Fatal("no image reached the server")
	}
	if len(received) > reviewScreenshotMaxBytes {
		t.Fatalf("server received %d bytes, want at most the %d-byte cap", len(received), reviewScreenshotMaxBytes)
	}
	if len(received) >= len(big) {
		t.Fatalf("server received %d bytes, want fewer than the original %d", len(received), len(big))
	}
	decoded, _, err := image.Decode(bytes.NewReader(received))
	if err != nil {
		t.Fatalf("received image does not decode: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("received image is %dx%d, want the %dx%d the note names", b.Dx(), b.Dy(), w, h)
	}
}

// TestReviewScreenshotSendsUnderCapFileByteIdentically pins that a file
// already under the cap is never decoded and re-encoded: Gemini receives the
// file's bytes exactly, and the result says nothing about downscaling.
func TestReviewScreenshotSendsUnderCapFileByteIdentically(t *testing.T) {
	var got struct {
		Input []gemini.Content `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[]", "")))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	small := testPNGBytes(t, 32, 24)
	if err := os.WriteFile(filepath.Join(root, "small.png"), small, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"small.png"},
		Question:   "what is wrong?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "downscaled") {
		t.Fatalf("an under-cap file must not be reported as downscaled, got: %s", res.Content)
	}
	var received []byte
	for _, p := range got.Input {
		if p.Type == gemini.ContentTypeImage {
			raw, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				t.Fatalf("received image is not valid base64: %v", err)
			}
			received = raw
		}
	}
	if len(received) == 0 {
		t.Fatal("no image reached the server")
	}
	if !bytes.Equal(received, small) {
		t.Fatalf("server received %d bytes, want the original %d byte-for-byte", len(received), len(small))
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
		w.Write([]byte(geminitest.Answer("[{\"element\":\"nav\",\"issue\":\"overlaps\"}]", "")))
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
		w.Write([]byte(geminitest.Answer("[]", "")))
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
		w.Write([]byte(geminitest.Answer("[]", "")))
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
			if !strings.Contains(instruction, `An empty "findings" list is a valid and expected answer`) {
				t.Errorf("instruction should state that no findings is an answer, got: %s", instruction)
			}
			if !strings.Contains(instruction, `"confidence"`) {
				t.Errorf("instruction should ask for a confidence per finding, got: %s", instruction)
			}
			// The description is what makes an empty findings list checkable
			// by a reader who cannot open the image, so both review
			// instructions must demand it and must say what to do with a
			// capture that did not render
			// (docs/reviews/vision-path-2026-08-14.md).
			if !strings.Contains(instruction, `"observed" is required and is never empty`) {
				t.Errorf("instruction should require a description of what was seen, got: %s", instruction)
			}
			if !strings.Contains(instruction, "failed to render") {
				t.Errorf("instruction should tell the model to say so when an image did not render, got: %s", instruction)
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
		w.Write([]byte(geminitest.Answer("ok", "")))
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
		w.Write([]byte(geminitest.Answer("ok", `{"total_tokens":72,"total_input_tokens":15,"total_cached_tokens":0,"total_output_tokens":1,"total_thought_tokens":56}`)))
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

// TestFormatReviewAnswer covers the shapes Gemini's answer can take: the
// object all three instructions now ask for, a long one that must be capped
// by dropping whole findings (never by cutting bytes), an empty findings list
// (which is the case the "observed" line exists for), describe mode's
// elements, the bare array that predates "observed", and unparseable prose.
func TestFormatReviewAnswer(t *testing.T) {
	const observed = "A dashboard with a dark header, a six-row table, and a footer."
	three := `{"observed":"` + observed + `","findings":[{"image":"a.png","element":"nav","issue":"overlaps the hero","expected":"64px","actual":"120px","confidence":"high"},{"image":"b.png","element":"footer","issue":"clipped","expected":"visible","actual":"cut","confidence":"medium"},{"image":"a.png","element":"logo","issue":"wrong colour","expected":"#123456","actual":"#654321","confidence":"low"}]}`

	t.Run("long answer capped by finding count", func(t *testing.T) {
		out, truncated := formatReviewAnswer(three, reviewModeReview, 400)
		if !truncated {
			t.Fatal("a long answer must report truncation")
		}
		// The description survives the cap: it is the evidence the count is
		// read against, so dropping it to fit findings would defeat the point.
		if !strings.HasPrefix(out, "Observed: "+observed+"\n\n3 findings, 1 high confidence\n") {
			t.Errorf("result should lead with the description then the full answer's count, got: %q", out)
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

	t.Run("short answer passes through whole", func(t *testing.T) {
		short := `{"observed":"` + observed + `","findings":[{"image":"a.png","element":"nav","issue":"overlaps","confidence":"high"}]}`
		out, truncated := formatReviewAnswer(short, reviewModeReview, 200_000)
		if truncated {
			t.Fatal("a short answer must not be truncated")
		}
		if !strings.Contains(out, "1 finding, 1 high confidence\n") {
			t.Errorf("count line = %q, want the singular form", out)
		}
		findings := mustParseFindings(t, out)
		if len(findings) != 1 || findings[0]["image"] != "a.png" || findings[0]["confidence"] != "high" {
			t.Errorf("findings = %v, want the one finding intact", findings)
		}
	})

	// The case the whole shape exists for. A bare "0 findings" reads
	// identically to a blank page, so the description has to be there and has
	// to be the first thing in the result
	// (docs/reviews/vision-path-2026-08-14.md).
	t.Run("no findings still carries what was seen", func(t *testing.T) {
		out, truncated := formatReviewAnswer(`{"observed":"`+observed+`","findings":[]}`, reviewModeReview, 200_000)
		if truncated {
			t.Fatal("an empty findings list must not report truncation")
		}
		if !strings.HasPrefix(out, "Observed: "+observed) {
			t.Fatalf("a clean review must lead with what was seen, got: %q", out)
		}
		if !strings.Contains(out, "0 findings, 0 high confidence\n") {
			t.Errorf("count line = %q, want 0 findings", out)
		}
		if find := mustParseFindings(t, out); len(find) != 0 {
			t.Errorf("findings = %v, want none", find)
		}
		if strings.Contains(out, "truncated") {
			t.Errorf("an empty findings list must not carry a truncation note: %s", out)
		}
	})

	// A blank capture is the failure the description is there to catch, and it
	// has to arrive as words rather than as an empty list.
	t.Run("a blank page says so in words", func(t *testing.T) {
		out, _ := formatReviewAnswer(`{"observed":"Image 1 is entirely white; nothing rendered.","findings":[]}`, reviewModeReview, 200_000)
		if !strings.Contains(out, "nothing rendered") {
			t.Errorf("a blank capture must reach the model as prose, got: %q", out)
		}
	})

	t.Run("describe mode counts elements and quotes no confidence", func(t *testing.T) {
		answer := `{"observed":"` + observed + `","elements":[{"image":"a.png","text":"Sessions","role":"heading","styling":"bold, 24px"},{"image":"a.png","text":"12","role":"badge","styling":"dim"}]}`
		out, truncated := formatReviewAnswer(answer, reviewModeDescribe, 200_000)
		if truncated {
			t.Fatal("a short answer must not be truncated")
		}
		if !strings.Contains(out, "2 elements\n") {
			t.Errorf("describe mode should count elements, got: %q", out)
		}
		if strings.Contains(out, "high confidence") {
			t.Errorf("describe mode reports no confidence — its elements carry none: %q", out)
		}
	})

	// The shape from before "observed" existed. Still legible, so it is still
	// read — but the reader is told the corroboration is missing rather than
	// being left to assume it was there.
	t.Run("a bare array is read and flagged as uncorroborated", func(t *testing.T) {
		out, truncated := formatReviewAnswer(`[{"image":"a.png","element":"nav","issue":"overlaps","confidence":"high"}]`, reviewModeReview, 200_000)
		if truncated {
			t.Fatal("a short array must not be truncated")
		}
		if !strings.Contains(out, "no description of what it saw") {
			t.Errorf("a missing description must be called out, got: %q", out)
		}
		if find := mustParseFindings(t, out); len(find) != 1 {
			t.Errorf("the findings must still reach the model: %v", find)
		}
	})

	// The model wraps its answer in a fence when nothing constrains the
	// container, and nothing does — see the mode/instruction notes above and
	// docs/gemini-3.5-flash-ui-review-prompting.md.
	t.Run("a fenced answer is unwrapped", func(t *testing.T) {
		fenced := "```json\n{\"observed\":\"" + observed + "\",\"findings\":[]}\n```"
		out, _ := formatReviewAnswer(fenced, reviewModeReview, 200_000)
		if !strings.HasPrefix(out, "Observed: "+observed) {
			t.Errorf("a fenced object must be read like a bare one, got: %q", out)
		}
		if strings.Contains(out, "```") {
			t.Errorf("the fence must not reach the model: %q", out)
		}
	})

	// A nil slice marshals to the literal "null", which reads as a broken
	// answer rather than an empty one. Seen in a live run before it was
	// fixed, on an answer that carried neither key.
	t.Run("a missing list renders as empty, never null", func(t *testing.T) {
		out, _ := formatReviewAnswer(`{"observed":"`+observed+`"}`, reviewModeDescribe, 200_000)
		if strings.Contains(out, "null") {
			t.Errorf("an absent list must not render as null, got: %q", out)
		}
		if !strings.Contains(out, "0 elements\n[]") {
			t.Errorf("an absent list should render as an empty list, got: %q", out)
		}
	})

	t.Run("unparseable prose is labelled", func(t *testing.T) {
		prose := "The header overlaps the hero image on narrow screens."
		out, truncated := formatReviewAnswer(prose, reviewModeReview, 200_000)
		if truncated {
			t.Fatal("short prose must not report truncation")
		}
		if !strings.HasPrefix(out, "Gemini's answer was not in the expected JSON shape; unparsed text follows:") {
			t.Errorf("prose must be labelled as unparsed, got: %q", out)
		}
		if !strings.Contains(out, prose) {
			t.Errorf("the raw prose should still reach the model: %s", out)
		}
	})

	t.Run("prose over the cap is cut with the label", func(t *testing.T) {
		prose := strings.Repeat("the header overlaps the hero. ", 50)
		out, truncated := formatReviewAnswer(prose, reviewModeReview, 120)
		if !truncated {
			t.Fatal("long prose must report truncation")
		}
		if !strings.HasPrefix(out, "Gemini's answer was not in the expected JSON shape; unparsed text follows:") {
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
	answer := `{"observed":"Two pages.","findings":[{"image":"a.png","element":"nav","issue":"overlaps","confidence":"high"},{"image":"b.png","element":"footer","issue":"clipped","confidence":"medium"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer(answer, "")))
	}))
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	// A small output cap forces the drop-by-count path while still leaving
	// room for the description, one whole finding, the count line and the
	// drop note.
	e.OutputCap = 250
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
	if !strings.HasPrefix(res.Content, "conversation_id: rvw-") {
		t.Errorf("result should carry the conversation id first, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Observed: Two pages.\n\n2 findings, 1 high confidence\n") {
		t.Errorf("result should carry the description then the count line, got: %q", res.Content)
	}
	if find := mustParseFindings(t, res.Content); len(find) == 0 || len(find) >= 2 {
		t.Errorf("capped result should keep some but not all findings, got: %s", res.Content)
	}
}

// reviewScreenshotRequests is the shape every capture below decodes the
// Gemini request body into.
type reviewScreenshotRequests []struct {
	Input []gemini.Content `json:"input"`
}

// reviewConversationServer stands in for Gemini across a conversation,
// recording every request body and answering [] each time.
func reviewConversationServer(t *testing.T, requests *reviewScreenshotRequests) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []gemini.Content `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*requests = append(*requests, req)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[]", "")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReviewScreenshotConversationFollowUp drives a first call and then a
// follow-up against the test Gemini server, asserting the second request
// carries the images again, the first question, the first answer, and the
// new question — the follow-up is one review with the thread re-sent, not a
// fresh review from scratch.
func TestReviewScreenshotConversationFollowUp(t *testing.T) {
	var requests reviewScreenshotRequests
	srv := reviewConversationServer(t, &requests)

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "a.png", "first image bytes")
	writeFile(t, root, "b.png", "second image bytes")

	first := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png", "b.png"},
		Question:   "what is wrong?",
		Spec:       "the nav is 64px tall",
	})
	if first.IsError {
		t.Fatalf("first call failed: %s", first.Content)
	}
	firstLine, _, _ := strings.Cut(first.Content, "\n")
	if !strings.HasPrefix(firstLine, "conversation_id: rvw-") {
		t.Fatalf("first result should carry a conversation id, got: %q", firstLine)
	}
	id := strings.TrimPrefix(firstLine, "conversation_id: ")

	second := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		Question:       "look closer at the header",
	})
	if second.IsError {
		t.Fatalf("follow-up failed: %s", second.Content)
	}
	if !strings.HasPrefix(second.Content, "conversation_id: "+id+"\n") {
		t.Errorf("follow-up result should echo the conversation id, got: %q", second.Content)
	}

	if len(requests) != 2 {
		t.Fatalf("Gemini saw %d requests, want 2", len(requests))
	}

	// The second request carries the images again, byte for byte.
	var secondImages []gemini.Content
	for _, p := range requests[1].Input {
		if p.Type == gemini.ContentTypeImage {
			secondImages = append(secondImages, p)
		}
	}
	if len(secondImages) != 2 {
		t.Fatalf("second request has %d image parts, want 2 (the images again)", len(secondImages))
	}
	if secondImages[0].Data != base64.StdEncoding.EncodeToString([]byte("first image bytes")) ||
		secondImages[1].Data != base64.StdEncoding.EncodeToString([]byte("second image bytes")) {
		t.Errorf("second request's images differ from the first call's files: %+v", secondImages)
	}

	// The second request's last text part carries the first question, the
	// first answer (the raw [] from the first response), the spec, and the
	// new question last.
	last := requests[1].Input[len(requests[1].Input)-1]
	if last.Type != gemini.ContentTypeText {
		t.Fatalf("second request's last part = %+v, want the composed question text", last)
	}
	for _, want := range []string{
		"Design spec / target CSS:", "the nav is 64px tall",
		"Question: what is wrong?", "Answer: []",
		"look closer at the header",
	} {
		if !strings.Contains(last.Text, want) {
			t.Errorf("follow-up question should carry %q, got: %s", want, last.Text)
		}
	}
	if !strings.HasSuffix(last.Text, "look closer at the header") {
		t.Errorf("the new question should come last, got: %s", last.Text)
	}

	// The labels still introduce the re-sent images.
	var labels []string
	for _, p := range requests[1].Input {
		if p.Type == gemini.ContentTypeText && strings.HasPrefix(p.Text, "Image ") {
			labels = append(labels, p.Text)
		}
	}
	if len(labels) != 2 || labels[0] != "Image 1: a.png" || labels[1] != "Image 2: b.png" {
		t.Errorf("follow-up labels = %q, want the base names again", labels)
	}
}

// TestReviewScreenshotFollowUpMissingFile pins the re-read-from-disk
// contract: a follow-up does not hold the image bytes, so a file deleted
// since the conversation started fails with an ordinary error result naming
// the missing path.
func TestReviewScreenshotFollowUpMissingFile(t *testing.T) {
	var requests reviewScreenshotRequests
	srv := reviewConversationServer(t, &requests)

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "a.png", "first image bytes")

	first := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png"},
		Question:   "what is wrong?",
	})
	if first.IsError {
		t.Fatalf("first call failed: %s", first.Content)
	}
	firstLine, _, _ := strings.Cut(first.Content, "\n")
	id := strings.TrimPrefix(firstLine, "conversation_id: ")

	if err := os.Remove(filepath.Join(root, "a.png")); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		Question:       "look again",
	})
	if !res.IsError {
		t.Fatalf("follow-up on a deleted file should fail, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.png") || !strings.Contains(res.Content, "not found") {
		t.Errorf("error should name the missing path, got: %s", res.Content)
	}
	// Nothing reached Gemini: the failure happened before the request.
	if len(requests) != 1 {
		t.Errorf("Gemini saw %d requests, want 1 (the failed follow-up sent nothing)", len(requests))
	}
}

// TestReviewScreenshotConversationMisuse pins the follow-up contract: an
// unknown conversation_id and a follow-up that carries image_paths or spec
// are refused with readable errors, so the model corrects the call rather
// than paying for a request it did not mean.
func TestReviewScreenshotConversationMisuse(t *testing.T) {
	var requests reviewScreenshotRequests
	srv := reviewConversationServer(t, &requests)

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "a.png", "x")

	first := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"a.png"},
		Question:   "what is wrong?",
	})
	if first.IsError {
		t.Fatalf("first call failed: %s", first.Content)
	}
	firstLine, _, _ := strings.Cut(first.Content, "\n")
	id := strings.TrimPrefix(firstLine, "conversation_id: ")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: "rvw-deadbeef",
		Question:       "look again",
	})
	if !res.IsError || !strings.Contains(res.Content, "unknown conversation_id") {
		t.Errorf("unknown id should be refused, got: %s", res.Content)
	}

	res = runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		Question:       "look again",
		Spec:           "a new spec",
	})
	if !res.IsError || !strings.Contains(res.Content, "no spec") {
		t.Errorf("spec on a follow-up should be refused, got: %s", res.Content)
	}

	// The instruction is fixed for a conversation's life the same way the
	// spec is: the thread being replayed was answered under it.
	res = runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		Question:       "look again",
		Mode:           reviewModeDescribe,
	})
	if !res.IsError || !strings.Contains(res.Content, "no mode") {
		t.Errorf("a mode switch mid-conversation should be refused, got: %s", res.Content)
	}

	// The refused calls sent nothing: only the first call reached Gemini.
	if len(requests) != 1 {
		t.Errorf("Gemini saw %d requests, want 1 (the refused calls sent nothing)", len(requests))
	}
}

// TestReviewScreenshotFollowUpReplacesImages pins the re-capture loop: a
// follow-up carrying new image_paths sends those bytes, keeps the thread of
// earlier questions and answers, and tells the vision model the images have
// changed — so it looks at the new capture rather than reconciling it against
// its own previous answer (docs/reviews/vision-path-2026-08-14.md).
func TestReviewScreenshotFollowUpReplacesImages(t *testing.T) {
	var requests reviewScreenshotRequests
	srv := reviewConversationServer(t, &requests)
	defer srv.Close()

	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	writeFile(t, root, "before.png", "first bytes")
	writeFile(t, root, "after.png", "second bytes")

	first := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"before.png"},
		Question:   "is the dropdown covered?",
	})
	if first.IsError {
		t.Fatalf("first call failed: %s", first.Content)
	}
	firstLine, _, _ := strings.Cut(first.Content, "\n")
	id := strings.TrimPrefix(firstLine, "conversation_id: ")

	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		ImagePaths:     []string{"after.png"},
		Question:       "and now?",
	})
	if res.IsError {
		t.Fatalf("a follow-up with new images should be accepted: %s", res.Content)
	}
	if len(requests) != 2 {
		t.Fatalf("Gemini saw %d requests, want 2", len(requests))
	}

	second := requests[1].Input
	var images, texts []string
	for _, part := range second {
		switch part.Type {
		case gemini.ContentTypeImage:
			decoded, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil {
				t.Fatalf("image part is not base64: %v", err)
			}
			images = append(images, string(decoded))
		case gemini.ContentTypeText:
			texts = append(texts, part.Text)
		}
	}
	if len(images) != 1 || images[0] != "second bytes" {
		t.Errorf("follow-up should send only the replacement image, got %v", images)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "Image 1: after.png") {
		t.Errorf("the replacement image should be labelled by its own name, got: %s", joined)
	}
	if !strings.Contains(joined, "is the dropdown covered?") {
		t.Errorf("the earlier exchange should still be replayed, got: %s", joined)
	}
	if !strings.Contains(joined, "NEW captures") {
		t.Errorf("the model must be told the images were replaced, got: %s", joined)
	}

	// A third follow-up with no images continues from the replacement, not
	// from the capture the conversation opened with.
	if res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ConversationID: id,
		Question:       "look closer",
	}); res.IsError {
		t.Fatalf("third call failed: %s", res.Content)
	}
	third := requests[2].Input
	for _, part := range third {
		if part.Type != gemini.ContentTypeImage {
			continue
		}
		decoded, _ := base64.StdEncoding.DecodeString(part.Data)
		if string(decoded) != "second bytes" {
			t.Errorf("the conversation should have moved to the replacement image, got %q", decoded)
		}
	}
}

// captureReviewRequest runs one ReviewScreenshot call against a stub Gemini
// and returns the request body it sent, so a test can assert on the
// instruction and the generation config together.
func captureReviewRequest(t *testing.T, e *Executor, args reviewScreenshotArgs, answer string) (gemini.InteractionRequest, Result) {
	t.Helper()
	var got gemini.InteractionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer(answer,
			`{"total_tokens":300,"total_input_tokens":100,"total_output_tokens":50,"total_thought_tokens":150,"total_cached_tokens":0}`)))
	}))
	defer srv.Close()
	e.Gemini = reviewScreenshotClient(srv)
	return got, runTool(t, e, "ReviewScreenshot", args)
}

// TestReviewScreenshotDescribeMode pins the mode the review path could not
// express: a question about what is on the screen gets an instruction that
// judges nothing, a shape that carries elements rather than findings, and
// less thinking — because saying what is there is not the part that needs
// reasoning, and thinking is where a call's cost goes
// (docs/reviews/vision-path-2026-08-14.md).
func TestReviewScreenshotDescribeMode(t *testing.T) {
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "shot.png", "x")

	req, res := captureReviewRequest(t, e, reviewScreenshotArgs{
		ImagePaths: []string{"shot.png"},
		Question:   "what does this page show?",
		Mode:       reviewModeDescribe,
		// A spec is meaningless here and must not drag in the review
		// instruction: describe mode judges against nothing.
		Spec: "the rail is 244px wide",
	}, `{"observed":"A settings form.","elements":[{"image":"shot.png","text":"Save","role":"button","styling":"bold"}]}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}

	if !strings.Contains(req.SystemInstruction, "You are describing a web page screenshot") {
		t.Errorf("describe mode should send the describe instruction, got: %s", req.SystemInstruction)
	}
	if strings.Contains(req.SystemInstruction, "only standard of correctness") {
		t.Errorf("describe mode must not be held to a spec, got: %s", req.SystemInstruction)
	}
	if req.GenerationConfig == nil || req.GenerationConfig.ThinkingLevel != gemini.ThinkingLevelLow {
		t.Errorf("describe mode should think less than a review, got: %+v", req.GenerationConfig)
	}
	// No response_format: the model picks its own container and reads the
	// instruction for what goes in it. Constraining the container is what
	// returns an empty one
	// (docs/gemini-3.5-flash-ui-review-prompting.md, measured).
	if req.ResponseFormat != nil {
		t.Errorf("no response_format should be sent, got: %+v", req.ResponseFormat)
	}
	if !strings.Contains(res.Content, "Observed: A settings form.") {
		t.Errorf("result should lead with the description, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "1 element\n") {
		t.Errorf("describe mode should count elements, got: %s", res.Content)
	}
}

// TestReviewScreenshotRejectsUnknownMode: a typo'd mode names the set it
// should have come from rather than silently falling back to review, which
// would answer a different question than the one asked.
func TestReviewScreenshotRejectsUnknownMode(t *testing.T) {
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "shot.png", "x")
	// No Gemini client is needed: the mode is rejected before the call.
	res := runTool(t, e, "ReviewScreenshot", reviewScreenshotArgs{
		ImagePaths: []string{"shot.png"},
		Question:   "what is wrong?",
		Mode:       "transcribe",
	})
	if !res.IsError || !strings.Contains(res.Content, `"review"`) || !strings.Contains(res.Content, `"describe"`) {
		t.Errorf("an unknown mode should name the valid set, got: %s", res.Content)
	}
}

// TestReviewScreenshotThinkingLevel pins both halves of the knob: review mode
// thinks harder than describe by default, and an operator who pins
// google.vision_thinking_level overrides both.
func TestReviewScreenshotThinkingLevel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned string
		mode   string
		want   string
	}{
		{"review defaults to medium", "auto", reviewModeReview, gemini.ThinkingLevelMedium},
		{"describe defaults to low", "auto", reviewModeDescribe, gemini.ThinkingLevelLow},
		{"an unset setting behaves like auto", "", reviewModeDescribe, gemini.ThinkingLevelLow},
		{"the setting overrides the mode", "high", reviewModeDescribe, gemini.ThinkingLevelHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			e, err := NewExecutor(root, &Policy{Mode: ModeFull})
			if err != nil {
				t.Fatal(err)
			}
			e.Settings = settings.NewResolver(&fakeSettingsStore{values: map[string]string{
				settings.KeyGoogleVisionThinkingLevel: tc.pinned,
			}})
			writeFile(t, root, "shot.png", "x")

			req, res := captureReviewRequest(t, e, reviewScreenshotArgs{
				ImagePaths: []string{"shot.png"},
				Question:   "what is here?",
				Mode:       tc.mode,
			}, `{"observed":"A page.","findings":[],"elements":[]}`)
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			if req.GenerationConfig == nil || req.GenerationConfig.ThinkingLevel != tc.want {
				t.Errorf("thinking_level = %+v, want %q", req.GenerationConfig, tc.want)
			}
		})
	}
}

// TestReviewScreenshotReportsItsCost pins the other half of the previous
// review's recommendation: the tool description can only say a call is
// expensive in general, because it is part of the frozen request head
// (docs/CACHE.md), so the actual figure has to come home on the result — in
// the place the model decides whether to make another call.
func TestReviewScreenshotReportsItsCost(t *testing.T) {
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Prices = &pricing.Table{
		CapturedAt: "2026-08-10",
		Models: map[string]pricing.ModelPrices{
			"gemini-3.5-flash": {InputCacheHitPerMillionUSD: 0.15, InputCacheMissPerMillionUSD: 1.5, OutputPerMillionUSD: 9.0},
		},
	}
	writeFile(t, root, "shot.png", "x")

	_, res := captureReviewRequest(t, e, reviewScreenshotArgs{
		ImagePaths: []string{"shot.png"},
		Question:   "what is wrong?",
	}, `{"observed":"A page.","findings":[]}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "This call cost $") {
		t.Errorf("the result should say what the call cost, got: %s", res.Content)
	}
	// Thinking is the part a caller can act on — by mode, or by the setting —
	// so it is named rather than buried in a single total.
	if !strings.Contains(res.Content, "were the model thinking") {
		t.Errorf("the result should name the thinking share, got: %s", res.Content)
	}
	if res.GeminiUsage == nil {
		t.Fatal("the call's usage must ride home for the runner to commit")
	}
	// Named, so the usage event is separable from the session's own turns.
	if res.GeminiUsage.Model == "" {
		t.Error("the usage event must name the model that was billed")
	}
}

// TestReviewScreenshotCostLineOmittedWithoutAPrice: a price table with no
// entry for the vision model leaves the cost at zero, and a zero must print
// nothing rather than an invented $0.0000 that reads as "this was free".
func TestReviewScreenshotCostLineOmittedWithoutAPrice(t *testing.T) {
	root := t.TempDir()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "shot.png", "x")

	_, res := captureReviewRequest(t, e, reviewScreenshotArgs{
		ImagePaths: []string{"shot.png"},
		Question:   "what is wrong?",
	}, `{"observed":"A page.","findings":[]}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "This call cost") {
		t.Errorf("an unpriced model must not claim a cost, got: %s", res.Content)
	}
}
