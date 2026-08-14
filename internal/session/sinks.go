package session

import (
	"context"
	"log"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// sinks.go: where a sub-turn's output goes once it happens — the disk
// mirror (internal/store/mirror.go) and the hub (internal/hub), the two
// consumers every event and state change is published to.

func (r *Runner) mirrorAppend(sess store.Session, events []store.Event) {
	if r.Mirror == nil || len(events) == 0 {
		return
	}
	if err := r.Mirror.AppendEvents(sess, events); err != nil {
		log.Printf("session: mirror append failed for %s: %v", sess.ID, err)
	}
}

// openLog opens sess's http log writer, creating its file under the
// recorder's root. A nil Recorder (capture off) is a no-op; a failed open
// is logged, never a reason to fail the run. Call it once the session row
// exists, so every session that exists gets a file.
func (r *Runner) openLog(sess store.Session) {
	if r.Recorder == nil {
		return
	}
	if err := r.Recorder.Open(sess.ID, time.Now()); err != nil {
		log.Printf("session: open http log for %s: %v", sess.ID, err)
	}
}

// closeLog closes sessionID's http log writer, finalising its gzip member.
// A nil Recorder or a session that was never opened is a no-op.
func (r *Runner) closeLog(sessionID string) {
	if r.Recorder == nil {
		return
	}
	if err := r.Recorder.Close(sessionID); err != nil {
		log.Printf("session: close http log for %s: %v", sessionID, err)
	}
}

func (r *Runner) mirrorUpdateSession(sess store.Session) {
	if r.Mirror == nil {
		return
	}
	if err := r.Mirror.UpdateSession(sess); err != nil {
		log.Printf("session: mirror session update failed for %s: %v", sess.ID, err)
	}
}

// publishEvents fans newly committed events out to live SSE subscribers, on
// top of the disk mirror. Call it with the same events a successful
// AppendEvents just returned, alongside the mirrorAppend call for the same
// batch.
func (r *Runner) publishEvents(sess store.Session, events []store.Event) {
	if r.Hub == nil || len(events) == 0 {
		return
	}
	r.Hub.PublishEvents(sess.ID, events)
}

// publishState recomputes sess's session-list row and fans it out to the
// list stream (docs/DESIGN.md §5.8). It reads the usage summary and request
// id back from the store rather than threading them through the run loop,
// so the row a live subscriber sees is always exactly what a fresh GET
// /api/sessions would return for the same session.
func (r *Runner) publishState(ctx context.Context, sess store.Session) {
	if r.Hub == nil {
		return
	}
	summaries, err := r.Store.SessionUsageSummaries(ctx, []string{sess.ID})
	if err != nil {
		log.Printf("session: usage summary for %s: %v", sess.ID, err)
		return
	}
	requestIDs, err := r.Store.RequestIDsForSessions(ctx, []string{sess.ID})
	if err != nil {
		log.Printf("session: request id lookup for %s: %v", sess.ID, err)
		return
	}
	r.Hub.PublishSessionState(hub.BuildSessionState(sess, summaries[sess.ID], requestIDs[sess.ID], r.priceTableDate()))
}

// priceTableDate is r.Prices's capture date, or "" when this Runner has no
// price table (a test double, most often) — BuildSessionState treats an
// empty date as "not shown" rather than a zero value worth displaying.
func (r *Runner) priceTableDate() string {
	if r.Prices == nil {
		return ""
	}
	return r.Prices.CapturedAt
}
