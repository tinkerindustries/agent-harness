package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
)

// reviewScreenshotClient builds a Gemini client pointed at srv, so a test can
// drive a vision tool end to end without reaching the real API. Named for the
// tool that originated it, back when it lived in reviewscreenshot_test.go
// alongside the tool itself; kept here now that Glance, Ground, and Detect
// are the only callers.
func reviewScreenshotClient(srv *httptest.Server) *gemini.Client {
	return gemini.NewClient(srv.URL, gemini.WithAPIKeyProvider(func() (string, error) {
		return "gk-test", nil
	}))
}

// testPNGBytes renders a w×h RGBA image of deterministic noise and encodes it
// as PNG. Random pixels compress poorly, so a large one is a reliably
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

// TestParseMatches covers the tolerant parse pipeline ground.py's four
// stages became (vision.go, groundItems/normalizeBox): a clean array, a
// fenced one, every accepted wrapper key, the regex scavenger over prose,
// inverted axes, out-of-range values, and a degenerate box. The tolerance is
// the product (docs/VISION-TOOLKIT.md §6, "the parser's tolerance is the
// product") — each case here is a shape the model has been observed to
// answer in.
func TestParseMatches(t *testing.T) {
	const w, h = 1000, 1000
	wantBox := [4]int{200, 100, 400, 300} // box_2d [100,200,300,400] = y0,x0,y1,x1

	single := func(t *testing.T, matches []visionMatch, err error, wantLabel string) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
		}
		if matches[0].Label != wantLabel {
			t.Errorf("label = %q, want %q", matches[0].Label, wantLabel)
		}
		if matches[0].Box != wantBox {
			t.Errorf("box = %v, want %v", matches[0].Box, wantBox)
		}
	}

	t.Run("clean JSON array", func(t *testing.T) {
		matches, err := parseMatches(`[{"box_2d":[100,200,300,400],"label":"button"}]`, w, h, "target")
		single(t, matches, err, "button")
	})

	t.Run("fenced array", func(t *testing.T) {
		answer := "```json\n[{\"box_2d\":[100,200,300,400],\"label\":\"button\"}]\n```"
		matches, err := parseMatches(answer, w, h, "target")
		single(t, matches, err, "button")
	})

	t.Run("object wrapping the array under an accepted key", func(t *testing.T) {
		for _, key := range wrapperKeys {
			t.Run(key, func(t *testing.T) {
				answer := fmt.Sprintf(`{%q:[{"box_2d":[100,200,300,400],"label":"button"}]}`, key)
				matches, err := parseMatches(answer, w, h, "target")
				single(t, matches, err, "button")
			})
		}
	})

	t.Run("an unrecognised wrapper key is an error", func(t *testing.T) {
		if _, err := parseMatches(`{"matches":[{"box_2d":[100,200,300,400]}]}`, w, h, "target"); err == nil {
			t.Fatal("expected an error for a wrapper key not in wrapperKeys")
		}
	})

	t.Run("prose with an embedded object (the regex scavenger)", func(t *testing.T) {
		answer := `Sure, here is what I found: {"box_2d": [100, 200, 300, 400], "label": "the button"} — hope that helps!`
		matches, err := parseMatches(answer, w, h, "target")
		single(t, matches, err, "the button")
	})

	t.Run("inverted axes are swapped before scaling", func(t *testing.T) {
		// y0/y1 and x0/x1 each reversed from the clean case above; the
		// normalised box must come out identical.
		matches, err := parseMatches(`[{"box_2d":[300,400,100,200],"label":"button"}]`, w, h, "target")
		single(t, matches, err, "button")
	})

	t.Run("out-of-range values clamp to the image", func(t *testing.T) {
		matches, err := parseMatches(`[{"box_2d":[-50,-50,1200,1200],"label":"button"}]`, w, h, "target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
		}
		want := [4]int{0, 0, 1000, 1000}
		if matches[0].Box != want {
			t.Errorf("box = %v, want %v (clamped to the image)", matches[0].Box, want)
		}
	})

	t.Run("a degenerate box is dropped, not errored", func(t *testing.T) {
		matches, err := parseMatches(`[{"box_2d":[500,500,500,500],"label":"button"}]`, w, h, "target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(matches) != 0 {
			t.Fatalf("expected the zero-area box to be dropped, got %+v", matches)
		}
	})

	t.Run("a missing label falls back to the target", func(t *testing.T) {
		matches, err := parseMatches(`[{"box_2d":[100,200,300,400]}]`, w, h, "the submit button")
		single(t, matches, err, "the submit button")
	})

	t.Run("label falls back through label, caption, description", func(t *testing.T) {
		matches, err := parseMatches(`[{"box_2d":[100,200,300,400],"caption":"from caption"}]`, w, h, "target")
		single(t, matches, err, "from caption")
	})

	t.Run("text with no recoverable box is an error", func(t *testing.T) {
		if _, err := parseMatches("Sorry, I can't help with that.", w, h, "target"); err == nil {
			t.Fatal("expected an error for text with no box anywhere")
		}
	})

	t.Run("an empty array is not an error", func(t *testing.T) {
		matches, err := parseMatches(`[]`, w, h, "target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(matches) != 0 {
			t.Fatalf("got %+v, want no matches", matches)
		}
	})
}

