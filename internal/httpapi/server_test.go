package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New()
	api := &Server{Store: st, Hub: h, Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "static placeholder")
	})}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st, h
}

func mustCreateSession(t *testing.T, st *store.Store, id string, createdAt time.Time) {
	t.Helper()
	err := st.CreateSession(context.Background(), store.Session{
		ID:             id,
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      "/tmp/" + id,
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// appendAndPublish appends inputs to sessionID and fans them out through h,
// standing in for what session.Runner does after every commit (append,
// then publish, in that order) without needing a real DeepSeek backend.
func appendAndPublish(t *testing.T, st *store.Store, h *hub.Hub, sessionID string, inputs []store.EventInput) []store.Event {
	t.Helper()
	appended, err := st.AppendEvents(context.Background(), sessionID, inputs)
	if err != nil {
		t.Fatalf("append events: %v", err)
	}
	h.PublishEvents(sessionID, appended)
	return appended
}

// --- method gate ---

func TestNonGetMethodsReturn405Everywhere(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	paths := []string{
		"/",
		"/api/sessions",
		"/api/sessions/sess-1",
		"/api/sessions/sess-1/events",
		"/api/sessions/sess-1/stream",
		"/api/stream",
		"/api/queue",
		"/api/sessions/does-not-exist",
		"/totally/unregistered/path",
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions}

	client := srv.Client()
	for _, p := range paths {
		for _, m := range methods {
			req, err := http.NewRequest(m, srv.URL+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, p, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, p, resp.StatusCode)
			}
			if resp.Header.Get("Allow") == "" {
				t.Errorf("%s %s: expected an Allow header on 405", m, p)
			}
		}
	}
}

func TestGetOnUnregisteredPathIsNot405(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Falls through to the static handler (the frontend's SPA fallback in
	// production); it must not be rejected as a method violation.
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatal("GET on an unregistered path should not be gated as a bad method")
	}
}

func TestHeadIsAllowed(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp, err := http.Head(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /api/sessions: got %d, want 200", resp.StatusCode)
	}
}

// --- session list and metadata ---

func TestListSessionsNewestFirst(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	mustCreateSession(t, st, "older", now.Add(-time.Hour))
	mustCreateSession(t, st, "newer", now)

	resp, err := http.Get(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d", resp.StatusCode)
	}
	var states []hub.SessionState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(states))
	}
	if states[0].ID != "newer" || states[1].ID != "older" {
		t.Fatalf("expected newest first, got %s then %s", states[0].ID, states[1].ID)
	}
}

func TestListSessionsIncludesUsageAndRequestID(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("claim work request: %v", err)
	}
	if err := st.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("set work request session: %v", err)
	}
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindUsage, Payload: store.UsagePayload{PromptCacheHitTokens: 50, CompletionTokens: 3, CostUSD: 0.0005}},
	})

	resp, err := http.Get(srv.URL + "/api/sessions/sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got hub.SessionState
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-1" {
		t.Fatalf("expected request_id req-1, got %q", got.RequestID)
	}
	if got.SubTurns != 1 || got.Usage.CacheHitTokens != 50 {
		t.Fatalf("unexpected usage summary: %+v", got)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/sessions/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
}

// --- paged events ---

func TestGetEventsPagingRanges(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	var inputs []store.EventInput
	for i := 0; i < 7; i++ {
		inputs = append(inputs, store.EventInput{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: strconv.Itoa(i)}})
	}
	appendAndPublish(t, st, h, "sess-1", inputs)
	// seq now runs 1..7.

	get := func(qs string) eventsPage {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/sessions/sess-1/events" + qs)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got status %d", qs, resp.StatusCode)
		}
		var page eventsPage
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}

	all := get("")
	if len(all.Events) != 7 {
		t.Fatalf("expected all 7 events with no params, got %d", len(all.Events))
	}
	if all.Next != nil {
		t.Fatalf("expected no next cursor when everything fit, got %v", *all.Next)
	}

	page1 := get("?from=1&limit=3")
	if len(page1.Events) != 3 || page1.Events[0].Seq != 1 || page1.Events[2].Seq != 3 {
		t.Fatalf("unexpected first page: %+v", page1.Events)
	}
	if page1.Next == nil || *page1.Next != 4 {
		t.Fatalf("expected next cursor 4, got %v", page1.Next)
	}

	page2 := get(fmt.Sprintf("?from=%d&limit=3", *page1.Next))
	if len(page2.Events) != 3 || page2.Events[0].Seq != 4 || page2.Events[2].Seq != 6 {
		t.Fatalf("unexpected second page: %+v", page2.Events)
	}

	page3 := get(fmt.Sprintf("?from=%d&limit=3", *page2.Next))
	if len(page3.Events) != 1 || page3.Events[0].Seq != 7 {
		t.Fatalf("unexpected final page: %+v", page3.Events)
	}
	if page3.Next != nil {
		t.Fatalf("expected no next cursor on the final, partial page, got %v", *page3.Next)
	}
}

