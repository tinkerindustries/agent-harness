// Package httpapi is the harness's HTTP surface (docs/DESIGN.md §4.2,
// docs/DATA-API.md): it reads and writes the data the harness manages and
// cannot reach the run loop. GET and HEAD are served on every path, including
// ones that do not exist; the writing methods are allowed where a write route
// exists — PUT and DELETE on a settings key, PATCH and DELETE on one session,
// and POST on the run-control subresources (docs/RUN-CONTROL.md). It serves
// the session list and metadata from the store, a paged read of one session's
// event log, two SSE streams — a per-session transcript and a quiet
// session-level list feed — fed by the in-process hub package rather than
// NATS, the settings table, and the work-request and workspace-lease rows.
// The write surface is the data the harness manages: closing an abandoned
// session, deleting a finished one, setting a key. Run control is a declared
// seam, not an import: stopping goes through the RunController interface
// below, satisfied by *worker.Pool without this package knowing the package
// exists, and nothing here starts a run. Steering is the one control that
// needs no seam at all — it is a store write by the handler and a store read
// by the loop, with the database as the boundary (docs/RUN-CONTROL.md
// "Steering: augment, don't gate").
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// sseKeepaliveInterval is how often an idle stream sends a ": keep-alive"
// comment, which also doubles as the poll interval for noticing a session
// that reached a terminal status without an event of its own to announce it
// (compaction retires the old session id silently; see handleSessionStream).
const sseKeepaliveInterval = 15 * time.Second

// sessionIdleThreshold is how long a running session must have been quiet
// before PATCH /api/sessions/{id} may close it (docs/DATA-API.md
// "Preconditions"). A live run appends events continuously, so a session
// whose most recent event is newer than this is presumed live and the close
// refuses with a 409 naming the last event's time. Ten minutes is the value
// used operating the harness by hand: long enough that a run paused on a
// slow tool call is never mistaken for dead, short enough that an abandoned
// row is closable without waiting out the work day. A named constant, never
// a literal, because the close endpoints and the tests both lean on it.
const sessionIdleThreshold = 10 * time.Minute

// terminalSessionStatuses are the statuses PATCH /api/sessions/{id} accepts:
// every status except running. Closing *into* running would be a resume,
// which is run control (docs/DATA-API.md) — the endpoint is for closing a
// session a dead worker left running, not for reviving one.
var terminalSessionStatuses = map[string]bool{
	store.StatusOK:        true,
	store.StatusFailed:    true,
	store.StatusTimeout:   true,
	store.StatusMaxTurns:  true,
	store.StatusCancelled: true,
	store.StatusCompacted: true,
}

// terminalSessionStatusList is the same set in a stable order, for the 400
// message that names the accepted values.
var terminalSessionStatusList = []string{
	store.StatusOK, store.StatusFailed, store.StatusTimeout,
	store.StatusMaxTurns, store.StatusCancelled, store.StatusCompacted,
}

// terminalRequestStatuses are the statuses PATCH /api/requests/{request_id}
// accepts: the terminal statuses a work request can hold (docs/DESIGN.md
// §4.10) — everything except running, which is the only status a live worker
// can hold and the one this endpoint exists to retire. "cancelled" is
// included even though nothing in the worker produces it: closing a dead
// request is the operator's analogue of closing a dead session, and the
// operator's status vocabulary is the session one.
var terminalRequestStatuses = map[string]bool{
	"ok":        true,
	"failed":    true,
	"denied":    true,
	"timeout":   true,
	"cancelled": true,
}

// terminalRequestStatusList is the same set in a stable order, for the 400
// message that names the accepted values.
var terminalRequestStatusList = []string{
	"ok", "failed", "denied", "timeout", "cancelled",
}

const (
	// defaultEventsLimit and maxEventsLimit are the built-in paging bounds
	// when a Server is built without the fields (the test path). Production
	// resolves http.events_limit_default and http.events_limit_max from the
	// settings registry at startup and sets the Server fields; the values
	// are pinned equal by internal/settings/registry_test.go.
	defaultEventsLimit = 500
	maxEventsLimit     = 5000
)

// QueueConsumer is the subset of *jetstream.Consumer the queue health
// endpoint reads. A narrow interface here, rather than a direct
// jetstream.Consumer field, is what lets a test supply a fake with no real
// NATS server behind it.
type QueueConsumer interface {
	Info(ctx context.Context) (*jetstream.ConsumerInfo, error)
}

// QueuePool is the subset of *worker.Pool's halted state the queue health
// endpoint reads. A narrow interface here, rather than an import of
// internal/worker, keeps this package's dependency pointed at the one
// method it needs rather than a whole package.
type QueuePool interface {
	Halted() (bool, string)
}

// RunController is the subset of the worker pool that run control needs,
// declared here rather than importing internal/worker — the same shape as
// QueuePool above: a narrow interface named for what it does, implemented by
// *worker.Pool without this package knowing the package exists, so the import
// boundary ARCHITECTURE.md calls structural stays structural (docs/RUN-CONTROL.md
// "Stopping is addressed at one goroutine, so the seam is a registry").
type RunController interface {
	// Stop begins ending sessionID and returns immediately: it cancels the
	// run's context, and a stop already in progress is not an error (a second
	// stop is another 202, not a 409). It reports ErrRunNotFound when no run
	// in this process owns that session. The handler must not parse the
	// error's text to tell those apart — it asks Running first and treats any
	// error from Stop as a 500 carrying the message.
	Stop(sessionID, reason string) error
	// Running reports whether this process is running sessionID.
	Running(sessionID string) bool
}

