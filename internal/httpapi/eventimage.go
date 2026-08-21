package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/attachment"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// The bytes of an image a tool result carries, served from the event log at
// a URL of their own instead of riding every surface that hands out events.
//
// A Read of an image file for a vision provider records the whole picture as
// a base64 data URI in the event payload (store.ToolResultPayload.ImageURL),
// which is right for the model — the fold rebuilds the parts array from the
// log, so a replay reproduces the exact bytes whatever happened to the file
// since — and ruinous for the browser. Measured on
// sess-35da6920ca8e5ca590acb3a46341c924, a Blender session that read 34
// renders back: 39.4 MB across 668 events, of which the 34 image results
// were 38.9 MB. Every one of those bytes went down the transcript stream on
// every page load, uncompressed, ahead of the text nobody could read until
// it finished.
//
// So the HTTP surface detaches them. detachEventImage swaps the data URI for
// an image_href pointing back here, and handleGetEventImage decodes the
// bytes out of the event on demand. The store is untouched: this is a
// projection applied on the way out, in the same place and for the same
// reason redaction is (internal/httpapi/events.go redactEvent), and the
// write path — the fold that rebuilds the model's own conversation — still
// reads the literal payload.
//
// What the browser gets for it: the transcript arrives as text, each picture
// is fetched only when the tile it belongs to is rendered, the fetches run
// in parallel with each other, and — unlike the workspace screenshots next
// door — they are permanently cacheable, because an event row is append-only
// and the bytes at a given seq can never change.

// imageURLKey is the raw-JSON probe detachEventImage uses to skip payloads
// that carry no image without unmarshalling them. Nearly every tool_result
// is a small text blob, and the ones that are not are the expensive ones.
var imageURLKey = []byte(`"image_url"`)

// imageHrefField is the name the reference goes out under. It is deliberately
// NOT image_url: a data URI and a path that has to be fetched fail in
// different ways and are consumed by different code, so a client that has
// not been taught about the reference renders no image rather than an
// <img src="/api/..."> it believes is inline bytes.
const imageHrefField = "image_href"

// eventImageHref is the URL one event's image is served at. The session id
// is path-escaped because it is a stored value; every id the harness mints
// is already URL-safe, but this is the one place that assumption would turn
// into a broken link rather than an error.
func eventImageHref(sessionID string, seq int64) string {
	return fmt.Sprintf("/api/sessions/%s/events/%d/image", url.PathEscape(sessionID), seq)
}

// detachEventImage replaces a tool result's inline image bytes with a URL
// that serves them. Any other event, and any tool result without an image,
// is returned untouched and uncopied.
//
// The rewrite goes through map[string]json.RawMessage rather than
// store.ToolResultPayload so that a field added to that struct — or one
// written by an older binary and since removed — survives the round trip.
// The payload this produces is the same object minus one key plus one key,
// not a re-marshalling of the fields this package happens to know about.
//
// A failure at any step leaves the event exactly as it arrived. Serving the
// bytes inline is the old, working behaviour; dropping the picture because
// its payload did not parse would not be.
func detachEventImage(ev store.Event) store.Event {
	if ev.Kind != store.KindToolResult || !bytes.Contains(ev.Payload, imageURLKey) {
		return ev
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &fields); err != nil {
		return ev
	}
	raw, ok := fields["image_url"]
	if !ok {
		return ev
	}
	var uri string
	if err := json.Unmarshal(raw, &uri); err != nil {
		return ev
	}
	// Only inline bytes are worth detaching. A value that is already a
	// reference costs nothing to send and has no bytes here to serve, so it
	// travels as it is.
	if !strings.HasPrefix(uri, dataURIPrefix) {
		return ev
	}
	href, err := json.Marshal(eventImageHref(ev.SessionID, ev.Seq))
	if err != nil {
		return ev
	}
	delete(fields, "image_url")
	fields[imageHrefField] = href
	payload, err := json.Marshal(fields)
	if err != nil {
		return ev
	}
	ev.Payload = payload
	return ev
}