// TestGlancePrompt pins glancePrompt's four branches (vision.go, ported from
// bin/glance's build_prompt): ocr, query, and the default description for one
// image versus several. ocr and query are mutually exclusive upstream
// (execGlance enforces it before glancePrompt is ever called), so each is
// exercised on its own.
func TestGlancePrompt(t *testing.T) {
	t.Run("ocr, single image", func(t *testing.T) {
		got := glancePrompt(glanceArgs{OCR: true}, 1)
		if !strings.Contains(got, "this image") {
			t.Errorf("got %q, want it to name a single image", got)
		}
		if strings.Contains(got, "these images") || strings.Contains(got, "ordinal") {
			t.Errorf("a single-image OCR prompt should not ask for per-image labels: %q", got)
		}
	})

	t.Run("ocr, multiple images, labelled and with extra requirements", func(t *testing.T) {
		got := glancePrompt(glanceArgs{OCR: true, OCRExtra: "preserve line breaks"}, 2)
		for _, want := range []string{"these images", "Label each image's text with its ordinal", "Additional requirements: preserve line breaks"} {
			if !strings.Contains(got, want) {
				t.Errorf("got %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("query is sent verbatim, ocr and count ignored", func(t *testing.T) {
		const query = "Is the badge left of the title on one line?"
		if got := glancePrompt(glanceArgs{Query: query}, 1); got != query {
			t.Errorf("got %q, want the query unchanged", got)
		}
		if got := glancePrompt(glanceArgs{Query: query}, 3); got != query {
			t.Errorf("query should win regardless of image count, got %q", got)
		}
	})

	t.Run("default, multiple images asks for differences", func(t *testing.T) {
		got := glancePrompt(glanceArgs{}, 3)
		for _, want := range []string{"Describe each image", "Image 1, Image 2", "notable differences"} {
			if !strings.Contains(got, want) {
				t.Errorf("got %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("default, single image", func(t *testing.T) {
		const want = "Please describe the contents of this image in detail."
		if got := glancePrompt(glanceArgs{}, 1); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestParseRegion pins the X1,Y1,X2,Y2 argument every region-taking tool
// shares: axis inversion is corrected, the box is clamped to the image, and
// a box that clamps to nothing or fails to parse is an error naming why.
func TestParseRegion(t *testing.T) {
	cases := []struct {
		name    string
		region  string
		w, h    int
		want    [4]int
		wantErr string
	}{
		{"ordinary box", "10,20,110,220", 1000, 1000, [4]int{10, 20, 110, 220}, ""},
		{"inverted x axis", "110,20,10,220", 1000, 1000, [4]int{10, 20, 110, 220}, ""},
		{"inverted y axis", "10,220,110,20", 1000, 1000, [4]int{10, 20, 110, 220}, ""},
		{"both axes inverted", "110,220,10,20", 1000, 1000, [4]int{10, 20, 110, 220}, ""},
		{"clamped to the image bounds", "-50,-50,5000,5000", 200, 100, [4]int{0, 0, 200, 100}, ""},
		{"whitespace around numbers", " 10 , 20 , 110 , 220 ", 1000, 1000, [4]int{10, 20, 110, 220}, ""},
		{"not four integers", "1,2,3", 100, 100, [4]int{}, "region expects four integers"},
		{"too many", "1,2,3,4,5", 100, 100, [4]int{}, "region expects four integers"},
		{"non-numeric", "a,b,c,d", 100, 100, [4]int{}, "region expects four integers"},
		{"empty after clamping to a box outside the image", "5000,5000,6000,6000", 1000, 1000, [4]int{}, "empty after clamping"},
		{"a degenerate box", "500,500,500,500", 1000, 1000, [4]int{}, "empty after clamping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRegion(tc.region, tc.w, tc.h)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseRegion(%q) error = %v, want it to contain %q", tc.region, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRegion(%q) unexpected error: %v", tc.region, err)
			}
			if got != tc.want {
				t.Errorf("parseRegion(%q) = %v, want %v", tc.region, got, tc.want)
			}
		})
	}
}

// TestBoxPosition pins the coarse 3x3 grid a multi-match result labels each
// box with (ground.py's _position): which ninth of the image the box's
// centre falls in, named by the two axes independently so the four corners,
// four edges and the centre all read naturally.
func TestBoxPosition(t *testing.T) {
	const w, h = 300, 300
	cases := []struct {
		name string
		box  [4]int
		want string
	}{
		{"top-left", [4]int{0, 0, 100, 100}, "top-left"},
		{"top", [4]int{100, 0, 200, 100}, "top"},
		{"top-right", [4]int{200, 0, 300, 100}, "top-right"},
		{"left", [4]int{0, 100, 100, 200}, "left"},
		{"center", [4]int{100, 100, 200, 200}, "center"},
		{"right", [4]int{200, 100, 300, 200}, "right"},
		{"bottom-left", [4]int{0, 200, 100, 300}, "bottom-left"},
		{"bottom", [4]int{100, 200, 200, 300}, "bottom"},
		{"bottom-right", [4]int{200, 200, 300, 300}, "bottom-right"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boxPosition(tc.box, w, h); got != tc.want {
				t.Errorf("boxPosition(%v, %d, %d) = %q, want %q", tc.box, w, h, got, tc.want)
			}
		})
	}
}

// TestExecGroundReturnsAndFormatsBoxes drives Ground end to end against a
// test Gemini server: the 0-1000 grid box in the answer must come back scaled
// to the actual image's pixels and formatted the way formatMatches renders a
// single match. This is the path TestParseMatches and TestBoxPosition only
// exercise in isolation.
func TestExecGroundReturnsAndFormatsBoxes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer(`[{"box_2d":[100,200,300,400],"label":"Submit button"}]`, ""))
	}))
	defer srv.Close()

	e, root := newTestExecutor(t)
	e.Gemini = reviewScreenshotClient(srv)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), testPNGBytes(t, 100, 80), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "Ground", groundArgs{ImagePath: "shot.png", Target: "the submit button"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	// [100,200,300,400] is y0,x0,y1,x1 on the 0-1000 grid; scaled against the
	// 100x80 test image that is x1:20, y1:8, x2:40, y2:24.
	if !strings.Contains(res.Content, "x1: 20, y1: 8, x2: 40, y2: 24") {
		t.Errorf("box not scaled/formatted as expected, got: %s", res.Content)
	}
}

// TestExecGroundReportsNoMatch pins ground.py's own "no match found" text for
// an empty array, rather than an empty or missing result.
func TestExecGroundReportsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer(`[]`, ""))
	}))
	defer srv.Close()

	e, root := newTestExecutor(t)
	e.Gemini = reviewScreenshotClient(srv)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), testPNGBytes(t, 100, 80), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "Ground", groundArgs{ImagePath: "shot.png", Target: "a dragon"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.TrimSpace(res.Content) != "no match found" {
		t.Errorf("got %q, want \"no match found\"", res.Content)
	}
}