// Server holds the things every handler reads: the store, for everything
// historical; the hub, for everything live; and, optionally, the queue's
// consumer and pool, for /api/queue's consumer lag, in-flight count, and
// redelivery count (docs/DESIGN.md §5.8). The write surface is the data the
// harness manages (docs/DATA-API.md): the settings endpoints and the session
// close/delete endpoints read and write through Store, and nothing else in
// Server is mutated by a request — the run surface stays read-only except
// for the two run-control endpoints: stop, which acts on a run through the
// RunController seam, and steer, which is a plain store write the session
// loop reads at its next sub-turn boundary and needs no seam at all
// (docs/RUN-CONTROL.md). Consumer and Pool are nil in any caller that has no
// queue at all (a CLI-only harness never wires one up); the handler degrades
// to reporting the queue as unavailable rather than panicking. Run is nil the
// same way in a caller with no pool, and the stop handler then answers 409
// for every existing session — this process is running nothing. ControlToken
// is the process's copy of http.control_token; empty means run control is not
// configured and the run-control endpoints fail closed with 503.
type Server struct {
	Store          *store.Store
	Hub            *hub.Hub
	Static         http.Handler
	Consumer       QueueConsumer
	Pool           QueuePool
	Run            RunController
	ControlToken   string
	PriceTableDate string
	Settings       *settings.Resolver

	// DefaultEventsLimit and MaxEventsLimit bound ?limit= on the events
	// endpoint. They are resolved from http.events_limit_default and
	// http.events_limit_max once at startup: a change needs a restart, which
	// the settings screen marks. Zero (the test path) falls back to the
	// package constants.
	DefaultEventsLimit int
	MaxEventsLimit     int
}

// Handler returns the harness's whole HTTP surface. methodGate runs before
// routing, so a request outside the method allowlist is rejected on every
// path, including ones nothing here recognises: GET and HEAD everywhere, and
// the writing methods where a write route exists — PUT and DELETE on a
// settings key path, PATCH and DELETE on one session's path, and POST on the
// run-control subresources (docs/DESIGN.md §4.2, docs/DATA-API.md,
// docs/RUN-CONTROL.md).
func (s *Server) Handler() http.Handler {
	return methodGate(s.routes())
}

// routes builds the router: every pattern the surface serves, method and
// path. Handler wraps it in the method gate; the bare router is also
// exposed for the test that asserts the events resource carries no
// mutating route (docs/DATA-API.md "Events are not writable") — a route
// registered on it later fails that test rather than passing behind the
// gate unnoticed.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.handleStopSession)
	mux.HandleFunc("POST /api/sessions/{id}/steer", s.handleSteerSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleGetEvents)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleSessionStream)
	mux.HandleFunc("GET /api/control-token", s.handleGetControlToken)
	mux.HandleFunc("GET /api/requests", s.handleListWorkRequests)
	mux.HandleFunc("GET /api/requests/{request_id}", s.handleGetWorkRequest)
	mux.HandleFunc("PATCH /api/requests/{request_id}", s.handlePatchWorkRequest)
	mux.HandleFunc("DELETE /api/requests/{request_id}", s.handleDeleteWorkRequest)
	mux.HandleFunc("GET /api/requests/{request_id}/status", s.handleRequestStatus)
	mux.HandleFunc("GET /api/leases", s.handleListLeases)
	// The lease key is a workspace path, which contains slashes, so the
	// segment is the rest of the path after /api/leases/ — a client
	// percent-encodes each slash (DELETE /api/leases/%2Ftmp%2Fws) and the
	// wildcard matches the remainder exactly (docs/DATA-API.md phase 3).
	mux.HandleFunc("DELETE /api/leases/{workspace...}", s.handleDeleteLease)
	mux.HandleFunc("GET /api/stream", s.handleListStream)
	mux.HandleFunc("GET /api/queue", s.handleQueueHealth)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings/{key}", s.handlePutSetting)
	mux.HandleFunc("DELETE /api/settings/{key}", s.handleDeleteSetting)
	mux.Handle("/", s.Static)
	return mux
}

// methodGate enforces the method allowlist ahead of any routing decision. GET
// and HEAD pass on every path; the writing methods pass only where a write
// route exists — PUT and DELETE on a settings key path, PATCH and DELETE on a
// session or work-request path, DELETE on a lease path, and POST on the
// run-control subresources (docs/DATA-API.md, docs/RUN-CONTROL.md). A
// pattern registered with a method already 405s a wrong-method request that
// matches its path (net/http's ServeMux does this since Go 1.22), but that
// only covers paths this package recognises; the static handler's "/" pattern
// matches everything, method or not, and a POST to a path nobody registered
// would otherwise fall through to a 404 rather than the 405 docs/DESIGN.md
// §4.2 requires everywhere else.
//
// The gate decides against the escaped path, matching how the mux matches:
// a workspace lease key is a path containing slashes, and a client
// percent-encodes them (DELETE /api/leases/%2Ftmp%2Fws), so the escaped form
// is the one whose shape identifies the route.
func methodGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		if method == http.MethodGet || method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.EscapedPath()
		if writeAllowed(method, path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", allowedMethods(path))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
}

// writeAllowed reports whether method is a writing method the surface allows
// on path. The sets live here, one place, so a phase that adds a resource
// extends this switch rather than the gate itself.
func writeAllowed(method, path string) bool {
	switch {
	case isSettingsKeyPath(path):
		return method == http.MethodPut || method == http.MethodDelete
	case isSessionPath(path):
		return method == http.MethodPatch || method == http.MethodDelete
	case isRequestPath(path):
		return method == http.MethodPatch || method == http.MethodDelete
	case isLeasePath(path):
		return method == http.MethodDelete
	case isStopPath(path):
		return method == http.MethodPost
	case isSteerPath(path):
		return method == http.MethodPost
	default:
		return false
	}
}

// isSettingsKeyPath reports whether path is one key's settings resource —
// /api/settings/<key>, exactly one key segment. PUT and DELETE may pass the
// gate here and nowhere else; the mux's own PUT/DELETE patterns then handle
// it, and the handler rejects a key outside settings.ValidKeys.
func isSettingsKeyPath(path string) bool {
	const prefix = "/api/settings/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isSessionPath reports whether path is exactly one session's resource —
// /api/sessions/<id> with no further segments. The collection
// (/api/sessions) and the event and stream subresources carry their own
// rules: events are never writable (docs/DATA-API.md).
func isSessionPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isStopPath reports whether path is one session's stop subresource —
// /api/sessions/<id>/stop, exactly one id segment and the literal "stop".
// POST may pass the gate here and nowhere else. It is deliberately separate
// from isSessionPath, which requires no further segments and must keep PATCH
// and DELETE scoped to the row: a stop is an action on a run, not an edit of
// a row (docs/RUN-CONTROL.md "The HTTP surface").
func isStopPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && tail == "stop"
}

