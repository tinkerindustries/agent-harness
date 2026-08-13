package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

// pngBytes renders a tiny real PNG, so the image path is exercised against
// bytes that actually are a PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// visionExecutor returns an Executor whose provider sees images, with root
// as its workspace.
func visionExecutor(t *testing.T, root string) (*Executor, string) {
	t.Helper()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.SeeImages = true
	return e, root
}

// TestReadImageOnVisionProvider is the heart of the phase: on a provider
// that sees images, Read of an image path returns the file as an image_url
// part — a base64 data URI of the exact shape the Kimi guide prescribes —
// with a short text label naming the file (docs/KIMI-INTEGRATION.md §4.5,
// third_party/kimi-docs/guide/use-kimi-vision-model.md).
func TestReadImageOnVisionProvider(t *testing.T) {
	e, root := visionExecutor(t, t.TempDir())
	data := pngBytes(t)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "shot.png"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "Image: shot.png" {
		t.Errorf("Content = %q, want the file label %q", res.Content, "Image: shot.png")
	}
	wantURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	if res.ImageURL != wantURI {
		t.Errorf("ImageURL = %q, want %q", res.ImageURL, wantURI)
	}
	// The data URI must carry the exact shape the guide shows: the MIME
	// type, then base64, with the bytes round-trippable.
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(res.ImageURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("ImageURL is not valid base64: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("ImageURL base64 does not round-trip the file bytes")
	}
}

// TestReadImageFormats covers JPEG and WebP alongside PNG: the MIME type in
// the data URI must follow the file's format.
func TestReadImageFormats(t *testing.T) {
	cases := []struct {
		name, ext, mime string
		data            []byte
	}{
		{"jpeg", ".jpg", "image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}},
		{"webp", ".webp", "image/webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8L")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, root := visionExecutor(t, t.TempDir())
			filename := "shot" + tc.ext
			if err := os.WriteFile(filepath.Join(root, filename), tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: filename}))
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			want := "data:" + tc.mime + ";base64," + base64.StdEncoding.EncodeToString(tc.data)
			if res.ImageURL != want {
				t.Errorf("ImageURL = %q, want %q", res.ImageURL, want)
			}
		})
	}
}

// TestReadImageOnDeepSeekUnchanged pins the other half of the split: on a
// provider that cannot see images, Read of an image path behaves exactly as
// it always has — the binary-file refusal, byte for byte, no image part.
func TestReadImageOnDeepSeekUnchanged(t *testing.T) {
	e, root := newTestExecutor(t)
	data := pngBytes(t)
	if err := os.WriteFile(filepath.Join(root, "image.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "image.png"}))
	if !res.IsError {
		t.Fatal("expected an error reading an image on DeepSeek")
	}
	if !strings.Contains(res.Content, "cannot read a binary file: image.png (") {
		t.Fatalf("expected the unchanged binary-file refusal, got: %s", res.Content)
	}
	if res.ImageURL != "" {
		t.Fatalf("DeepSeek Read must not return an image part, got %q", res.ImageURL)
	}
}

// TestReadImageRefusesSignatureMismatch: a binary file named .png whose
// bytes are not a PNG must be refused before it can enter the conversation —
// the bytes would otherwise be re-sent on every sub-turn of the frozen
// prefix and rejected by the API there.
func TestReadImageRefusesSignatureMismatch(t *testing.T) {
	e, root := visionExecutor(t, t.TempDir())
	// Binary (contains a NUL byte) but not a PNG signature.
	if err := os.WriteFile(filepath.Join(root, "fake.png"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "fake.png"}))
	if !res.IsError {
		t.Fatal("expected a refusal for a file that is not the image its extension names")
	}
	if !strings.Contains(res.Content, "not a image/png image") {
		t.Fatalf("expected a refusal naming the format, got: %s", res.Content)
	}
	if res.ImageURL != "" {
		t.Fatalf("a refused Read must not carry an image part, got %q", res.ImageURL)
	}
}

// TestReadImageRefusesOtherBinaryTypes: on a vision provider, a binary file
// that is not one of the three image formats is refused with a message that
// says what is and is not readable — the model recovers from a specific
// refusal.
func TestReadImageRefusesOtherBinaryTypes(t *testing.T) {
	e, root := visionExecutor(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "blob.bin"}))
	if !res.IsError {
		t.Fatal("expected a refusal for a non-image binary file")
	}
	for _, want := range []string{"PNG, JPEG, and WebP", "SVG reads as text", "any other binary type is refused"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("refusal does not say %q: %s", want, res.Content)
		}
	}
}

// TestReadSvgOnVisionProvider: an SVG reads as text even on a vision
// provider — it is XML, valid UTF-8, so the binary probe never fires and the
// model reads the source, exactly as the Kimi guide prescribes for SVG.
func TestReadSvgOnVisionProvider(t *testing.T) {
	e, root := visionExecutor(t, t.TempDir())
	svg := "<svg xmlns=\"http://www.w3.org/2000/svg\"><rect width=\"1\" height=\"1\"/></svg>\n"
	if err := os.WriteFile(filepath.Join(root, "icon.svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "icon.svg"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.ImageURL != "" {
		t.Fatalf("an SVG must read as text, not an image part, got %q", res.ImageURL)
	}
	if !strings.Contains(res.Content, "<svg") {
		t.Fatalf("expected the SVG source as text, got: %s", res.Content)
	}
}

// TestReadImageOverCap: an image whose encoded data URI exceeds the
// screenshot cap is refused with a result naming the actual limit and
// suggesting a resize — a refusal the model can act on, never a failed run.
// The cap is the existing screenshot cap setting
// (tools.reviewscreenshot_max_bytes), resolved through the settings
// registry so a changed limit applies without a restart (docs/CACHE.md).
func TestReadImageOverCap(t *testing.T) {
	resolver := settings.NewResolver(&fakeSettingsStore{values: map[string]string{
		settings.KeyToolReviewScreenshotMaxBytes: "64",
	}})
	e, root := visionExecutor(t, t.TempDir())
	e.Settings = resolver

	data := pngBytes(t)
	if len(data) <= 64 {
		t.Fatalf("test setup: the tiny PNG is %d bytes, want it over the 64-byte cap", len(data))
	}
	if err := os.WriteFile(filepath.Join(root, "big.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "big.png"}))
	if !res.IsError {
		t.Fatal("expected a refusal for an over-cap image")
	}
	if !strings.Contains(res.Content, "64-byte cap for images") {
		t.Fatalf("expected the refusal to name the 64-byte cap, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Resize or re-encode") {
		t.Fatalf("expected the refusal to suggest resizing, got: %s", res.Content)
	}
	if res.ImageURL != "" {
		t.Fatalf("an over-cap image must not carry an image part, got %q", res.ImageURL)
	}
}

// TestImageSignatureMatches is the unit contract of the format gate: the
// magic bytes that decide whether a binary file is a readable image.
func TestImageSignatureMatches(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
		want bool
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}, "image/png", true},
		{"png truncated", []byte{0x89, 'P', 'N'}, "image/png", false},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg", true},
		{"jpeg truncated", []byte{0xFF, 0xD8}, "image/jpeg", false},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8L"), "image/webp", true},
		{"webp wrong fourcc", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "image/webp", false},
		{"wrong mime", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, "image/jpeg", false},
		{"unknown mime", []byte{0x00, 0x01}, "image/gif", false},
		{"empty", nil, "image/png", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageSignatureMatches(tc.data, tc.mime); got != tc.want {
				t.Errorf("imageSignatureMatches(% x, %s) = %v, want %v", tc.data, tc.mime, got, tc.want)
			}
		})
	}
}