// handleGetEventImage serves GET /api/sessions/{id}/events/{seq}/image: the
// decoded bytes of the image the tool result at that seq carries.
//
// Read-only and unauthenticated like every other GET on this surface. It
// reads the event log rather than the workspace, which is what separates it
// from handleGetScreenshot next door: a screenshot is a live file that can
// be retaken or cleaned up, so that endpoint must revalidate and has a "no
// longer available" state; an event is append-only, so these bytes are the
// same forever and are cached as such.
func (s *Server) handleGetEventImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSession(r.Context(), id); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	seq := parseInt64(r.PathValue("seq"), 0)
	if seq <= 0 {
		http.Error(w, "seq must be a positive event sequence number", http.StatusBadRequest)
		return
	}
	// GetEventsAfter is exclusive on its cursor, so seq-1 asks for exactly
	// this row. A log with a hole in it — nothing appends one, but nothing
	// forbids one either — returns the next event instead, which the seq
	// check below rejects rather than serving somebody else's picture.
	events, err := s.Store.GetEventsAfter(r.Context(), id, seq-1, 1)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if len(events) == 0 || events[0].Seq != seq {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}
	ev := events[0]

	// Every way of not having an image is the same 404 with the same text:
	// the wrong kind of event, a tool result that carried none, a payload
	// that will not parse, a data URI that will not decode. A caller walking
	// seq numbers learns which events have pictures, which the transcript
	// told it anyway, and nothing else.
	if ev.Kind != store.KindToolResult {
		http.Error(w, "event carries no image", http.StatusNotFound)
		return
	}
	var payload store.ToolResultPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		http.Error(w, "event carries no image", http.StatusNotFound)
		return
	}
	mime, data, err := decodeImageDataURI(payload.ImageURL)
	if err != nil {
		http.Error(w, "event carries no image", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// An event never changes, so the browser may keep these bytes for as
	// long as it likes and never ask again. `private` because the log is a
	// session's own tool output and this port is the first thing anyone will
	// put behind a tunnel (docs/RUN-CONTROL.md) — cacheable in the one
	// browser that asked for it, never in anything shared.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	// The id/seq pair names an immutable row, so it is a complete validator
	// on its own. ServeContent answers If-None-Match from it, which is what
	// makes a reload of a long-lived tab free even after the entry expires.
	w.Header().Set("ETag", fmt.Sprintf("%q", id+"-"+fmt.Sprint(seq)))
	// ServeContent brings Range and HEAD with it, and leaves the
	// Content-Type set above alone rather than sniffing.
	http.ServeContent(w, r, "", ev.CreatedAt, bytes.NewReader(data))
}

const dataURIPrefix = "data:"

// imageDataMIMETypes is the set of types these bytes may be served as. It is
// the values of attachment.ImageMIMETypes — the same three types
// ReviewScreenshot accepts and handleGetScreenshot serves — so an event whose
// payload named some other type is a 404 rather than this endpoint becoming
// a way to get arbitrary Content-Type out of the log.
var imageDataMIMETypes = func() map[string]bool {
	set := make(map[string]bool, len(attachment.ImageMIMETypes))
	for _, mime := range attachment.ImageMIMETypes {
		set[mime] = true
	}
	return set
}()

// decodeImageDataURI splits a base64 image data URI into its MIME type and
// its bytes. It accepts only what internal/tools actually writes —
// "data:<image mime>;base64,<payload>" — so a URI with parameters, a
// percent-encoded payload, or a type outside the allowlist is an error
// rather than something to guess at.
func decodeImageDataURI(uri string) (mime string, data []byte, err error) {
	if !strings.HasPrefix(uri, dataURIPrefix) {
		return "", nil, errors.New("not a data URI")
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return "", nil, errors.New("data URI has no payload")
	}
	meta := uri[len(dataURIPrefix):comma]
	if !strings.HasSuffix(meta, ";base64") {
		return "", nil, errors.New("data URI is not base64")
	}
	mime = strings.TrimSuffix(meta, ";base64")
	if !imageDataMIMETypes[mime] {
		return "", nil, fmt.Errorf("data URI type %q is not an image type this endpoint serves", mime)
	}
	data, err = base64.StdEncoding.DecodeString(uri[comma+1:])
	if err != nil {
		return "", nil, fmt.Errorf("data URI payload: %w", err)
	}
	if len(data) == 0 {
		return "", nil, errors.New("data URI payload is empty")
	}
	return mime, data, nil
}