// isSteerPath reports whether path is one session's steer subresource —
// /api/sessions/<id>/steer, exactly one id segment and the literal "steer".
// POST may pass the gate here and nowhere else, the same shape rule as
// isStopPath: it is an action on a run (docs/RUN-CONTROL.md "The HTTP
// surface"), not an edit of the row, and isSessionPath must not be widened to
// cover it.
func isSteerPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && tail == "steer"
}

// isRequestPath reports whether path is exactly one work request's resource —
// /api/requests/<request_id> with no further segments. The poll snapshot
// subresource /api/requests/<request_id>/status is a read and carries its
// own rule (it is never writable), so the gate keeps it out of the write
// routes.
func isRequestPath(path string) bool {
	const prefix = "/api/requests/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isLeasePath reports whether path is one workspace lease's resource — the
// remainder of /api/leases/ is the lease key, which is a workspace path and
// may itself contain slashes, percent-encoded by the client
// (/api/leases/%2Ftmp%2Fws). The collection (/api/leases) is read-only.
func isLeasePath(path string) bool {
	const prefix = "/api/leases/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != ""
}

// allowedMethods names the methods the surface actually allows for path, for
// the Allow header on a rejected request. Only paths with a write route allow
// the writing methods; every other path is GET and HEAD.
func allowedMethods(path string) string {
	switch {
	case isSettingsKeyPath(path):
		return "GET, HEAD, PUT, DELETE"
	case isSessionPath(path):
		return "GET, HEAD, PATCH, DELETE"
	case isRequestPath(path):
		return "GET, HEAD, PATCH, DELETE"
	case isLeasePath(path):
		return "GET, HEAD, DELETE"
	case isStopPath(path):
		return "GET, HEAD, POST"
	case isSteerPath(path):
		return "GET, HEAD, POST"
	default:
		return "GET, HEAD"
	}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Store.ListSessions(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	states, err := s.buildStates(r.Context(), sessions)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	states, err := s.buildStates(r.Context(), []store.Session{sess})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states[0])
}

// buildStates assembles the session-list wire row for each of sessions in
// two batch queries rather than one round trip per session, so the list
// endpoint stays cheap as the number of sessions grows.
func (s *Server) buildStates(ctx context.Context, sessions []store.Session) ([]hub.SessionState, error) {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	summaries, err := s.Store.SessionUsageSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	requestIDs, err := s.Store.RequestIDsForSessions(ctx, ids)
	if err != nil {
		return nil, err
	}
	states := make([]hub.SessionState, len(sessions))
	for i, sess := range sessions {
		states[i] = hub.BuildSessionState(sess, summaries[sess.ID], requestIDs[sess.ID], s.PriceTableDate)
	}
	return states, nil
}

// --- session writes ---

// patchSessionBody is the JSON body PATCH /api/sessions/{id} accepts: the
// terminal status to close the session into (docs/DATA-API.md).
type patchSessionBody struct {
	Status string `json:"status"`
}

// handlePatchSession serves PATCH /api/sessions/{id}: closes a session a dead
// worker left running by transitioning it to a terminal status and setting
// finished_at (docs/DATA-API.md). It is the write the read-only rule was
// retired for — an abandoned row still says "running" and nothing else ever
// closes it.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; a body whose status is a terminal status (400
// otherwise, naming the accepted values); the If-Match version (428 missing,
// 412 stale — checked inside the store's write transaction so no interleaving
// write can race it); and the idle precondition — a running session whose most
// recent event is newer than sessionIdleThreshold is presumed live and
// refused with a 409 naming the last event's time. Success is 200 with the
// updated session row (including its new version), also fanned out to the
// /api/stream list feed so open session lists update.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	var body patchSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"status": "<terminal status>"}`})
		return
	}
	if !terminalSessionStatuses[body.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("status must be one of %s", strings.Join(terminalSessionStatusList, ", "))})
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	sess, err := s.Store.CloseSession(r.Context(), r.PathValue("id"), body.Status, want, time.Now(), sessionIdleThreshold)
	if err != nil {
		writeSessionWriteError(w, err)
		return
	}
	states, err := s.buildStates(r.Context(), []store.Session{sess})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states[0])
	s.Hub.PublishSessionState(states[0])
}

// handleDeleteSession serves DELETE /api/sessions/{id}: removes the session
// row and its whole event log (docs/DATA-API.md), surfacing
// store.DeleteSession — which refuses a running session regardless of
// idleness, so an abandoned row must be closed with PATCH before it can be
// deleted. It carries the content-type and origin guards and requires the
// If-Match version (428 missing, 412 stale); the version check and the
// running refusal both happen inside the store's write transaction.
// Success is 200 {"ok": true}.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteSession(r.Context(), r.PathValue("id"), want); err != nil {
		writeSessionWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// parseIfMatch reads the If-Match header every mutating write carries
// (docs/DATA-API.md "Optimistic concurrency"): the version from the resource
// representation, echoed back. A missing header is 428 Precondition Required
// — the write cannot be proven fresh without it. A value that is not a
// positive integer is a 400: the header is malformed, and the client should
// fix its request rather than re-read. Whether the value *matches* the row is
// the store's call, checked inside the write transaction.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	header := r.Header.Get("If-Match")
	if header == "" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "If-Match header required: echo the version from the resource"})
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || v < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "If-Match must be the version integer from the resource"})
		return 0, false
	}
	return v, true
}

