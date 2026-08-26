package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/store"
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
			if got.Event.Seq != want.Seq {
				t.Fatalf("got seq %d, want %d", got.Event.Seq, want.Seq)
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
		if ev.Event.SessionID != "sess-a" {
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

// TestPublishSessionStateReachesBothAudiences asserts the one publish call a
// session makes lands in both shapes: the ListRow projection on the list
// feed, and the whole row as a Frame on that session's own stream. The two
// are what the session list and the session detail screen are each connected
// to, and neither should need the other's endpoint to stay current.
func TestPublishSessionStateReachesBothAudiences(t *testing.T) {
	h := New()
	rows, cancelList := h.SubscribeList()
	defer cancelList()
	frames, cancelSession := h.Subscribe("sess-1")
	defer cancelSession()

	state := SessionState{
		ID:              "sess-1",
		Status:          "running",
		Summary:         "wired it up",
		PermissionMode:  "full",
		Version:         7,
		RecentToolCalls: []store.RecentToolCall{{Name: "Write", Arguments: `{"content":"a whole file"}`}},
		Usage:           Usage{CostUSD: 1.25, CacheHitTokens: 90},
	}
	h.PublishSessionState(state)

	select {
	case got := <-rows:
		if got.ID != "sess-1" || got.Status != "running" {
			t.Fatalf("unexpected list row: %+v", got)
		}
		if got.Usage.CostUSD != 1.25 {
			t.Fatalf("expected the running cost on the list row, got %v", got.Usage.CostUSD)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the list row")
	}

	select {
	case f := <-frames:
		if f.State == nil {
			t.Fatalf("expected a state frame on the session stream, got %+v", f)
		}
		// The session's own stream is the one that carries everything: the
		// fields ListRow drops are exactly what the detail screen reads.
		if f.State.Summary != "wired it up" || f.State.PermissionMode != "full" || f.State.Version != 7 {
			t.Fatalf("expected the whole row on the session stream, got %+v", f.State)
		}
		if len(f.State.RecentToolCalls) != 1 {
			t.Fatalf("expected the tool-call roll on the session stream, got %+v", f.State.RecentToolCalls)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the state frame")
	}
}

// TestListRowOfDropsWhatTheListDoesNotRender pins the projection by its JSON,
// which is the contract the browser actually sees. The named fields are the
// ones the session list has no pixel for and that the feed re-sent on every
// sub-turn — recent_tool_calls, the largest of them by far, carries raw tool
// arguments and was never redacted on this feed.
func TestListRowOfDropsWhatTheListDoesNotRender(t *testing.T) {
	b, err := json.Marshal(ListRowOf(SessionState{
		ID:              "sess-1",
		Status:          "running",
		PermissionMode:  "full",
		ParentID:        "sess-parent",
		Summary:         "wired it up",
		Version:         7,
		PriceTableDate:  "2026-01-01",
		RecentToolCalls: []store.RecentToolCall{{Name: "Write", Arguments: `{"content":"a whole file"}`}},
		Usage:           Usage{CostUSD: 1.25, CacheHitTokens: 90, CacheMissTokens: 10},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{
		`"recent_tool_calls"`, `"summary"`, `"permission_mode"`, `"parent_id"`,
		`"version"`, `"price_table_date"`, `"cache_hit_tokens"`, `"cache_miss_tokens"`,
	} {
		if jsonContains(b, absent) {
			t.Fatalf("expected %s off the list feed, got %s", absent, b)
		}
	}
	// What the list does render has to survive the projection, cost among it:
	// the stat strip's "Spend today" is a sum over this field.
	for _, present := range []string{`"id"`, `"status"`, `"sub_turns"`, `"cost_usd"`} {
		if !jsonContains(b, present) {
			t.Fatalf("expected %s on the list feed, got %s", present, b)
		}
	}
}

// TestListRowOfCapsTheTask asserts the cap and, just as importantly, that a
// task under it is untouched — the list's fallback description line is the
// whole task for most runs, and a cap that marked every row would be a
// visible change to all of them.
func TestListRowOfCapsTheTask(t *testing.T) {
	short := "wire it up"
	if got := ListRowOf(SessionState{Task: short}).Task; got != short {
		t.Fatalf("expected a short task carried whole, got %q", got)
	}

	// Multi-byte on purpose: the cap counts runes, so a task cut here must
	// still be valid UTF-8 rather than half a character.
	long := strings.Repeat("é", MaxListTaskChars+50)
	got := ListRowOf(SessionState{Task: long}).Task
	if !utf8.ValidString(got) {
		t.Fatalf("capped task is not valid UTF-8: %q", got)
	}
	if runes := utf8.RuneCountInString(got); runes != MaxListTaskChars+1 {
		t.Fatalf("expected %d runes (the cap plus the ellipsis), got %d", MaxListTaskChars+1, runes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected a capped task to say so with an ellipsis, got %q", got)
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
		ParentIsUser:    true,
		CompleteStatus:  "gave_up",
	}
	st := BuildSessionState(sess, store.SessionUsageSummary{}, "req-1", "")
	if st.JobType != agentmeta.JobTypeOrchestration {
		t.Fatalf("expected job type %q on the wire row, got %q", agentmeta.JobTypeOrchestration, st.JobType)
	}
	if st.ParentAgentType != "claude-code" || st.ParentAgentID != "sess-parent-1" || !st.ParentIsUser {
		t.Fatalf("expected parent agent claude-code/sess-parent-1 (user) on the wire row, got %q/%q (user=%v)", st.ParentAgentType, st.ParentAgentID, st.ParentIsUser)
	}
	if st.CompleteStatus != "gave_up" {
		t.Fatalf("expected complete_status %q on the wire row, got %q", "gave_up", st.CompleteStatus)
	}
}

// TestBuildSessionStateCarriesLivePlan asserts the plan, recent-tool-call
// roll, task, summary, and the four session fields reach the wire row in the
// shape the browser consumes them: plan as the raw todos array, the roll as
// an array of calls, task, title, and description as strings, phase and
// total_phases as ints — all omitted when empty.
func TestBuildSessionStateCarriesLivePlan(t *testing.T) {
	sess := store.Session{
		ID:              "sess-1",
		Task:            "carry the job's description onto the session row",
		Title:           "Add session title fields",
		Description:     "Carry a title, description, and phase position from every producer onto the session row.",
		Phase:           2,
		TotalPhases:     5,
		Plan:            `[{"content":"a","status":"completed","activeForm":""}]`,
		RecentToolCalls: []store.RecentToolCall{{Name: "Bash", Arguments: `{"command":"go build ./..."}`}},
		Summary:         "wired it up",
	}
	st := BuildSessionState(sess, store.SessionUsageSummary{}, "req-1", "")
	if st.Task != sess.Task {
		t.Fatalf("expected task %q on the wire row, got %q", sess.Task, st.Task)
	}
	if st.Title != sess.Title || st.Description != sess.Description {
		t.Fatalf("expected title %q and description %q on the wire row, got %q/%q", sess.Title, sess.Description, st.Title, st.Description)
	}
	if st.Phase != 2 || st.TotalPhases != 5 {
		t.Fatalf("expected phase 2/5 on the wire row, got %d/%d", st.Phase, st.TotalPhases)
	}
	if string(st.Plan) != sess.Plan {
		t.Fatalf("expected plan %q on the wire row, got %q", sess.Plan, st.Plan)
	}
	if len(st.RecentToolCalls) != 1 || st.RecentToolCalls[0].Name != "Bash" {
		t.Fatalf("unexpected recent tool calls on the wire row: %+v", st.RecentToolCalls)
	}
	if st.Summary != "wired it up" {
		t.Fatalf("expected summary %q on the wire row, got %q", "wired it up", st.Summary)
	}

	// The empty row omits them all: the wire must not carry a "plan":null
	// or an empty task/title/description the browser would have to
	// second-guess.
	b, err := json.Marshal(BuildSessionState(store.Session{ID: "sess-2"}, store.SessionUsageSummary{}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"task"`, `"title"`, `"description"`, `"phase"`, `"total_phases"`, `"plan"`, `"recent_tool_calls"`, `"summary"`} {
		if jsonContains(b, absent) {
			t.Fatalf("expected %s omitted when empty, got %s", absent, b)
		}
	}
}

func jsonContains(b []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(b); i++ {
		if string(b[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
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
