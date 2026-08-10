package hub

import (
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

func TestSubscribePublishReceivesInOrder(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("sess-1")
	defer cancel()

	events := []store.Event{{SessionID: "sess-1", Seq: 1}, {SessionID: "sess-1", Seq: 2}}
	h.PublishEvents("sess-1", events)

	for _, want := range events {
		select {
		case got := <-ch:
			if got.Seq != want.Seq {
				t.Fatalf("got seq %d, want %d", got.Seq, want.Seq)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}

// TestPublishIsIsolatedPerSession asserts a publish for one session never
// reaches a subscriber of another — the per-session firehose must not leak
// across sessions the way the quiet list stream is allowed to fan out to
// everyone.
func TestPublishIsIsolatedPerSession(t *testing.T) {
	h := New()
	chA, cancelA := h.Subscribe("sess-a")
	defer cancelA()
	chB, cancelB := h.Subscribe("sess-b")
	defer cancelB()

	h.PublishEvents("sess-a", []store.Event{{SessionID: "sess-a", Seq: 1}})

	select {
	case ev := <-chA:
		if ev.SessionID != "sess-a" {
			t.Fatalf("unexpected event on sess-a channel: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sess-a event")
	}

	select {
	case ev, ok := <-chB:
		t.Fatalf("sess-b channel should have received nothing, got %+v (ok=%v)", ev, ok)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing arrived
	}
}

// TestSlowSubscriberDoesNotBlockPublish is the backpressure property
// docs/DESIGN.md §5 requires: a subscriber that never drains must not stall
// the goroutine calling PublishEvents. It proves this by publishing well
// past the channel's buffer without ever reading from it and checking the
// call still returns promptly.
func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	h := New()
	_, cancel := h.Subscribe("sess-1")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < sessionBufferSize*4; i++ {
			h.PublishEvents("sess-1", []store.Event{{SessionID: "sess-1", Seq: int64(i)}})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishEvents blocked on a subscriber that never drained")
	}
}

// TestDroppedSubscriberChannelCloses asserts that once a slow subscriber's
// buffer overflows, its channel is closed rather than left silently full —
// that closed signal is what tells an SSE handler to end the response so
// the browser reconnects and replays via Last-Event-ID (docs/DESIGN.md §4.2).
func TestDroppedSubscriberChannelCloses(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("sess-1")
	defer cancel()

	for i := 0; i < sessionBufferSize+8; i++ {
		h.PublishEvents("sess-1", []store.Event{{SessionID: "sess-1", Seq: int64(i)}})
	}

	// Drain whatever is buffered; the channel must close once drained,
	// rather than the reader ever seeing it hang open with no more values.
	closed := false
	for i := 0; i < sessionBufferSize+16; i++ {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting to observe channel closure")
		}
		if closed {
			break
		}
	}
	if !closed {
		t.Fatal("expected the dropped subscriber's channel to close")
	}
}

func TestSubscribeListReceivesSnapshot(t *testing.T) {
	h := New()
	ch, cancel := h.SubscribeList()
	defer cancel()

	h.PublishSessionState(SessionState{ID: "sess-1", Status: "running"})

	select {
	case got := <-ch:
		if got.ID != "sess-1" || got.Status != "running" {
			t.Fatalf("unexpected state: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for list event")
	}
}

// TestBuildSessionStateCarriesProvenance asserts the job type and parent
// agent fields on a stored session reach the wire row unchanged, so both the
// REST list and the session_state stream surface them.
func TestBuildSessionStateCarriesProvenance(t *testing.T) {
	sess := store.Session{
		ID:              "sess-1",
		JobType:         agentmeta.JobTypeOrchestration,
		ParentAgentType: "claude-code",
		ParentAgentID:   "sess-parent-1",
		CompleteStatus:  "gave_up",
	}
	st := BuildSessionState(sess, store.SessionUsageSummary{}, "req-1", "")
	if st.JobType != agentmeta.JobTypeOrchestration {
		t.Fatalf("expected job type %q on the wire row, got %q", agentmeta.JobTypeOrchestration, st.JobType)
	}
	if st.ParentAgentType != "claude-code" || st.ParentAgentID != "sess-parent-1" {
		t.Fatalf("expected parent agent claude-code/sess-parent-1 on the wire row, got %q/%q", st.ParentAgentType, st.ParentAgentID)
	}
	if st.CompleteStatus != "gave_up" {
		t.Fatalf("expected complete_status %q on the wire row, got %q", "gave_up", st.CompleteStatus)
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("sess-1")
	cancel()

	// A cancelled subscriber's channel must be closed, and a publish after
	// cancellation must not panic (no send on a channel nobody owns
	// anymore) or resurrect it.
	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after cancel")
	}
	h.PublishEvents("sess-1", []store.Event{{SessionID: "sess-1", Seq: 1}})
}