// writeSessionWriteError maps a store error from a session write to the wire:
// not found is 404, a live or running session is 409, a version mismatch is
// 412 — each carrying the store's own message, which names the last event's
// time or the current version — and anything else is a 500 like every other
// handler (docs/DATA-API.md "Error shape").
func writeSessionWriteError(w http.ResponseWriter, err error) {
	var active *store.ActiveSessionError
	var running *store.SessionRunningError
	var conflict *store.VersionConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
	case errors.As(err, &active):
		writeJSON(w, http.StatusConflict, map[string]string{"error": active.Error()})
	case errors.As(err, &running):
		writeJSON(w, http.StatusConflict, map[string]string{"error": running.Error()})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": conflict.Error()})
	default:
		writeInternalError(w, err)
	}
}

// --- run control: stop (phase 4) ---

// stopSessionBody is the JSON body POST /api/sessions/{id}/stop accepts: the
// operator's reason, optional, carried verbatim as the message of the
// cancelled result (docs/RUN-CONTROL.md "The HTTP surface").
type stopSessionBody struct {
	Reason string `json:"reason"`
}

// handleStopSession serves POST /api/sessions/{id}/stop: asks this process's
// worker pool to end the run, whatever state it is in — a healthy run ends at
// its next check point, a wedged one is force-finished after the grace period
// (docs/RUN-CONTROL.md "Stopping"). The whole path is non-blocking: the 202 is
// the acceptance, and the terminal state arrives over the session's own SSE
// stream.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured — a missing credential fails closed); the session
// existing in the store (404); this process running it (409 naming the
// session's actual status — the honest answer for a session that already
// finished, and for one being run by nothing at all); and 202
// {"session_id", "stopping": true} otherwise. A second stop for a run already
// stopping is another 202, not a 409: stopping is idempotent
// (docs/RUN-CONTROL.md "The HTTP surface").
//
// No If-Match, deliberately: that rule (docs/DATA-API.md "Optimistic
// concurrency") governs mutations of a row — it stops an operator's write
// landing on a row that changed since they read it. A stop is an action on a
// run, not an edit of a row, and a running session's version changes
// continuously underneath the caller, so requiring a version echo would make
// a correct stop racy by construction.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	var body stopSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"reason": "..."}`})
		return
	}
	// The registry is asked before the store, and the order is load-bearing. A
	// run is registered before its workspace is prepared, and its session row
	// is not created until the session loop starts — so a run wedged in a
	// git clone is registered, stoppable, and has no row to look up. Asking
	// the store first would answer 404 for exactly the run an operator most
	// needs to end (docs/RUN-CONTROL.md "Half two": the escalation has its own
	// branch for a stop that finds no session row to mark).
	//
	// The controller is nil in any caller that has no pool; that caller is
	// running nothing, and every session falls through to the store below.
	id := r.PathValue("id")
	if s.Run != nil && s.Run.Running(id) {
		if err := s.Run.Stop(id, body.Reason); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": id, "stopping": true})
		return
	}

	// Not running here: the store decides whether that is a 404 for a session
	// nobody has heard of, or a 409 for one this process finished or never
	// ran, naming the status it actually holds.
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("session %s is not running in this process (status %s)", sess.ID, sess.Status),
	})
}

