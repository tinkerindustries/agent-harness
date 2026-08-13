package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// writePNG writes a real one-pixel PNG at path, so a serving test asserts on
// bytes a browser would actually decode rather than on a text file wearing a
// .png extension.
func writePNG(t *testing.T, path string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	return buf.Bytes()
}

// mustCreateSessionInWorkspace creates a session rooted at a real directory,
// which is what the screenshot endpoint reads through.
func mustCreateSessionInWorkspace(t *testing.T, st *store.Store, id, workspace string) {
	t.Helper()
	err := st.CreateSession(context.Background(), store.Session{
		ID:             id,
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      workspace,
		PermissionMode: "full",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		CreatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func screenshotURL(base, sessionID, path string) string {
	return base + "/api/sessions/" + sessionID + "/screenshot?path=" + url.QueryEscape(path)
}

func TestScreenshotServesAnImageFromTheSessionWorkspace(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-shot", ws)
	want := writePNG(t, filepath.Join(ws, "scratch", "home.png"))

	resp, err := http.Get(screenshotURL(srv.URL, "sess-shot", "scratch/home.png"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("body is %d bytes, want the %d-byte PNG that was written", len(body), len(want))
	}
}

// An absolute path is what the model actually passes to ReviewScreenshot most
// of the time, so it has to resolve the same way the relative form does.
func TestScreenshotAcceptsAnAbsolutePathInsideTheWorkspace(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-abs", ws)
	abs := filepath.Join(ws, "scratch", "wide.png")
	writePNG(t, abs)

	resp, err := http.Get(screenshotURL(srv.URL, "sess-abs", abs))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestScreenshotHeadIsServedWithoutABody(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-head", ws)
	writePNG(t, filepath.Join(ws, "shot.png"))

	req, err := http.NewRequest(http.MethodHead, screenshotURL(srv.URL, "sess-head", "shot.png"), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned %d bytes of body, want none", len(body))
	}
}

// The workspace of a finished run gets cleaned up (docs/RUN-CONTROL.md), and
// this endpoint reads the live workspace, so a missing file is the ordinary
// case rather than the exceptional one. It must be a plain 404 the frontend
// can turn into "no longer available".
func TestScreenshotIs404WhenTheWorkspaceIsGone(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-gone", ws)
	writePNG(t, filepath.Join(ws, "shot.png"))
	if err := os.RemoveAll(ws); err != nil {
		t.Fatalf("remove workspace: %v", err)
	}

	resp, err := http.Get(screenshotURL(srv.URL, "sess-gone", "shot.png"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// The endpoint must not become a general file read over the workspace, and
// must not become one over the rest of the box either.
func TestScreenshotRefusesPathsOutsideTheWorkspace(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	outside := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-escape", ws)
	writePNG(t, filepath.Join(outside, "secret.png"))

	// A sibling directory sharing the workspace's name prefix must not pass
	// the containment check as a child of it.
	sibling := ws + "-evil"
	writePNG(t, filepath.Join(sibling, "secret.png"))
	t.Cleanup(func() { os.RemoveAll(sibling) })

	cases := []struct {
		name string
		path string
	}{
		// A traversal has to carry an image extension to get past the type
		// gate at all, which is what makes it a containment test rather than
		// a second test of the extension allowlist.
		{"traversal", "../../etc/hosts.png"},
		{"traversal to a real image", filepath.Join("..", filepath.Base(outside), "secret.png")},
		{"absolute path outside", filepath.Join(outside, "secret.png")},
		{"name-prefix sibling", filepath.Join(sibling, "secret.png")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(screenshotURL(srv.URL, "sess-escape", tc.path))
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for %q", resp.StatusCode, tc.path)
			}
		})
	}
}

// A symlink planted inside the workspace must be resolved and then rejected,
// not followed out of the workspace.
func TestScreenshotDoesNotFollowASymlinkOutOfTheWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	outside := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-link", ws)

	target := filepath.Join(outside, "secret.png")
	writePNG(t, target)
	link := filepath.Join(ws, "innocent.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	resp, err := http.Get(screenshotURL(srv.URL, "sess-link", "innocent.png"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Only the three types ReviewScreenshot accepts come back out, so the
// endpoint cannot be used to read source, .env files, or the database.
func TestScreenshotRefusesNonImageExtensions(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ws := t.TempDir()
	mustCreateSessionInWorkspace(t, st, "sess-type", ws)
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("DEEPSEEK_API_KEY=sk-live"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	for _, path := range []string{".env", "main.go", "shot.png.txt", "shot.gif"} {
		resp, err := http.Get(screenshotURL(srv.URL, "sess-type", path))
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status for %q = %d, want 400", path, resp.StatusCode)
		}
		if bytes.Contains(body, []byte("sk-live")) {
			t.Fatalf("the response for %q leaked file contents", path)
		}
	}
}

func TestScreenshotRequiresAPathParameter(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSessionInWorkspace(t, st, "sess-nopath", t.TempDir())

	resp, err := http.Get(srv.URL + "/api/sessions/sess-nopath/screenshot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestScreenshotIs404ForAnUnknownSession(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp, err := http.Get(screenshotURL(srv.URL, "sess-missing", "shot.png"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
