package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/redact"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Small helpers with callers across the package rather than one resource:
// the two write guards, ?limit=/?offset= query parsing, the SSE frame
// writers evals.go's stream shares with sse.go's, and the three response
// shapes (writeJSON, and the two error bodies) every handler in the package
// ends on.

// writeGuards runs the two guards every write endpoint carries, in order, and
// reports whether the request may proceed: a JSON content type (415) and a
// same-origin check (403) (docs/DATA-API.md "The guards every write
// carries"). Settings, session, work-request, and lease
// handlers all call this one helper rather than repeating the pair, so the
// guard set is extended in one place, not in every handler.
func writeGuards(w http.ResponseWriter, r *http.Request) bool {
	if !requireJSONContentType(w, r) {
		return false
	}
	if !checkOrigin(w, r) {
		return false
	}
	return true
}

// requireJSONContentType refuses a write whose Content-Type is not
// application/json with 415 — a missing header included. A cross-origin form
// post cannot set that header without a preflight the gate rejects, so this
// and checkOrigin keep a web page from writing to a loopback port
// (docs/DESIGN.md §4.2).
func requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(mediaType) == "application/json" {
		return true
	}
	writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
	return false
}

// checkOrigin refuses a cross-origin write with 403. The port binds
// loopback by default, which stops a remote attacker and does nothing about a
// page open in the operator's own browser: any site can issue a cross-origin
// request to 127.0.0.1. When the request carries an Origin header it must
// match the request's own Host (scheme aside), so a same-origin fetch from
// the harness's own page passes and a fetch from any other site does not. A
// request with no Origin header is not browser-initiated and passes.
func checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Host != r.Host {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin write refused"})
		return false
	}
	return true
}

func parseInt64(v string, def int64) int64 {
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func parseInt(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeSSEEvent writes one store.Event as an SSE frame, seq as the id so
// the browser's EventSource resumes from it automatically on reconnect.
// writeSSEEvent writes one transcript event as an SSE frame. It redacts on
// the way out, exactly as handleGetEvents does — the stream and the page are
// the same data over two transports, and a secret masked on one and served
// on the other would be no masking at all.
func writeSSEEvent(w io.Writer, ev store.Event) {
	b, err := json.Marshal(redactEvent(ev))
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
}

// writeSSELive writes one uncommitted model-output delta as a *named* SSE
// event with no id. Both halves of that shape are load-bearing. The name
// keeps it off the browser's onmessage handler, which folds committed
// events into the transcript and would be corrupted by text that is about
// to arrive again in a real event; the missing id keeps it out of
// Last-Event-ID, which must only ever name a committed seq.
//
// It redacts on the same terms writeSSEEvent does: this is the same model
// output, arriving earlier, and a secret masked in the log but streamed in
// the clear here would be no masking at all.
func writeSSELive(w io.Writer, d hub.LiveDelta) {
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: live\ndata: %s\n\n", redact.Bytes(b))
}

// writeSSEReplayed marks the end of a transcript stream's history replay,
// before the first live event. It carries no data worth reading — the frame's
// arrival is the whole message — and, like a live delta, it is named and has
// no id so it neither reaches the client's onmessage nor moves the resume
// cursor. See handleSessionStream for why the client cannot work the seam out
// for itself.
func writeSSEReplayed(w io.Writer) {
	fmt.Fprint(w, "event: replayed\ndata: {}\n\n")
}

// writeSSEClosed marks a transcript stream the server is ending because the
// session will not append again. Like the replayed marker it is named, id-less
// and empty — its arrival is the whole message — and it exists for the same
// reason: the client cannot work this out for itself.
//
// It used to try. The browser closed its own EventSource the moment a
// run_finished or error event landed, on the reasoning that no more events
// could ever follow, which stopped a reconnect from polling a session with
// nothing left to say. Resume makes that inference wrong twice over
// (docs/RUN-CONTROL.md "Continuing"): a continued session appends after its
// terminal event, and every later replay of its history carries that old
// terminal event in the middle of the log, which would close a stream that is
// following a run currently in progress. Ending the stream is the server's
// call, so the server is what says so.
func writeSSEClosed(w io.Writer) {
	fmt.Fprint(w, "event: closed\ndata: {}\n\n")
}

// writeSSEState writes one session's metadata row as a *named* SSE event
// with no id, for the same two reasons a live delta carries that shape: the
// name keeps it off the client's onmessage handler, which folds committed
// events and would choke on a row, and the missing id keeps it out of
// Last-Event-ID, which must only ever name a committed seq.
//
// It redacts, exactly as the events beside it do. The row carries the task
// the run was launched with and the model's own summary of it, and a secret
// masked in the log but streamed in the clear here would be no masking at
// all.
func writeSSEState(w io.Writer, s hub.SessionState) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: state\ndata: %s\n\n", redact.Bytes(b))
}

// writeSSEData writes v as a plain SSE frame with no id — the shape the
// list stream uses, since a session-list row is a full replacement rather
// than a resumable log position. It redacts on the way out for the same
// reason writeSSEState does: a list row carries the task, and the two feeds
// serve the same data to the same browser.
func writeSSEData(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", redact.Bytes(b))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSessionLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeInternalError(w, err)
}

func writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("httpapi: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