// steerSessionBody is the JSON body POST /api/sessions/{id}/steer accepts:
// the operator's instruction, verbatim, plus an optional source naming where
// it came from — "web", "mcp", or "cli" — which defaults to "web" when
// absent (the browser is the HTTP surface's primary client). The text is
// carried verbatim into the user message the loop folds, so a caller's
// formatting survives (docs/RUN-CONTROL.md "Steering").
type steerSessionBody struct {
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// handleSteerSession serves POST /api/sessions/{id}/steer: appends a
// steer_message event to the session's log for the loop to pick up at its
// next sub-turn boundary (docs/RUN-CONTROL.md "Steering"). The loop reads the
// store once per sub-turn and never blocks on it, so this handler does not
// touch the RunController the stop handler needs — a reader who just read
// that handler will expect one, and it is deliberately absent: steering is a
// store write by the handler and a store read by the loop, with the
// database — which every replica shares — as the seam. The event is fanned
// out to the session's SSE stream so the transcript shows it immediately as
// a pending steer block.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured); a body whose text is empty or whitespace only (400 —
// an empty steer would be a user message with nothing to say); the session
// existing in the store (404); the session being `running` (409 — a steer for
// a finished run would sit in the log forever, unapplied and unexplained);
// and 202 {"session_id", "seq"} otherwise, where seq is the sequence number
// the steer_message landed at. Unlike stop, steering is deliberately NOT
// idempotent: two steers are two instructions, which is why the response
// carries the seq the caller's text landed at.
//
// No If-Match, for the same reason as stop: this is an action on a run, not
// an edit of a row, and a running session's version changes continuously
// underneath the caller (docs/RUN-CONTROL.md "The HTTP surface").
func (s *Server) handleSteerSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	var body steerSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"text": "..."}`})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text must not be empty"})
		return
	}
	id := r.PathValue("id")
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	if sess.Status != store.StatusRunning {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("session %s is not running (status %s); a steer needs a running run", sess.ID, sess.Status),
		})
		return
	}

	source := body.Source
	if source == "" {
		source = "web"
	}
	appended, err := s.Store.AppendEvents(r.Context(), id, []store.EventInput{{
		Kind:    store.KindSteerMessage,
		Payload: store.SteerMessagePayload{Text: body.Text, Source: source},
	}})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Fan the event out to the session's transcript stream the way the runner
	// fans out its own commits, so the browser's steer block appears the
	// moment the steer is accepted rather than at the next sub-turn.
	s.Hub.PublishEvents(id, appended)
	writeJSON(w, http.StatusAccepted, map[string]any{"session_id": id, "seq": appended[0].Seq})
}

// requireControlToken enforces the bearer token the run-control endpoints
// carry (docs/RUN-CONTROL.md "Authentication"). The token is the process's
// own copy of http.control_token, generated and stored at startup by serve
// when the setting is empty; a Server built without that generation (every
// test that does not set one) has an empty token and answers 503 — a missing
// credential must fail closed, never let the request through.
//
// What the token buys and does not buy, plainly: nothing against another
// process running as the same user, which can read the settings table;
// everything on the day the port is exposed off loopback — deliberately, by a
// `-addr 0.0.0.0`, or by a container port publish — which is the case
// docs/DESIGN.md §4.2 names as the one stage two forces. The comparison is
// constant time, so a timing side channel cannot probe the token byte by
// byte.
func (s *Server) requireControlToken(w http.ResponseWriter, r *http.Request) bool {
	if s.ControlToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "run control is not configured: no control token has been generated (start harness serve once, or set http.control_token)",
		})
		return false
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "missing bearer token: run-control endpoints require Authorization: Bearer <token>",
		})
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.ControlToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
		return false
	}
	return true
}

// handleGetControlToken serves GET /api/control-token: the run-control bearer
// token, to a loopback caller only (docs/RUN-CONTROL.md "Authentication").
// This is how the browser and a same-host MCP server get the token in phase
// 4b; a caller that is not on this machine has to be given it out of band,
// which is the property that makes the token worth having. A non-loopback
// caller is 403 with no hint about whether a token exists at all.
func (s *Server) handleGetControlToken(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackAddr(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "the control token is only served to loopback callers"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": s.ControlToken})
}

// isLoopbackAddr reports whether remoteAddr (an "ip:port" string from
// http.Request.RemoteAddr) is a loopback address: 127.0.0.0/8 or ::1 by the
// address, or the name "localhost". It is what gates GET /api/control-token:
// the token must not leave this machine over the wire.
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- work-request and workspace-lease writes (phase 3) ---

// workRequestRow is one work_requests row over HTTP: the idempotency row
// itself — request id, session id, status, result JSON, received_at,
// finished_at, delivery count — plus the version every row resource carries
// (docs/DATA-API.md). It is the row, distinct from the polled snapshot
// /api/requests/{request_id}/status serves; it answers "what does the table
// say" rather than "what is the run doing right now".
type workRequestRow struct {
	RequestID     string          `json:"request_id"`
	SessionID     string          `json:"session_id,omitempty"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	ReceivedAt    time.Time       `json:"received_at"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
	DeliveryCount int             `json:"delivery_count"`
	Version       int             `json:"version"`
}

func workRequestRowFrom(wr store.WorkRequest) workRequestRow {
	return workRequestRow{
		RequestID:     wr.RequestID,
		SessionID:     wr.SessionID,
		Status:        wr.Status,
		Result:        wr.Result,
		ReceivedAt:    wr.ReceivedAt,
		FinishedAt:    wr.FinishedAt,
		DeliveryCount: wr.DeliveryCount,
		Version:       wr.Version,
	}
}

// handleListWorkRequests serves GET /api/requests: every work_requests row,
// newest first by received_at — the work-request analog of the sessions
// list's order (docs/DATA-API.md). Each row is the same shape
// GET /api/requests/{request_id} returns, including the version a write
// must echo back in If-Match. The collection is how an operator finds a
// request whose worker died during workspace preparation: it never got a
// session, so it has no session to be discovered through, only this row. It
// is a read, so it carries no write guards.
func (s *Server) handleListWorkRequests(w http.ResponseWriter, r *http.Request) {
	requests, err := s.Store.ListWorkRequests(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows := make([]workRequestRow, 0, len(requests))
	for _, wr := range requests {
		rows = append(rows, workRequestRowFrom(wr))
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleGetWorkRequest(w http.ResponseWriter, r *http.Request) {
	wr, err := s.Store.GetWorkRequest(r.Context(), r.PathValue("request_id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "request not found", http.StatusNotFound)
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workRequestRowFrom(wr))
}

// handlePatchWorkRequest serves PATCH /api/requests/{request_id}: closes a
// request a dead worker left running by transitioning it to a terminal status
// and setting finished_at (docs/DATA-API.md phase 3). It is the work-request
// analogue of PATCH /api/sessions/{id}: an abandoned row still says "running"
// and nothing else ever closes it.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; a body whose status is a terminal status (400
// otherwise, naming the accepted values); the If-Match version (428 missing,
// 412 stale — checked inside the store's write transaction so no interleaving
// write can race it); and the in-flight precondition — a request whose
// session's most recent event is newer than sessionIdleThreshold is being run
// by a live pool worker right now and is refused with a 409 naming the last
// event's time. A request with no session id has never run and passes. A
// request already terminal keeps its status — a re-close is a version bump
// and nothing else, so a retried write is idempotent. Success is 200 with the
// updated row.
func (s *Server) handlePatchWorkRequest(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	var body patchSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"status": "<terminal status>"}`})
		return
	}
	if !terminalRequestStatuses[body.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("status must be one of %s", strings.Join(terminalRequestStatusList, ", "))})
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	wr, err := s.Store.CloseWorkRequest(r.Context(), r.PathValue("request_id"), body.Status, want, time.Now(), sessionIdleThreshold)
	if err != nil {
		writeRequestWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workRequestRowFrom(wr))
}

