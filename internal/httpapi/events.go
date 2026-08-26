package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/redact"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// GET /api/sessions/{id}/events: a paged read of one session's event log,
// redacted (internal/redact) before it leaves the process — the event log
// is a credential store, since a session's tool output is whatever the
// commands it ran printed. Never writable (docs/DATA-API.md "Events are not
// writable"), which boundary_test.go pins on the bare router.

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

	page := eventsPage{Events: redactEvents(events), From: from, Limit: limit, HasMore: hasMore}
	if hasMore {
		next := events[len(events)-1].Seq + 1
		page.Next = &next
	}
	writeJSON(w, http.StatusOK, page)
}

// redactEvent masks credential-shaped strings in an event's payload before
// it is served (internal/redact). The port serves whatever a session's
// commands printed, and a run that needed a token in its container put a
// full github_pat_ value in a tool result with one `head -2 .env`
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md).
//
// The boundary is this package, not the store: the mirror on disk keeps the
// literal bytes, because it sits beside the workspace that holds the .env
// the token came from and redacting one while the other is readable is
// false comfort. What is different about the HTTP surface is that it is
// reachable — loopback today, and the first thing anyone will want to
// expose (docs/RUN-CONTROL.md). It is also not applied on the write path:
// the fold rebuilds the model's own conversation from these same events and
// must see what the command actually printed.
func redactEvent(ev store.Event) store.Event {
	ev.Payload = redact.Bytes(ev.Payload)
	return ev
}

// eventForWire is the whole outbound projection of one event, applied
// identically by every surface that hands events out — the events page, the
// snapshot, and each frame of the transcript stream — so none of them can
// serve a shape the others do not.
//
// The order is load-bearing in both directions. Detaching first means the
// redaction regex never walks the megabytes of base64 an image payload
// carries, which on a render-heavy session is the great majority of the
// bytes in the log and cannot contain a credential in any case. Redacting
// second means it still sees the rewritten payload, so a token in a tool
// result beside an image is masked exactly as it would be without one.
func eventForWire(ev store.Event) store.Event {
	return redactEvent(detachEventImage(ev))
}

// redactEvents is eventForWire over a page, returning a new slice so the
// caller's events — the ones handleGetEvents still reads Seq off for the
// next-page cursor — are untouched.
func redactEvents(events []store.Event) []store.Event {
	out := make([]store.Event, len(events))
	for i, ev := range events {
		out[i] = eventForWire(ev)
	}
	return out
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
