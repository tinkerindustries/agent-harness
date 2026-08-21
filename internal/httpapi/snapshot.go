package httpapi

import (
	"net/http"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// GET /api/sessions/{id}/snapshot: everything a transcript screen needs to
// paint what already happened, in one response — the session's metadata row,
// its whole event log, and the cursor the caller should stream on from.
//
// It exists because the transcript stream used to be the only source, and a
// page load therefore meant replaying the entire history down an SSE
// connection frame by frame. That is correct and it is gapless, but it
// arrives at the speed of a stream rather than a download: a browser paints
// each chunk as it lands, so a long session visibly fills in over seconds
// instead of appearing. This endpoint is the bulk half — one request, one
// decompression, one fold — and the stream keeps the live half it is
// actually good at.
//
// # Why the seam cannot drop an event
//
// The pair is used in one order and only one: fetch the snapshot, read
// Cursor off it, then open the stream at ?from=Cursor.
//
//   - Every event with seq <= Cursor is in this response, by construction:
//     Cursor is the seq of the last row the read returned.
//   - Every event with seq > Cursor is replayed by the stream, because
//     handleSessionStream replays everything after its cursor before it
//     starts forwarding live frames, and the log is append-only with
//     monotonic seq — an event committed in the gap between these two
//     requests has a seq above Cursor and is therefore in that replay.
//   - Nothing arrives twice, because the two sets are split on the same
//     number.
//
// The window between the requests is the reason Cursor is returned at all
// rather than the client counting the events it got: a client-side maximum
// would be the same number today, but it would be the client's arithmetic
// deciding whether the log has a hole in it, and this way it is the server's
// own read.
//
// Live deltas (hub.LiveDelta) are the one thing that can be missed here, and
// deliberately so: they carry no seq because they are uncommitted model
// output, so deltas emitted before the stream opens are simply not seen. The
// text they were previewing arrives as a committed event moments later, so
// the transcript self-heals within one sub-turn. A snapshot cannot carry
// them and there is nothing to resume them from.
type sessionSnapshot struct {
	// Session is the same row the stream's `state` frames carry
	// (hub.SessionState), so the screen has its cost, cache and sub-turn
	// figures before the first frame rather than only once the session next
	// changes — which for a finished session is never.
	Session hub.SessionState `json:"session"`
	Events  []store.Event    `json:"events"`
	// Cursor is the seq of the last event in Events, and 0 for a session
	// with no events yet — which is also the right ?from= for one, since
	// seq numbering starts at 1.
	Cursor int64 `json:"cursor"`
}

func (s *Server) handleGetSessionSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}

	// -1 is "no limit": the whole log, the same read handleSessionStream
	// does for a replay from the start. A transcript is one screen's worth
	// of data and is not paged anywhere else either; what made it expensive
	// was the images, and eventForWire has taken those out by the time this
	// is serialised.
	events, err := s.Store.GetEventsAfter(r.Context(), id, 0, -1)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	states, err := s.buildStates(r.Context(), []store.Session{sess})
	if err != nil {
		writeInternalError(w, err)
		return
	}

	snap := sessionSnapshot{Session: states[0], Events: redactEvents(events)}
	if len(events) > 0 {
		snap.Cursor = events[len(events)-1].Seq
	}
	writeCompressedJSON(w, r, http.StatusOK, snap)
}