// handleDeleteWorkRequest serves DELETE /api/requests/{request_id}: removes
// the work_requests row (docs/DATA-API.md phase 3). It carries the same
// guards as the other writes, requires the If-Match version, and refuses a
// request whose session is still live with a 409 naming the last event's time
// — deleting the row of a run a live worker is finishing would swallow the
// worker's result. A dead request — one whose session is idle or absent — may
// be deleted directly. Success is 200 {"ok": true}.
func (s *Server) handleDeleteWorkRequest(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteWorkRequest(r.Context(), r.PathValue("request_id"), want, time.Now(), sessionIdleThreshold); err != nil {
		writeRequestWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeRequestWriteError maps a store error from a work-request write to the
// wire: not found is 404, a request in flight is 409, a version mismatch is
// 412 — each carrying the store's own message, which names the last event's
// time or the current version — and anything else is a 500 like every other
// handler (docs/DATA-API.md "Error shape").
func writeRequestWriteError(w http.ResponseWriter, err error) {
	var active *store.ActiveRequestError
	var conflict *store.VersionConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
	case errors.As(err, &active):
		writeJSON(w, http.StatusConflict, map[string]string{"error": active.Error()})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": conflict.Error()})
	default:
		writeInternalError(w, err)
	}
}

// workspaceLeaseRow is one workspace_leases row over HTTP (docs/DATA-API.md
// phase 3): the workspace, the session holding it, when it was acquired, when
// it was last heartbeated, and the version every row resource carries.
type workspaceLeaseRow struct {
	Workspace   string    `json:"workspace"`
	SessionID   string    `json:"session_id"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Version     int       `json:"version"`
}

func workspaceLeaseRowFrom(l store.WorkspaceLease) workspaceLeaseRow {
	return workspaceLeaseRow{
		Workspace:   l.Workspace,
		SessionID:   l.SessionID,
		AcquiredAt:  l.AcquiredAt,
		HeartbeatAt: l.HeartbeatAt,
		Version:     l.Version,
	}
}

func (s *Server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	leases, err := s.Store.ListWorkspaceLeases(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows := make([]workspaceLeaseRow, 0, len(leases))
	for _, l := range leases {
		rows = append(rows, workspaceLeaseRowFrom(l))
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleDeleteLease serves DELETE /api/leases/{workspace}: releases a
// workspace lease (docs/DATA-API.md phase 3). It carries the content-type and
// origin guards, requires the If-Match version (428 missing, 412 stale), and
// refuses a lease whose heartbeat is newer than sessionIdleThreshold with a
// 409 naming the last heartbeat — the lease analog of the session endpoints'
// active refusal: a live session heartbeats its lease, so a recent heartbeat
// means the workspace is in use right now. Success is 200 {"ok": true}.
func (s *Server) handleDeleteLease(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteWorkspaceLease(r.Context(), r.PathValue("workspace"), want, time.Now(), sessionIdleThreshold); err != nil {
		writeLeaseWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeLeaseWriteError maps a store error from a lease write to the wire: not
// found is 404, a live lease is 409 naming the last heartbeat, a version
// mismatch is 412, and anything else is a 500 like every other handler.
func writeLeaseWriteError(w http.ResponseWriter, err error) {
	var active *store.ActiveLeaseError
	var conflict *store.VersionConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease not found"})
	case errors.As(err, &active):
		writeJSON(w, http.StatusConflict, map[string]string{"error": active.Error()})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": conflict.Error()})
	default:
		writeInternalError(w, err)
	}
}

// requestStatus is GET /api/requests/{request_id}/status's response: a
// polled snapshot of one work request, keyed on request_id rather than
// session id so a request that never got a session still has an answer.
// DurationMS is elapsed time since the request was claimed; for a terminal
// request that is the time between claim and finish. TranscriptURL is
// relative, the same /sessions/{id} shape the frontend links.
type requestStatus struct {
	RequestID     string                 `json:"request_id"`
	SessionID     string                 `json:"session_id"`
	Status        string                 `json:"status"`
	SubTurn       int                    `json:"sub_turn,omitempty"`
	Todos         []store.StatusTodo     `json:"todos,omitempty"`
	ActiveForm    string                 `json:"active_form,omitempty"`
	ToolCalls     []store.StatusToolCall `json:"tool_calls,omitempty"`
	Usage         *store.UsagePayload    `json:"usage,omitempty"`
	StartedAt     time.Time              `json:"started_at"`
	DurationMS    int64                  `json:"duration_ms"`
	TranscriptURL string                 `json:"transcript_url,omitempty"`
	ErrorCode     string                 `json:"error_code,omitempty"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
}

func (s *Server) handleRequestStatus(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("request_id")
	st, err := s.Store.RequestStatus(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "request not found", http.StatusNotFound)
			return
		}
		writeInternalError(w, err)
		return
	}

	var duration time.Duration
	if st.FinishedAt != nil {
		duration = st.FinishedAt.Sub(st.StartedAt)
	} else {
		duration = time.Since(st.StartedAt)
	}
	if duration < 0 {
		duration = 0
	}

	out := requestStatus{
		RequestID:    st.RequestID,
		SessionID:    st.SessionID,
		Status:       st.Status,
		SubTurn:      st.SubTurn,
		Todos:        st.Todos,
		ActiveForm:   st.ActiveForm,
		ToolCalls:    st.ToolCalls,
		Usage:        st.Usage,
		StartedAt:    st.StartedAt,
		DurationMS:   duration.Milliseconds(),
		ErrorCode:    st.ErrorCode,
		ErrorMessage: st.ErrorMessage,
	}
	if st.SessionID != "" {
		out.TranscriptURL = "/sessions/" + st.SessionID
	}
	writeJSON(w, http.StatusOK, out)
}

