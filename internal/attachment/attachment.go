// Package attachment validates one image a client submitted alongside a
// request, before its bytes reach the store, and writes the accepted ones
// into a session's workspace. The name-shape check is what confines the file
// to scratch/attachments/ rather than somewhere a path-shaped name could
// reach, and it runs on both paths: once at ingress, once again at the
// write.
//
// This package is deliberately a leaf, depending on nothing internal.
// Several packages have to agree on one spelling of that directory and on
// one image-extension table, and a leaf is the only place all of them can
// read it without pulling a heavier package's whole dependency closure in
// behind it.
package attachment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImageMIMETypes maps a lower-cased file extension, dot included, to the
// MIME type it names: the three image types ReviewScreenshot accepts
// (internal/tools/reviewscreenshot.go), and therefore the only types worth
// accepting anywhere an attachment's whole purpose is being passed back to
// that tool. Declared once so the ingress check and the vision tools
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
// that every attachment is materialised into: the images a request carried,
// and the ones somebody sent into a session that already has a workspace
// (docs/RUN-CONTROL.md, "Images in the composer").
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

// File is one image ready to be written into a session's workspace: the
// bytes the store held, plus the file name and MIME type to write them
// under.
type File struct {
	Name     string
	MIMEType string
	Data     []byte
}

// Write materialises files into dir's scratch/attachments/, each under its
// own name. The name must be a plain file name — no separators, no ".." —
// so an attachment can never escape the attachments directory however it was
// accepted; Validate enforces the same rule at ingress, and this is the
// second line of defence.
func Write(dir string, files []File) error {
	if len(files) == 0 {
		return nil
	}
	attDir := filepath.Join(dir, filepath.FromSlash(WorkspaceDir))
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		return fmt.Errorf("attachment: create scratch/attachments in %q: %w", dir, err)
	}
	for _, f := range files {
		name := filepath.Base(f.Name)
		if name == "" || name == "." || name == ".." || name != f.Name {
			return fmt.Errorf("attachment: name %q is not a plain file name", f.Name)
		}
		if err := os.WriteFile(filepath.Join(attDir, name), f.Data, 0o644); err != nil {
			return fmt.Errorf("attachment: write %s: %w", name, err)
		}
	}
	return nil
}
