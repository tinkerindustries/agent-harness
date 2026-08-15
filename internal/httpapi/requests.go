package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Work requests (docs/DATA-API.md): the idempotency row every queued run
// carries, its poll-friendly status snapshot (GET
// /api/requests/{request_id}/status), and queue health (GET /api/queue).
// Workspace leases are the closely related resource one file over
// (leases.go).

// --- work-request and workspace-lease writes ---

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
// and setting finished_at (docs/DATA-API.md). It is the work-request
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
// the work_requests row (docs/DATA-API.md). It carries the same
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

// queueHealth is /api/queue's response shape: queue depth (claimable now),
// the Nak-scheduled backlog, in-flight count, and redelivery count, which
// the session list shows as queue health, plus whether the pool has halted
// itself and why — the visible form of docs/DESIGN.md §4.5's "A 402 stops
// the pool". Available is false whenever there is nothing to report from,
// which happens for any harness that has no queue wired up (Queue nil) or
// when the stats read itself fails; Error then carries why.
type queueHealth struct {
	Available   bool   `json:"available"`
	QueueDepth  int    `json:"queue_depth,omitempty"`
	Scheduled   int    `json:"scheduled,omitempty"`
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
	if s.Queue == nil {
		writeJSON(w, http.StatusOK, health)
		return
	}
	stats, err := s.Queue.Stats(r.Context())
	if err != nil {
		health.Error = err.Error()
		writeJSON(w, http.StatusOK, health)
		return
	}
	health.Available = true
	health.QueueDepth = stats.Depth
	health.Scheduled = stats.Scheduled
	health.InFlight = stats.InFlight
	health.Redelivered = stats.Redelivered
	writeJSON(w, http.StatusOK, health)
}
