package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// Session reads (GET /api/sessions, GET /api/sessions/{id}) and the two
// row-edit writes (PATCH and DELETE /api/sessions/{id}) — closing an
// abandoned session and deleting a finished one. Run control on a session
// (stop, steer, start) is a different kind of write, an action rather than a
// row edit, and lives in runcontrol.go.

// sessionListStatuses are the valid ?status= values on GET /api/sessions:
// "running" (the live set — running plus creating, the rows the in-flight
// list shows), and "finished" — the display name for everything not live.
// The 400 for an unknown status names exactly these, the same refusal shape
// the events endpoint's ?kind= filter uses. The store widening does the rest:
// ListSessionsPage builds its SQL off store.IsLive, so these two public
// values keep their meaning as the live set grows.
var sessionListStatuses = []string{store.StatusRunning, "finished"}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && !slices.Contains(sessionListStatuses, status) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("status must be one of %s", strings.Join(sessionListStatuses, ", ")),
		})
		return
	}
	limit, offset := parsePaging(r, defaultPageLimit, maxPageLimit)
	sessions, total, err := s.Store.ListSessionsPage(r.Context(), store.SessionPageOptions{
		Status: status,
		Query:  r.URL.Query().Get("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	states, err := s.buildStates(r.Context(), sessions)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// The envelope, always — a paginated list endpoint returns Page even
	// when the caller asked for no page, so the response has one shape for
	// every consumer (docs/DATA-API.md "Pagination").
	writeJSON(w, http.StatusOK, newPage(states, total, limit, offset))
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
// worker left live — running, or creating mid-clone — by transitioning it to
// a terminal status and setting finished_at (docs/DATA-API.md). It is the
// write the read-only rule was retired for — an abandoned row still says
// "running" (or "creating") and nothing else ever closes it.
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