// queueHealth is /api/queue's response shape: consumer lag, in-flight
// count, and redelivery count, which the session list shows as queue
// health, plus whether the pool has halted itself and why — the
// visible form of docs/DESIGN.md §4.5's "A 402 stops the pool". Available
// is false whenever there is nothing to report from, which happens for any
// harness that has no queue wired up (Consumer nil) or when the live NATS
// call itself fails; Error then carries why.
type queueHealth struct {
	Available   bool   `json:"available"`
	ConsumerLag uint64 `json:"consumer_lag,omitempty"`
	InFlight    int    `json:"in_flight,omitempty"`
	Redelivered int    `json:"redelivered,omitempty"`
	Halted      bool   `json:"halted"`
	HaltReason  string `json:"halt_reason,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (s *Server) handleQueueHealth(w http.ResponseWriter, r *http.Request) {
	var health queueHealth
	if s.Pool != nil {
		health.Halted, health.HaltReason = s.Pool.Halted()
	}
	if s.Consumer == nil {
		writeJSON(w, http.StatusOK, health)
		return
	}
	info, err := s.Consumer.Info(r.Context())
	if err != nil {
		health.Error = err.Error()
		writeJSON(w, http.StatusOK, health)
		return
	}
	health.Available = true
	health.ConsumerLag = info.NumPending
	health.InFlight = info.NumAckPending
	health.Redelivered = info.NumRedelivered
	writeJSON(w, http.StatusOK, health)
}

// --- settings ---

// settingEntry is one row of GET /api/settings: the registry descriptor
// (group, type, default, description, secret, restart) plus the run's own
// state — whether it is set, whether the current value is an override or the
// default, and the display value. A secret key (settings.IsSecretKey) is
// masked to at most its last four characters, exactly as harness config list
// masks it; the full value never leaves the process over HTTP. An unset key
// omits value.
//
// Min and Max carry the validation bounds in display form and are present
// only for the types that have them — a JSON number for TypeInteger, Go
// duration text ("1s", "24h") for TypeDuration, nothing for TypeString — so a
// plain string setting's payload does not grow a pair of meaningless zeroes.
// The screen shows the bound; the registry still enforces it. Allowed, when
// non-empty, is a TypeString setting's closed set of accepted values
// (model.effort), serialised so the screen can render a ToggleGroup instead
// of a text input.
type settingEntry struct {
	Key         string   `json:"key"`
	Group       string   `json:"group"`
	Type        string   `json:"type"`
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret"`
	Restart     bool     `json:"restart"`
	Set         bool     `json:"set"`
	Override    bool     `json:"override"`
	Value       string   `json:"value,omitempty"`
	Min         any      `json:"min,omitempty"`
	Max         any      `json:"max,omitempty"`
	Allowed     []string `json:"allowed,omitempty"`
}

// handleGetSettings serves GET /api/settings: every key in settings.ValidKeys
// in order (registry order, grouped), each with its descriptor and whether
// it is set, its display value, and whether the stored value differs from
// the default. The override flag is computed here, where the real value is
// known — a masked secret can never be compared client-side. There is
// deliberately no reveal parameter — the full secret never leaves the
// process over HTTP; the CLI's `config get -reveal` is for an operator at a
// terminal.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	entries := make([]settingEntry, 0, len(settings.ValidKeys))
	for _, d := range settings.Descriptors() {
		value, ok, err := s.Settings.Get(r.Context(), d.Key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		entry := settingEntry{
			Key: d.Key, Group: d.Group, Type: d.Type.String(), Default: d.Default,
			Description: d.Description, Secret: d.Secret, Restart: d.Restart,
			Set: ok, Override: ok && value != d.Default,
		}
		if d.Type == settings.TypeInteger {
			entry.Min, entry.Max = d.Min, d.Max
		} else if d.Type == settings.TypeDuration {
			// A duration's Min and Max are stored as nanoseconds; the screen
			// wants "1s" and "24h", not 1000000000 and 86400000000000, and the
			// bound is display-only (validation stays in the registry), so
			// send the text Go already knows how to format.
			entry.Min, entry.Max = durationText(time.Duration(d.Min)), durationText(time.Duration(d.Max))
		}
		if len(d.Allowed) > 0 {
			entry.Allowed = d.Allowed
		}
		if ok {
			entry.Value = value
			if d.Secret {
				entry.Value = maskSecret(value)
			}
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, entries)
}

// durationText formats a duration bound the way the settings screen shows it
// — "1s", "2m", "24h" — rather than time.Duration.String()'s "24h0m0s".
// Every bound in the registry is a whole number of seconds, and a range
// rendered as "24h0m0s" reads like a bug next to a field whose own values
// are "30s", "10m", "1h".
func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return d.String()
	}
}

// putSettingBody is the JSON body PUT /api/settings/{key} accepts.
type putSettingBody struct {
	Value string `json:"value"`
}

