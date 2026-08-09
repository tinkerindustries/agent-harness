package httpapi

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"

	"github.com/mrgeoffrich/deepseek-harness/internal/webassets"
)

// NewStaticHandler serves the frontend: the embedded, built assets by
// default, or a reverse proxy to a running Vite dev server when
// devFrontendURL is set: embed.FS for the built frontend, with a dev-mode
// passthrough to the Vite server (docs/DESIGN.md §4.8).
func NewStaticHandler(devFrontendURL string) (http.Handler, error) {
	if devFrontendURL != "" {
		target, err := url.Parse(devFrontendURL)
		if err != nil {
			return nil, fmt.Errorf("httpapi: parse dev frontend url %q: %w", devFrontendURL, err)
		}
		return httputil.NewSingleHostReverseProxy(target), nil
	}

	dist, err := webassets.Dist()
	if err != nil {
		return nil, fmt.Errorf("httpapi: open embedded frontend: %w", err)
	}
	return &spaHandler{root: dist, files: http.FileServerFS(dist)}, nil
}

// spaHandler serves a file that exists in root verbatim, and index.html for
// everything else. The frontend does its own pathname-based navigation with
// no router library (docs/DESIGN.md §5.7), so a path like /sessions/abc has
// no matching file on disk; it is the client script in index.html that
// turns that path into the transcript screen.
type spaHandler struct {
	root  fs.FS
	files http.Handler
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := path.Clean(r.URL.Path)
	if upath != "/" {
		if info, err := fs.Stat(h.root, upath[1:]); err == nil && !info.IsDir() {
			h.files.ServeHTTP(w, r)
			return
		}
	}
	data, err := fs.ReadFile(h.root, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