// TestExecDetectUsesDefaultCategoryAndFormatsInventory pins Detect's one
// piece of logic: with no category, the request sent to Gemini names
// detectDefaultCategory, and the result is always numbered (formatInventory),
// unlike Ground's single-match bare form.
func TestExecDetectUsesDefaultCategoryAndFormatsInventory(t *testing.T) {
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPrompt = string(body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer(`[{"box_2d":[0,0,100,100],"label":"OK button"},{"box_2d":[900,900,1000,1000],"label":"Cancel button"}]`, ""))
	}))
	defer srv.Close()

	e, root := newTestExecutor(t)
	e.Gemini = reviewScreenshotClient(srv)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), testPNGBytes(t, 100, 80), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "Detect", detectArgs{ImagePath: "shot.png"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(gotPrompt, detectDefaultCategory) {
		t.Errorf("request should name the default category, got: %s", gotPrompt)
	}
	if !strings.Contains(res.Content, "1. top-left OK button") || !strings.Contains(res.Content, "2. bottom-right Cancel button") {
		t.Errorf("expected a numbered inventory with position words, got: %s", res.Content)
	}
}

// TestExecCrop covers the local, no-model-call path: the default output
// path, an explicit one, the scale upscale, and the argument refusals. It is
// also the regression test for a real bug found while wiring this port up —
// execCrop originally called a nonexistent e.ResolvePath method (there is
// only the package-level ResolvePath(root, path)), which meant Crop could
// not compile, let alone run.
func TestExecCrop(t *testing.T) {
	e, root := newTestExecutor(t)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), testPNGBytes(t, 100, 80), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("default output path and dimensions", func(t *testing.T) {
		res := runTool(t, e, "Crop", cropArgs{ImagePath: "shot.png", Region: "10,10,60,50"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		if !strings.Contains(res.Content, "shot.crop.png") {
			t.Errorf("result should name the default output path, got: %s", res.Content)
		}
		data, err := os.ReadFile(filepath.Join(root, "shot.crop.png"))
		if err != nil {
			t.Fatalf("no crop written: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("crop does not decode: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 50 || b.Dy() != 40 {
			t.Errorf("crop is %dx%d, want 50x40", b.Dx(), b.Dy())
		}
	})

	t.Run("explicit output and scale enlarges the crop", func(t *testing.T) {
		res := runTool(t, e, "Crop", cropArgs{ImagePath: "shot.png", Region: "0,0,20,20", Output: "scaled.png", Scale: 3})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		if !strings.Contains(res.Content, "upscaled 3x") {
			t.Errorf("result should note the upscale, got: %s", res.Content)
		}
		data, err := os.ReadFile(filepath.Join(root, "scaled.png"))
		if err != nil {
			t.Fatalf("no crop written: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("crop does not decode: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 60 || b.Dy() != 60 {
			t.Errorf("scaled crop is %dx%d, want 60x60 (20x20 at 3x)", b.Dx(), b.Dy())
		}
	})

	t.Run("region is required", func(t *testing.T) {
		res := runTool(t, e, "Crop", cropArgs{ImagePath: "shot.png"})
		if !res.IsError || !strings.Contains(res.Content, "region is required") {
			t.Errorf("got: %s", res.Content)
		}
	})

	t.Run("image_path is required", func(t *testing.T) {
		res := runTool(t, e, "Crop", cropArgs{Region: "0,0,10,10"})
		if !res.IsError || !strings.Contains(res.Content, "image_path is required") {
			t.Errorf("got: %s", res.Content)
		}
	})

	t.Run("an out-of-range scale is refused", func(t *testing.T) {
		res := runTool(t, e, "Crop", cropArgs{ImagePath: "shot.png", Region: "0,0,10,10", Scale: 9})
		if !res.IsError {
			t.Error("expected a refusal for an out-of-range scale")
		}
	})
}