func TestGetEventsUnknownSession(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/sessions/does-not-exist/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
}

// --- SSE: session transcript stream ---

// sseReader reads "id: N\ndata: ...\n\n" frames off body, skipping bare
// keep-alive comment lines, matching the shape internal/httpapi writes.
type sseReader struct {
	r *bufio.Reader
}

type sseFrame struct {
	id   string
	data string
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReader(body)}
}

func (s *sseReader) next() (sseFrame, error) {
	var frame sseFrame
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return sseFrame{}, err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if frame.data != "" {
				return frame, nil
			}
			// blank line with nothing accumulated: a keep-alive comment's
			// terminator, keep reading
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive, ignore
		case strings.HasPrefix(line, "id: "):
			frame.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestSessionStreamRepliesFullHistoryThenLive(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "go"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d", resp.StatusCode)
	}

	sr := newSSEReader(resp.Body)
	frame, err := sr.next()
	if err != nil {
		t.Fatalf("read historical frame: %v", err)
	}
	if frame.id != "1" {
		t.Fatalf("expected seq 1 from history, got id %q", frame.id)
	}

	// Now publish a live event and confirm it arrives on the same
	// connection without reconnecting.
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "hi"}},
	})
	frame, err = sr.next()
	if err != nil {
		t.Fatalf("read live frame: %v", err)
	}
	if frame.id != "2" {
		t.Fatalf("expected seq 2 live, got id %q", frame.id)
	}
}

// TestSessionStreamLastEventIDResumesWithoutGapOrDuplicate is the exact
// property docs/DESIGN.md §4.2 calls out: a reconnect with Last-Event-ID
// picks up from exactly where it left off, no gap and no duplicate.
func TestSessionStreamLastEventIDResumesWithoutGapOrDuplicate(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
		{Kind: store.KindTurnFinished, Payload: store.TurnFinishedPayload{FinishReason: "stop"}},
	})
	// seq 1, 2, 3 committed.

	// First connection: read only the first two frames, then disconnect
	// deliberately (simulating a dropped tab / network blip) before the
	// third has been read.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	req1, err := http.NewRequestWithContext(ctx1, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	sr1 := newSSEReader(resp1.Body)
	var lastSeen string
	for i := 0; i < 2; i++ {
		frame, err := sr1.next()
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		lastSeen = frame.id
	}
	if lastSeen != "2" {
		t.Fatalf("expected to have seen seq 2, got %q", lastSeen)
	}
	resp1.Body.Close()
	cancel1()

	// More events land on the session before the reconnect.
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 2}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "b"}},
	})
	// seq 4, 5 now committed, in addition to the still-unread seq 3.

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Last-Event-ID", lastSeen)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	sr2 := newSSEReader(resp2.Body)
	var gotIDs []string
	for i := 0; i < 3; i++ {
		frame, err := sr2.next()
		if err != nil {
			t.Fatalf("read resumed frame %d: %v", i, err)
		}
		gotIDs = append(gotIDs, frame.id)
	}
	want := []string{"3", "4", "5"}
	for i, id := range gotIDs {
		if id != want[i] {
			t.Fatalf("resumed sequence %v, want %v (no gap, no duplicate)", gotIDs, want)
		}
	}
}

// TestFinishedSessionServesFullTranscriptAndCloses asserts a session
// already at a terminal status still serves its whole history from the
// store on connect, and that the stream ends instead of idling forever
// since nothing more will ever be appended (docs/DESIGN.md §4.2).
func TestFinishedSessionServesFullTranscriptAndCloses(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "go"}},
		{Kind: store.KindRunFinished, Payload: store.RunFinishedPayload{Reason: "complete", Text: "done"}},
	})
	finished := time.Now().UTC()
	if err := st.UpdateSessionStatus(context.Background(), "sess-1", store.StatusOK, &finished); err != nil {
		t.Fatalf("update status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sr := newSSEReader(resp.Body)
	var ids []string
	for i := 0; i < 2; i++ {
		frame, err := sr.next()
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		ids = append(ids, frame.id)
	}
	if ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("unexpected history: %v", ids)
	}

	// No more data will ever come; the server must close the response
	// rather than hold the connection open forever.
	done := make(chan error, 1)
	go func() {
		_, err := sr.next()
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("expected the stream to end with EOF, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected a finished session's stream to close, but it stayed open")
	}
}