// handlePutSetting serves PUT /api/settings/{key}: writes key through the
// settings resolver. An unknown key is a 400 carrying UnknownKeyError's
// message; a missing or non-JSON content type is a 415; and a cross-origin
// request (an Origin header that does not match the request's own Host) is a
// 403 — the guards that keep a page open in the operator's browser from
// writing keys to a loopback port (docs/DESIGN.md §4.2, docs/DATA-API.md).
func (s *Server) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	var body putSettingBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"value": "..."}`})
		return
	}
	if err := s.Settings.Set(r.Context(), r.PathValue("key"), body.Value); err != nil {
		writeSettingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteSetting serves DELETE /api/settings/{key}: unsets key, mirroring
// harness config unset. Deleting an unset key is not an error. It carries the
// same content-type and origin guards as PUT (docs/DESIGN.md §4.2).
func (s *Server) handleDeleteSetting(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if err := s.Settings.Unset(r.Context(), r.PathValue("key")); err != nil {
		writeSettingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeSettingError writes a settings resolver error as JSON: an unknown key
// or a value that fails the registry's validation is a 400 carrying the
// resolver's message (an unknown key names the valid keys; a rejected value
// names the type or bounds — never a value), anything else is a 500 like
// every other handler.
func writeSettingError(w http.ResponseWriter, err error) {
	var ue settings.UnknownKeyError
	var ve settings.ValidationError
	switch {
	case errors.As(err, &ue):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.Error()})
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error()})
	default:
		writeInternalError(w, err)
	}
}

// writeGuards runs the two guards every write endpoint carries, in order, and
// reports whether the request may proceed: a JSON content type (415) and a
// same-origin check (403) (docs/DATA-API.md "The guards every write
// carries"). Settings, session, and — from phase 3 — work-request and lease
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

// maskSecret masks value so at most its last 4 characters are visible, the
// exact mask the CLI's harness config list applies to secrets
// (cmd/harness/config.go). A value of 4 characters or fewer reveals none of
// itself — the guarantee is that no stored secret ever appears in full over
// HTTP.
func maskSecret(value string) string {
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return strings.Repeat("*", len(value)-4) + value[len(value)-4:]
}

type eventsPage struct {
	Events []store.Event `json:"events"`
	From   int64         `json:"from"`
	Limit  int           `json:"limit"`
	// HasMore is the answer a pager actually needs: whether more events —
	// more *matching* events when ?kind= is set — follow this page. It is
	// decided by fetching one row past the page, so it is exact even when
	// the log ends exactly on a page boundary. Next, when HasMore is true,
	// is the seq to ask for the next page with: pass it back as ?from=
	// (docs/DATA-API.md "events").
	HasMore bool   `json:"has_more"`
	Next    *int64 `json:"next,omitempty"`
}

func (s *Server) eventsLimits() (def, max int) {
	def, max = defaultEventsLimit, maxEventsLimit
	if s.DefaultEventsLimit > 0 {
		def = s.DefaultEventsLimit
	}
	if s.MaxEventsLimit > 0 {
		max = s.MaxEventsLimit
	}
	return def, max
}

func (s *Server) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSession(r.Context(), id); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	kinds, err := parseEventKinds(r.URL.Query().Get("kind"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	def, max := s.eventsLimits()
	from := parseInt64(r.URL.Query().Get("from"), 0)
	if from < 0 {
		from = 0
	}
	limit := parseInt(r.URL.Query().Get("limit"), def)
	if limit <= 0 || limit > max {
		limit = def
	}

	// Fetch one row past the page so "is there more" is an exact answer
	// rather than a guess from a page that happens to be full — a full page
	// cannot tell "there is a next page" from "the log ends exactly here".
	// The extra row is dropped before the response. When a kind filter is
	// set, the probe is filtered the same way, so HasMore means more
	// *matching* events, never more events the filter would discard.
	events, err := s.Store.GetEventsAfterKinds(r.Context(), id, from-1, limit+1, kinds)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	if events == nil {
		events = []store.Event{}
	}

	page := eventsPage{Events: events, From: from, Limit: limit, HasMore: hasMore}
	if hasMore {
		next := events[len(events)-1].Seq + 1
		page.Next = &next
	}
	writeJSON(w, http.StatusOK, page)
}

// eventKindNames is every kind the event log can hold, as strings, in store
// declaration order — the ?kind= filter's valid-value list. It derives from
// store.EventKinds, the one list of kinds internal/store defines, so the
// filter accepts exactly what the log can hold and its 400 names exactly
// that; a kind added to the store extends the API here without a second
// literal to forget (docs/DATA-API.md "events").
var eventKindNames = func() []string {
	names := make([]string, len(store.EventKinds))
	for i, k := range store.EventKinds {
		names[i] = string(k)
	}
	return names
}()

// parseEventKinds turns a ?kind= value — a comma-separated list of event
// kind names — into the store kinds a page is filtered by. An empty value is
// no filter (every kind). An unknown name is an error that names the valid
// kinds, surfaced as the 400 docs/DATA-API.md's error shape prescribes.
func parseEventKinds(v string) ([]store.EventKind, error) {
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	kinds := make([]store.EventKind, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if !store.ValidEventKind(name) {
			return nil, fmt.Errorf("unknown event kind %q; valid kinds: %s", name, strings.Join(eventKindNames, ", "))
		}
		kinds = append(kinds, store.EventKind(name))
	}
	return kinds, nil
}

// handleSessionStream serves one session's transcript: the full history
// after Last-Event-ID (0 replays from the start), then live events as the
// hub publishes them, with no gap and no duplicate at the seam between the
// two (docs/DESIGN.md §4.2's "Last-Event-ID replay").
func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSession(r.Context(), id); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Subscribe before reading history: any event committed after this
	// point is guaranteed to arrive on the channel, so seq-based dedup
	// below is enough to cover the overlap window rather than needing a
	// lock across both operations.
	after := lastEventID(r)
	live, cancel := s.Hub.Subscribe(id)
	defer cancel()

	setSSEHeaders(w)
	flusher.Flush()

	events, err := s.Store.GetEventsAfter(r.Context(), id, after, -1)
	if err != nil {
		return
	}
	sent := after
	for _, ev := range events {
		writeSSEEvent(w, ev)
		sent = ev.Seq
	}
	flusher.Flush()

	// A session already at a terminal status will never append again.
	// Compaction retires the old session id without a closing event of its
	// own, so checking status here — not just watching for run_finished or
	// error — is what lets a reload of an already-compacted session's
	// stream close instead of idling forever.
	if sess, err := s.Store.GetSession(r.Context(), id); err == nil && sess.Status != store.StatusRunning {
		return
	}

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-live:
			if !ok {
				// The hub dropped this subscriber for lagging. Ending the
				// response here is what makes that safe: the browser's
				// EventSource reconnects with Last-Event-ID set to the last
				// id it saw, and the replay above fills the gap exactly.
				return
			}
			if ev.Seq <= sent {
				continue // already sent from history; the subscribe/read overlap window
			}
			writeSSEEvent(w, ev)
			sent = ev.Seq
			flusher.Flush()
			if ev.Kind == store.KindRunFinished || ev.Kind == store.KindError {
				return
			}
		case <-keepalive.C:
			if cur, err := s.Store.GetSession(r.Context(), id); err == nil && cur.Status != store.StatusRunning {
				return
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleListStream serves the session-list state feed: a full snapshot of
// every session's current row, then live updates as sessions are created,
// progress, or finish (docs/DESIGN.md §5.8). Unlike the transcript stream
// it carries no Last-Event-ID — a reconnect just gets a fresh snapshot,
// which is cheap because this stream is deliberately quiet.
func (s *Server) handleListStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	live, cancel := s.Hub.SubscribeList()
	defer cancel()

	sessions, err := s.Store.ListSessions(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	states, err := s.buildStates(r.Context(), sessions)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	setSSEHeaders(w)
	flusher.Flush()
	for _, st := range states {
		writeSSEData(w, st)
	}
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case st, ok := <-live:
			if !ok {
				return
			}
			writeSSEData(w, st)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func lastEventID(r *http.Request) int64 {
	return parseInt64(r.Header.Get("Last-Event-ID"), 0)
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
func writeSSEEvent(w io.Writer, ev store.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
}

// writeSSEData writes v as a plain SSE frame with no id — the shape the
// list stream uses, since a session-list row is a full replacement rather
// than a resumable log position.
func writeSSEData(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
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
