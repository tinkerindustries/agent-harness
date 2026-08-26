package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The two SSE streams (docs/DESIGN.md §4.2): a per-session transcript and a
// quieter session-list feed, both fed by internal/hub's in-process fan-out —
// the browser reads the store and the hub, never the queue. The
// frame-writing helpers themselves (setSSEHeaders, writeSSEEvent and
// friends) are shared with evals.go's own stream and live in respond.go.

// handleSessionStream serves one session's transcript: the full history
// after the stream cursor (streamCursor — Last-Event-ID, else ?from=, else
// 0, which replays from the start), then live events as the hub publishes
// them, with no gap and no duplicate at the seam between the two
// (docs/DESIGN.md §4.2's "Last-Event-ID replay").
//
// ?from= is what lets a page load skip the replay entirely: the browser
// fetches GET /api/sessions/{id}/snapshot, folds it, and opens this stream
// at the cursor that came back, so the replay below is normally empty and
// this connection carries only what happens next. handleGetSessionSnapshot
// documents why that seam cannot drop an event.
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
	after := streamCursor(r)
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
	// The seam between the replay and the live tail, named so the browser can
	// find it. Everything before this frame is history the page loaded with;
	// everything after it happened while somebody was watching, and the
	// frontend animates only the second kind (web/src/hooks.ts useArrivals).
	//
	// It has to be a frame rather than something the client infers, because
	// the replay does not arrive as one batch: a long history is delivered
	// across several reads, so "the first events I saw" is a fraction of the
	// backlog and everything after it would read as newly arrived.
	//
	// A *named* event with no id, exactly like a live delta: onmessage never
	// sees it, so it cannot be mistaken for a committed event, and it cannot
	// move the EventSource's Last-Event-ID cursor. A reconnect replays from
	// the last real event and gets a fresh marker at the new seam.
	writeSSEReplayed(w)
	flusher.Flush()

	// A session already at a terminal status will never append again.
	// Compaction retires the old session id without a closing event of its
	// own, so checking status here — not just watching for run_finished or
	// error — is what lets a reload of an already-compacted session's
	// stream close instead of idling forever. IsLive keeps the stream open
	// for a session still cloning too, so a browser that opened it before
	// the first event stays connected through the preparation window.
	if sess, err := s.Store.GetSession(r.Context(), id); err == nil && !store.IsLive(sess.Status) {
		writeSSEClosed(w)
		flusher.Flush()
		return
	}

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case frame, ok := <-live:
			if !ok {
				// The hub dropped this subscriber for lagging. Ending the
				// response here is what makes that safe: the browser's
				// EventSource reconnects with Last-Event-ID set to the last
				// id it saw, and the replay above fills the gap exactly.
				return
			}
			// A live delta is model output that has not been committed yet
			// (hub.LiveDelta). It carries no seq, so it is neither deduped
			// against the history replay nor allowed to move `sent`: the
			// resume cursor must only ever name a real event, or a
			// reconnect would skip whatever was committed in between.
			if frame.Live != nil {
				writeSSELive(w, *frame.Live)
				flusher.Flush()
				continue
			}
			// A state frame is this session's metadata row, republished
			// whenever it changes (hub.PublishSessionState). Like a live
			// delta it is named and carries no seq, so it neither reaches
			// the client's onmessage fold nor moves the resume cursor: it
			// is not a log position, it is the current value of a row the
			// log does not hold.
			if frame.State != nil {
				writeSSEState(w, *frame.State)
				flusher.Flush()
				continue
			}
			ev := frame.Event
			if ev.Seq <= sent {
				continue // already sent from history; the subscribe/read overlap window
			}
			writeSSEEvent(w, ev)
			sent = ev.Seq
			flusher.Flush()
			if ev.Kind == store.KindRunFinished || ev.Kind == store.KindError {
				writeSSEClosed(w)
				flusher.Flush()
				return
			}
		case <-keepalive.C:
			if cur, err := s.Store.GetSession(r.Context(), id); err == nil && !store.IsLive(cur.Status) {
				writeSSEClosed(w)
				flusher.Flush()
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
//
// Every row here is a hub.ListRow, the projection down to what the session
// list renders. This feed is quiet in frequency, not in volume: it re-sends
// a whole row on every sub-turn of every running session to every browser
// with the list open, so a field it carries that no pixel reads is paid for
// once per sub-turn per session per tab. A caller that wants the whole row
// asks for one — GET /api/sessions, or the `state` frames on the session's
// own stream, which is the feed for somebody watching one session.
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
	// The snapshot is projected through the same ListRowOf the live tail
	// below already comes through (hub.PublishSessionState), so the row a
	// browser starts from and the rows it is updated with carry the identical
	// field set — a snapshot with more fields than the updates would leave
	// whatever it seeded reverting the first time a session moved.
	for _, st := range states {
		writeSSEData(w, hub.ListRowOf(st))
	}
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case row, ok := <-live:
			if !ok {
				return
			}
			writeSSEData(w, row)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// streamCursor is the seq a transcript stream replays from, exclusive: the
// last event the caller already holds.
func streamCursor(r *http.Request) int64 {
	// Last-Event-ID wins whenever the browser sets it, which it does on
	// every automatic reconnect and only then. It is strictly fresher than
	// ?from=: the query string was fixed when the EventSource was
	// constructed and cannot be updated, so honouring it over a live cursor
	// would replay everything the connection had already delivered before it
	// dropped.
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return parseInt64(v, 0)
	}
	// ?from= is the first connection's cursor: the seq the caller already
	// holds, from a snapshot it fetched (handleGetSessionSnapshot) or from a
	// previous stream it is picking back up. Absent — or unparseable, or
	// negative — it is 0, which replays the whole log and is what a client
	// that knows nothing about this parameter gets.
	if from := parseInt64(r.URL.Query().Get("from"), 0); from > 0 {
		return from
	}
	return 0
}
