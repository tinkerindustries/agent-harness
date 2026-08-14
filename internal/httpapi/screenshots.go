package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/attachment"
)

// handleGetScreenshot serves GET /api/sessions/{id}/screenshot?path=...: one
// image file from that session's workspace, so the transcript can render the
// screenshots a Screenshot call captured and a ReviewScreenshot call sent to
// Gemini (docs/TOOLS.md, "Seeing the screenshots"). Read-only and
// unauthenticated, like every other GET on this surface; the write endpoints
// are the ones carrying the control token.
//
// The image is read from the live workspace rather than from a copy the
// harness kept, which is the trade-off this endpoint is built on: no schema
// change and full resolution, but a session whose workspace has been cleaned
// up (docs/RUN-CONTROL.md) has no images left to serve. That case is a
// deliberate, ordinary 404 rather than an error worth logging — the
// transcript renders it as "no longer available", and an operator reading an
// old session should be told the file is gone rather than shown a broken
// image.
func (s *Server) handleGetScreenshot(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}

	userPath := r.URL.Query().Get("path")
	if userPath == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}
	// The allowlist here is deliberately the same three types
	// ReviewScreenshot accepts (internal/attachment.ImageMIMETypes): the
	// endpoint exists to show a human the images a session sent to the
	// vision model, so serving a type that tool would have refused would be
	// showing something the run could not have reviewed. It is also what
	// keeps this from being a general file-read endpoint over the
	// workspace — a session's workspace holds cloned repositories and
	// whatever the model wrote; only images come back out.
	contentType, ok := attachment.MIMEType(userPath)
	if !ok {
		http.Error(w, "screenshots are PNG, JPEG, or WebP files", http.StatusBadRequest)
		return
	}
	if sess.Workspace == "" {
		http.Error(w, "screenshot not available: session has no workspace", http.StatusNotFound)
		return
	}

	path, err := resolveWithinWorkspace(sess.Workspace, userPath)
	if err != nil && !filepath.IsAbs(userPath) {
		// The path in the query string is the one the model passed to
		// Screenshot, because that is what the transcript renders the gallery
		// from (web/src/components/blocks/toolArgs.ts screenshotPaths). A
		// relative path there lands under scratch/ whether or not it says so
		// (internal/tools' resolveScreenshotOutput), so the same second
		// attempt the tools make is what keeps the image addressable from the
		// call that produced it. Join before resolve, so the containment check
		// below still sees the whole path.
		path, err = resolveWithinWorkspace(sess.Workspace, filepath.Join("scratch", userPath))
	}
	if err != nil {
		// An escape and a missing file are both 404 with the same text. A
		// caller probing for a path outside the workspace learns only that
		// it cannot have it, never whether it exists.
		http.Error(w, "screenshot not available", http.StatusNotFound)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "screenshot not available", http.StatusNotFound)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "screenshot not available", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A capture can be retaken to the same path mid-run, so the browser must
	// revalidate rather than serve a stale frame from cache. ServeContent's
	// Last-Modified handling makes that revalidation cheap (a 304), and it
	// brings Range and HEAD with it — HEAD reaches every path on this
	// surface through the method gate.
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// resolveWithinWorkspace resolves userPath against a session's workspace root
// and fails unless the result is a real, existing path inside it.
//
// This does not reuse internal/tools.ResolvePath, which enforces the same
// invariant for the tools themselves. Importing that package here would pull
// the DeepSeek client, the Gemini client and the price table into the HTTP
// surface's dependency closure for one path helper, in a package whose whole
// premise is that it cannot reach the run loop. What is left is strictly
// narrower than ResolvePath rather than a copy of it: the file must already
// exist, so symlinks resolve in one call with no walking up to the deepest
// existing ancestor, and there is no path-to-be-created case to get wrong.
func resolveWithinWorkspace(root, userPath string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}

	candidate := userPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	// EvalSymlinks resolves every component, so a symlink inside the
	// workspace pointing out of it is caught by the containment check below
	// rather than followed.
	resolved, err := filepath.EvalSymlinks(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", os.ErrNotExist
	}
	return resolved, nil
}
