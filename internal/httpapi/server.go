// Package httpapi is the harness's read-only HTTP surface (docs/DESIGN.md
// §4.2): GET and HEAD only, on every path, including ones
// that do not exist. It serves the session list and metadata from the
// store, a paged read of one session's event log, and two SSE streams — a
// per-session transcript and a quiet session-level list feed — fed by the
// in-process hub package rather than NATS. Nothing here starts, steers, or
// stops a run; that is the whole point of the browser being read-only.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// sseKeepaliveInterval is how often an idle stream sends a ": keep-alive"
// comment, which also doubles as the poll interval for noticing a session
// that reached a terminal status without an event of its own to announce it
// (compaction retires the old session id silently; see handleSessionStream).
const sseKeepaliveInterval = 15 * time.Second

const (
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

// Server holds the things every handler reads: the store, for everything
// historical; the hub, for everything live; and, optionally, the queue's
// consumer and pool, for /api/queue's consumer lag, in-flight count, and
// redelivery count (docs/DESIGN.md §5.8). None of these is
// mutated by a request — there is no write path (docs/DESIGN.md §4.2).
// Consumer and Pool are nil in any caller that has no queue at all (a
// CLI-only harness never wires one up); the handler degrades to reporting
// the queue as unavailable rather than panicking.
type Server struct {
	Store          *store.Store
	Hub            *hub.Hub
	Static         http.Handler
	Consumer       QueueConsumer
	Pool           QueuePool
	PriceTableDate string
}

// Handler returns the harness's whole HTTP surface. methodGate runs before
// routing, so a non-GET/HEAD request is rejected on every path, including
// ones nothing here recognises — the read-only constraint has to hold for
// paths that do not exist too (docs/DESIGN.md §4.2).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleGetEvents)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleSessionStream)
	mux.HandleFunc("GET /api/stream", s.handleListStream)
	mux.HandleFunc("GET /api/queue", s.handleQueueHealth)
	mux.Handle("/", s.Static)
	return methodGate(mux)
}

// methodGate enforces GET and HEAD only ahead of any routing decision. A
// pattern registered with a method already 405s a wrong-method request that
// matches its path (net/http's ServeMux does this since Go 1.22), but that
// only covers paths this package recognises; the static handler's "/"
// pattern matches everything, method or not, and POST to a path nobody
// registered would otherwise fall through to a 404 rather than the 405
// docs/DESIGN.md §4.2 requires everywhere.
func methodGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only: GET and HEAD only", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
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

type eventsPage struct {
	Events []store.Event `json:"events"`
	From   int64         `json:"from"`
	Limit  int           `json:"limit"`
	Next   *int64        `json:"next,omitempty"`
}

func (s *Server) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSession(r.Context(), id); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	from := parseInt64(r.URL.Query().Get("from"), 0)
	if from < 0 {
		from = 0
	}
	limit := parseInt(r.URL.Query().Get("limit"), defaultEventsLimit)
	if limit <= 0 || limit > maxEventsLimit {
		limit = defaultEventsLimit
	}

	events, err := s.Store.GetEventsAfter(r.Context(), id, from-1, limit)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if events == nil {
		events = []store.Event{}
	}

	page := eventsPage{Events: events, From: from, Limit: limit}
	if len(events) == limit {
		next := events[len(events)-1].Seq + 1
		page.Next = &next
	}
	writeJSON(w, http.StatusOK, page)
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
