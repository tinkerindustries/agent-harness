// Package hub is the harness's in-process SSE fan-out (docs/DESIGN.md
// §4.2): per-session transcript subscribers, fed the same
// store.Event values a session appends, and a session-list subscriber set,
// fed the low-rate state changes docs/DESIGN.md §5.8 wants quiet.
//
// The browser reads the store and this hub, never NATS — RESULTS progress
// messages are turn-level and rate-limited for a different consumer
// (docs/DESIGN.md §4.10). Nothing here touches JetStream.
package hub

import (
	"sync"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// sessionBufferSize and listBufferSize bound how far a subscriber may lag
// before the hub drops it rather than block the goroutine publishing. A
// dropped subscriber's SSE connection ends; the browser's EventSource
// reconnects, and Last-Event-ID replay (session streams) or a fresh
// snapshot (the list stream) fills in exactly what was missed — dropping
// costs one extra store read and nothing else.
const (
	sessionBufferSize = 256
	listBufferSize    = 64
)

// LiveDelta is model output as it arrives, before the sub-turn it belongs to
// has committed anything. It is NOT an event: it has no seq, it is never
// stored, and the authoritative reasoning_delta/content_delta events for the
// same text land later in the sub-turn's commit batch.
//
// It exists because a sub-turn's events are committed in one batch when the
// response is complete (internal/session, docs/DESIGN.md §4.5), so a browser
// receives turn_started and turn_finished in the same instant and the live
// transcript states were unreachable on a real run — a finding from the phase
// 5 review (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). Streaming
// these separately keeps the log's one-batch-per-sub-turn shape intact and
// still gives a watching operator the text as it is written.
type LiveDelta struct {
	SubTurn int `json:"sub_turn"`
	// Channel is "reasoning" or "content" — which of the model's two text
	// streams this text belongs to.
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// Channel values for LiveDelta.
const (
	ChannelReasoning = "reasoning"
	ChannelContent   = "content"
)

// Frame is one thing to send a transcript subscriber: either a committed
// event or an ephemeral live delta, never both. The two travel on one
// channel so a subscriber cannot receive them out of order relative to each
// other — a delta published after a commit must arrive after it, and two
// channels would race.
type Frame struct {
	Event store.Event
	Live  *LiveDelta
}

// Hub fans events out to SSE subscribers. All methods are safe for
// concurrent use; a single Hub is shared by every session goroutine and
// every HTTP handler in the process, the same shared-read-mostly-resource
// shape session.Runner's own fields already have (docs/DESIGN.md §4.5).
type Hub struct {
	mu       sync.Mutex
	sessions map[string]map[chan Frame]struct{}
	list     map[chan SessionState]struct{}
}

// New returns a Hub ready to use.
func New() *Hub {
	return &Hub{
		sessions: make(map[string]map[chan Frame]struct{}),
		list:     make(map[chan SessionState]struct{}),
	}
}

// Subscribe registers for sessionID's events published from this point
// forward. cancel unregisters and closes the channel; call it exactly once
// (a deferred call in the HTTP handler that owns the subscription is the
// expected shape).
func (h *Hub) Subscribe(sessionID string) (frames <-chan Frame, cancel func()) {
	ch := make(chan Frame, sessionBufferSize)
	h.mu.Lock()
	set, ok := h.sessions[sessionID]
	if !ok {
		set = make(map[chan Frame]struct{})
		h.sessions[sessionID] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()

	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set, ok := h.sessions[sessionID]; ok {
			if _, present := set[ch]; present {
				delete(set, ch)
				close(ch)
				if len(set) == 0 {
					delete(h.sessions, sessionID)
				}
			}
		}
	}
	return ch, cancel
}

// PublishEvents fans sessionID's newly committed events out to its
// subscribers, as Frames carrying no live delta. See publish for the
// dropping rule.
func (h *Hub) PublishEvents(sessionID string, events []store.Event) {
	if len(events) == 0 {
		return
	}
	frames := make([]Frame, len(events))
	for i, ev := range events {
		frames[i] = Frame{Event: ev}
	}
	h.publish(sessionID, frames)
}

// PublishLive fans one ephemeral delta out to sessionID's subscribers. It
// drops a lagging subscriber on exactly the same terms PublishEvents does,
// which matters more here: deltas are the highest-rate thing on this
// channel, so the publisher coalesces them (internal/session) rather than
// sending one per token, and a subscriber that still cannot keep up loses
// nothing durable — its reconnect replays the committed log, where this
// text lands anyway.
func (h *Hub) PublishLive(sessionID string, d LiveDelta) {
	if d.Text == "" {
		return
	}
	h.publish(sessionID, []Frame{{Live: &d}})
}

// publish is the shared fan-out. A subscriber whose buffer is already full
// is dropped — removed and closed — rather than allowed to block this call,
// so a slow browser tab can never stall the session goroutine that produced
// these frames. Every send and every close happens under h.mu, so a
// subscriber dropped here and a subscriber cancelled by its own HTTP handler
// can never race on the same channel.
func (h *Hub) publish(sessionID string, frames []Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.sessions[sessionID]
subs:
	for ch := range set {
		for _, f := range frames {
			select {
			case ch <- f:
			default:
				delete(set, ch)
				close(ch)
				continue subs
			}
		}
	}
	if len(set) == 0 {
		delete(h.sessions, sessionID)
	}
}

// SubscribeList registers for session-list state changes. Unlike Subscribe,
// there is no history to replay: the list stream carries a fresh full
// snapshot on every connection instead of a resumable log, so a caller
// reconnecting after a drop just gets the current state again rather than
// needing a sequence number (docs/DESIGN.md §5.8).
func (h *Hub) SubscribeList() (states <-chan SessionState, cancel func()) {
	ch := make(chan SessionState, listBufferSize)
	h.mu.Lock()
	h.list[ch] = struct{}{}
	h.mu.Unlock()

	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, present := h.list[ch]; present {
			delete(h.list, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// PublishSessionState fans one session's current row out to every list
// subscriber, dropping (not blocking on) any that has fallen behind.
func (h *Hub) PublishSessionState(s SessionState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.list {
		select {
		case ch <- s:
		default:
			delete(h.list, ch)
			close(ch)
		}
	}
}