// TestSlowSessionSubscriberDoesNotBlockProducer opens an SSE connection and
// never reads from it, then confirms a burst of publishes to the same
// session completes promptly anyway — the backpressure property
// docs/DESIGN.md §5 requires at the HTTP surface, not just inside the hub.
func TestSlowSessionSubscriberDoesNotBlockProducer(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Deliberately never read resp.Body from here on.

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			appendAndPublish(t, st, h, "sess-1", []store.EventInput{
				{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "x"}},
			})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publishing stalled behind a subscriber that never read its stream")
	}
}

// --- SSE: session-list stream ---

func TestListStreamSnapshotThenUpdate(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sr := newSSEReader(resp.Body)
	frame, err := sr.next()
	if err != nil {
		t.Fatalf("read snapshot frame: %v", err)
	}
	var snap hub.SessionState
	if err := json.Unmarshal([]byte(frame.data), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ID != "sess-1" || snap.Status != store.StatusRunning {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// A new session's creation reaches this subscriber without a
	// reconnect, and the list stream stays quiet otherwise (no transcript
	// deltas leak into it) — verified implicitly, since the transcript
	// events published in other tests use a different session id and this
	// subscriber only ever asked for the list.
	mustCreateSession(t, st, "sess-2", time.Now())
	h.PublishSessionState(hub.BuildSessionState(mustGetSession(t, st, "sess-2"), store.SessionUsageSummary{}, "", ""))

	frame, err = sr.next()
	if err != nil {
		t.Fatalf("read update frame: %v", err)
	}
	var update hub.SessionState
	if err := json.Unmarshal([]byte(frame.data), &update); err != nil {
		t.Fatal(err)
	}
	if update.ID != "sess-2" {
		t.Fatalf("expected update for sess-2, got %+v", update)
	}
}

func mustGetSession(t *testing.T, st *store.Store, id string) store.Session {
	t.Helper()
	sess, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// --- queue health ---

type fakeConsumer struct {
	info *jetstream.ConsumerInfo
	err  error
}

func (f fakeConsumer) Info(ctx context.Context) (*jetstream.ConsumerInfo, error) {
	return f.info, f.err
}

type fakePool struct {
	halted bool
	reason string
}

func (f fakePool) Halted() (bool, string) { return f.halted, f.reason }

func getQueueHealth(t *testing.T, srv *httptest.Server) queueHealth {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/api/queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var qh queueHealth
	if err := json.NewDecoder(resp.Body).Decode(&qh); err != nil {
		t.Fatal(err)
	}
	return qh
}

// TestQueueHealthWithNoConsumerReportsUnavailable is the shape a CLI-only
// harness gets: no NATS queue wired up at all, so the endpoint reports
// unavailable rather than panicking on a nil Consumer.
func TestQueueHealthWithNoConsumerReportsUnavailable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{Store: st, Hub: hub.New(), Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	qh := getQueueHealth(t, srv)
	if qh.Available {
		t.Fatalf("expected available=false with no Consumer, got %+v", qh)
	}
	if qh.Halted {
		t.Fatalf("expected halted=false with no Pool, got %+v", qh)
	}
}

// TestQueueHealthReportsConsumerFiguresAndHaltState covers the three
// figures PLAN.md phase 6 asks the session list to show — consumer lag,
// in-flight count, redelivery count — plus the pool's halted state, all
// against fakes so the test needs no real NATS server.
func TestQueueHealthReportsConsumerFiguresAndHaltState(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		Consumer: fakeConsumer{info: &jetstream.ConsumerInfo{
			NumPending: 7, NumAckPending: 2, NumRedelivered: 1,
		}},
		Pool: fakePool{halted: true, reason: "account balance exhausted (402 from DeepSeek)"},
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	qh := getQueueHealth(t, srv)
	if !qh.Available {
		t.Fatalf("expected available=true, got %+v", qh)
	}
	if qh.ConsumerLag != 7 || qh.InFlight != 2 || qh.Redelivered != 1 {
		t.Fatalf("unexpected queue figures: %+v", qh)
	}
	if !qh.Halted || qh.HaltReason == "" {
		t.Fatalf("expected the halt state to be reported, got %+v", qh)
	}
}

// TestQueueHealthSurfacesConsumerInfoError covers a live NATS call that
// itself fails (server unreachable, say): reported through Error rather
// than a 500, since the rest of the read-only surface still works fine.
func TestQueueHealthSurfacesConsumerInfoError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(),
		Static:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		Consumer: fakeConsumer{err: errors.New("nats: no responders available for request")},
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	qh := getQueueHealth(t, srv)
	if qh.Available {
		t.Fatalf("expected available=false when Consumer.Info errors, got %+v", qh)
	}
	if qh.Error == "" {
		t.Fatal("expected the Consumer.Info error to be surfaced")
	}
}
