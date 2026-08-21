// Package attachment validates one image a producer submitted alongside a
// work request — POST /api/runs (internal/httpapi) or the MCP
// deepseek_agent tool (internal/mcp) — before its bytes reach the store.
// Both producers build the same queue.Request-bound attachment and used to
// carry their own byte-for-byte copy of this check, including two identical
// copies of the MIME-type-by-extension table; a divergence between them
// would lose the name-shape check on one ingress without anyone noticing,
// and that check is what confines the file the worker later writes to
// scratch/attachments/ (internal/workspace.Prepare) rather than somewhere a
// path-shaped name could reach.
//
// This package is deliberately a leaf, depending on nothing internal.
// internal/httpapi imports neither internal/session nor internal/worker
// (ARCHITECTURE.md), and internal/httpapi/screenshots.go's own
// resolveWithinWorkspace already explains why that surface avoids
// internal/tools for a narrower reason it needs itself: importing a
// heavier package for one shared helper would pull that package's whole
// dependency closure in behind it. A leaf validator sidesteps the question
// for both internal/httpapi and internal/mcp.
package attachment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ImageMIMETypes maps a lower-cased file extension, dot included, to the
// MIME type it names: the three image types ReviewScreenshot accepts
// (internal/tools/reviewscreenshot.go), and therefore the only types worth
// accepting anywhere an attachment's whole purpose is being passed back to
// that tool. Declared once so a producer, the screenshot-serving endpoint
// (internal/httpapi/screenshots.go), and the vision tools
// (internal/tools/vision.go) cannot quietly disagree about the list.
//
// This is deliberately distinct from internal/tools/screenshot.go's
// screenshotOutputExtensions, which is narrower on purpose — no WebP,
// because it names what Chromium's own screenshot capture can produce, not
// what the harness can read back in.
var ImageMIMETypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// WorkspaceDir is the directory, relative to a session's workspace root,
// that every attachment is materialised into: the images a work request
// carried, written by internal/workspace.Prepare, and the ones somebody
// pasted into the composer of a session that already has a workspace,
// written by internal/session (docs/RUN-CONTROL.md, "Images in the
// composer").
//
// It lives here because four packages have to agree on it and two of them
// must not import each other: internal/httpapi records the paths on a steer
// it accepts, internal/session writes the files and names them to the model,
// internal/workspace creates the directory, and the browser addresses each
// file on GET /api/sessions/{id}/screenshot by exactly this path. A leaf
// they all already depend on is the only place all four can read one
// spelling.
const WorkspaceDir = "scratch/attachments"

// WorkspacePaths turns attachment file names into the workspace-relative
// paths they are materialised under. Nil for none, so a payload field built
// from it stays absent rather than empty.
func WorkspacePaths(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = WorkspaceDir + "/" + name
	}
	return out
}

// MIMEType reports the MIME type name claims by extension, from
// ImageMIMETypes, and whether the extension is one of them.
func MIMEType(name string) (string, bool) {
	mime, ok := ImageMIMETypes[strings.ToLower(filepath.Ext(name))]
	return mime, ok
}

// Validate checks one attachment and returns its name, resolved MIME type,
// and decoded bytes. name must be a plain file name — the workspace writes
// the file under it, so a path-shaped name would be a way out of
// scratch/attachments/ — and its extension must be one of ImageMIMETypes,
// because the model's whole use of the file is passing it back to
// ReviewScreenshot. mimeType, the caller-asserted MIME type, is optional but
// when present must match what the extension itself resolves to. base64Data
// is decoded and checked against maxBytes; an empty or oversized decoded
// payload is refused.
func Validate(name, mimeType, base64Data string, maxBytes int) (validName, resolvedMIME string, data []byte, err error) {
	if name == "" {
		return "", "", nil, errors.New("attachment name is required")
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return "", "", nil, fmt.Errorf("attachment name %q must be a plain file name, not a path", name)
	}
	mime, ok := MIMEType(name)
	if !ok {
		return "", "", nil, fmt.Errorf("attachment %q: only PNG, JPEG, and WebP images are accepted", name)
	}
	if mimeType != "" && mimeType != mime {
		return "", "", nil, fmt.Errorf("attachment %q: mime_type %q does not match the file's extension", name, mimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", "", nil, fmt.Errorf("attachment %q: data is not valid base64", name)
	}
	if len(decoded) > maxBytes {
		return "", "", nil, fmt.Errorf("attachment %q is %d bytes, over the %d-byte per-file limit", name, len(decoded), maxBytes)
	}
	if len(decoded) == 0 {
		return "", "", nil, fmt.Errorf("attachment %q is empty", name)
	}
	return name, mime, decoded, nil
}
