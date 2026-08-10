package httpapi

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	_ "modernc.org/sqlite"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub) {
	t.Helper()
	srv, st, h, _ := newTestServerWithDB(t)
	return srv, st, h
}

// newTestServerWithDB is newTestServer plus the SQLite file path, for tests
// that must rewrite the store directly (backdating an event's created_at to
// age a session past the idle threshold).
func newTestServerWithDB(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New()
	api := &Server{
		Store: st, Hub: h, Settings: settings.NewResolver(st),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
	}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st, h, path
}

// backdateEvents rewrites every event of sessionID's log to at, so the
// session's most recent event is genuinely older than the idle threshold. It
// opens a second connection to the same SQLite file the store holds — the
// same trick store_test.go uses to build a legacy database — and is safe
// here because nothing else is writing during the test.
func backdateEvents(t *testing.T, dbPath, sessionID string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}
	if _, err := db.Exec(`UPDATE events SET created_at = ? WHERE session_id = ?`, at.UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		t.Fatalf("backdate events: %v", err)
	}
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

// TestNonGetMethodsReturn405EverywhereExceptWriteRoutes pins the method gate:
// a non-GET/HEAD request 405s on every path — including paths nothing here
// recognises — with an Allow header naming what the path actually permits.
// The paths with a write route are the exceptions: PUT and DELETE on a
// settings key path, PATCH and DELETE on a session path (docs/DATA-API.md),
// which pass the gate and are asserted separately; the other methods still
// 405 there with all four allowed methods named.
func TestNonGetMethodsReturn405EverywhereExceptWriteRoutes(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	// Paths with no write route: every non-GET/HEAD method 405s, and the
	// Allow header names GET and HEAD only.
	plainPaths := []string{
		"/",
		"/api/sessions", // the collection has no write route
		"/api/sessions/sess-1/events",
		"/api/sessions/sess-1/stream",
		"/api/requests",              // the collection has no write route
		"/api/requests/req-1/status", // the poll snapshot is never writable
		"/api/leases",                // the collection has no write route
		"/api/stream",
		"/api/queue",
		"/api/settings",     // the collection path has no write route
		"/api/settings/",    // an empty key segment is not a key path
		"/api/settings/a/b", // not exactly one key segment
		"/totally/unregistered/path",
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions}

	client := srv.Client()
	for _, p := range plainPaths {
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
			if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
				t.Errorf("%s %s: Allow = %q, want %q", m, p, got, "GET, HEAD")
			}
		}
	}

	// A settings key path allows PUT and DELETE; every other method still
	// 405s there, naming all four allowed methods in Allow.
	settingsPath := "/api/settings/deepseek.api_key"
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(m, srv.URL+settingsPath, strings.NewReader(`{"value":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", m, settingsPath, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s: gate refused a permitted write", m, settingsPath)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodOptions} {
		req, err := http.NewRequest(m, srv.URL+settingsPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", m, settingsPath, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: got status %d, want 405", m, settingsPath, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, PUT, DELETE" {
			t.Errorf("%s %s: Allow = %q, want %q", m, settingsPath, got, "GET, HEAD, PUT, DELETE")
		}
	}

	// A session path allows PATCH and DELETE; every other method still 405s
	// there, naming all four allowed methods in Allow. The gate must let the
	// writing methods through even when the session id is unknown — the
	// handler, not the gate, owns the 404.
	for _, sessionPath := range []string{"/api/sessions/sess-1", "/api/sessions/does-not-exist"} {
		for _, m := range []string{http.MethodPatch, http.MethodDelete} {
			req, err := http.NewRequest(m, srv.URL+sessionPath, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, sessionPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Errorf("%s %s: gate refused a permitted write", m, sessionPath)
			}
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodOptions} {
			req, err := http.NewRequest(m, srv.URL+sessionPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, sessionPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, sessionPath, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET, HEAD, PATCH, DELETE" {
				t.Errorf("%s %s: Allow = %q, want %q", m, sessionPath, got, "GET, HEAD, PATCH, DELETE")
			}
		}
	}

	// A work-request path allows PATCH and DELETE; every other method still
	// 405s there, naming all four allowed methods in Allow.
	for _, requestPath := range []string{"/api/requests/req-1", "/api/requests/does-not-exist"} {
		for _, m := range []string{http.MethodPatch, http.MethodDelete} {
			req, err := http.NewRequest(m, srv.URL+requestPath, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, requestPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Errorf("%s %s: gate refused a permitted write", m, requestPath)
			}
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodOptions} {
			req, err := http.NewRequest(m, srv.URL+requestPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, requestPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, requestPath, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET, HEAD, PATCH, DELETE" {
				t.Errorf("%s %s: Allow = %q, want %q", m, requestPath, got, "GET, HEAD, PATCH, DELETE")
			}
		}
	}

	// A workspace-lease path allows DELETE (release); every other method
	// still 405s there, naming the three allowed methods in Allow.
	for _, leasePath := range []string{"/api/leases/ws-1", "/api/leases/does-not-exist"} {
		resp := doWrite(t, srv, http.MethodDelete, leasePath, "", map[string]string{"Content-Type": "application/json"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s: gate refused a permitted write", leasePath)
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodOptions} {
			req, err := http.NewRequest(m, srv.URL+leasePath, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, leasePath, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, leasePath, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET, HEAD, DELETE" {
				t.Errorf("%s %s: Allow = %q, want %q", m, leasePath, got, "GET, HEAD, DELETE")
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

// --- request status ---

func TestRequestStatusClaimedNoSession(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/requests/req-1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	var got requestStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-1" || got.SessionID != "" || got.Status != "running" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if got.SubTurn != 0 || len(got.Todos) != 0 || len(got.ToolCalls) != 0 || got.Usage != nil {
		t.Fatalf("expected no event-derived state, got %+v", got)
	}
	if got.TranscriptURL != "" {
		t.Fatalf("expected no transcript URL without a session, got %q", got.TranscriptURL)
	}
	if got.DurationMS < 0 {
		t.Fatalf("expected a non-negative duration, got %d", got.DurationMS)
	}
}

func TestRequestStatusSetupFailure(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.SetWorkRequestSession(ctx, "req-1", "sess-attempt"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	result := json.RawMessage(`{"request_id":"req-1","session_id":"sess-attempt","status":"failed",
		"error":{"code":"workspace_setup","message":"clone refused"}}`)
	if matched, err := st.FinishWorkRequest(ctx, "req-1", "sess-attempt", "failed", result, time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	resp, err := http.Get(srv.URL + "/api/requests/req-1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got requestStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("expected status failed, got %q", got.Status)
	}
	if got.ErrorCode != "workspace_setup" || got.ErrorMessage != "clone refused" {
		t.Fatalf("expected the stored failure to surface, got %q/%q", got.ErrorCode, got.ErrorMessage)
	}
}

func TestRequestStatusMidFlight(t *testing.T) {
	srv, st, h := newTestServer(t)
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	mustCreateSession(t, st, "sess-1", time.Now().UTC().Add(-time.Minute))
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 3}},
		{Kind: store.KindToolCall, Payload: store.ToolCallPayload{ID: "call-todo", Name: "TodoWrite",
			Arguments: `{"todos":[{"content":"fix it","status":"in_progress","activeForm":"Fixing it"}]}`}},
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{ToolCallID: "call-todo", Name: "TodoWrite", Content: "ok"}},
		{Kind: store.KindToolCall, Payload: store.ToolCallPayload{ID: "call-bash", Name: "Bash", Arguments: `{"command":"go test ./..."}`}},
		{Kind: store.KindUsage, Payload: store.UsagePayload{SubTurn: 3, PromptCacheMissTokens: 200, CompletionTokens: 10, CostUSD: 0.003}},
	})

	resp, err := http.Get(srv.URL + "/api/requests/req-1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got requestStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-1" || got.SessionID != "sess-1" || got.Status != "running" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if got.SubTurn != 3 {
		t.Fatalf("expected sub-turn 3, got %d", got.SubTurn)
	}
	if len(got.Todos) != 1 || got.Todos[0].Content != "fix it" || got.ActiveForm != "Fixing it" {
		t.Fatalf("unexpected todos: %+v (activeForm %q)", got.Todos, got.ActiveForm)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call-bash" {
		t.Fatalf("expected the Bash call in flight, got %+v", got.ToolCalls)
	}
	if got.Usage == nil || got.Usage.CostUSD != 0.003 {
		t.Fatalf("expected the latest usage event, got %+v", got.Usage)
	}
	if got.TranscriptURL != "/sessions/sess-1" {
		t.Fatalf("expected the transcript path, got %q", got.TranscriptURL)
	}
}

func TestRequestStatusNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/requests/does-not-exist/status")
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
// figures the session list shows — consumer lag, in-flight count,
// redelivery count — plus the pool's halted state, all
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

// --- settings ---

// doWrite issues one write (PUT, PATCH, DELETE) against the given path with
// the given extra headers, returning the response for the caller to inspect.
func doWrite(t *testing.T, srv *httptest.Server, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestGetSettingsMasksSecretsAndListsAllKeys(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, settings.KeyDeepSeekAPIKey, "sk-very-secret-1234"); err != nil {
		t.Fatalf("set deepseek key: %v", err)
	}
	if err := st.SetSetting(ctx, settings.KeyGoogleVisionModel, "gemini-3.6-flash"); err != nil {
		t.Fatalf("set vision model: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// The stored secret's full value must not appear anywhere in the body.
	if strings.Contains(string(body), "sk-very-secret-1234") {
		t.Fatalf("stored secret appeared in full in the response body: %s", body)
	}

	var got []settingEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(settings.ValidKeys) {
		t.Fatalf("expected %d entries, got %d: %+v", len(settings.ValidKeys), len(got), got)
	}
	for i, key := range settings.ValidKeys {
		if got[i].Key != key {
			t.Fatalf("entry %d key = %q, want %q", i, got[i].Key, key)
		}
	}

	byKey := map[string]settingEntry{}
	for _, e := range got {
		byKey[e.Key] = e
	}

	key := byKey[settings.KeyDeepSeekAPIKey]
	if !key.Set || key.Value != maskSecret("sk-very-secret-1234") || !key.Secret || key.Override != true {
		t.Fatalf("deepseek.api_key entry = %+v, want set, masked, secret, override", key)
	}
	gkey := byKey[settings.KeyGoogleAPIKey]
	if gkey.Set || gkey.Value != "" || !gkey.Secret {
		t.Fatalf("google.api_key should be unset with no value, got %+v", gkey)
	}
	vision := byKey[settings.KeyGoogleVisionModel]
	if !vision.Set || vision.Value != "gemini-3.6-flash" || vision.Secret {
		t.Fatalf("google.vision_model entry = %+v, want set with its full value", vision)
	}
	// A set value that differs from the default is an override; an unset key
	// is not.
	if byKey[settings.KeyRunMaxTokens].Override {
		t.Fatalf("run.max_tokens should not be an override when unset: %+v", byKey[settings.KeyRunMaxTokens])
	}
	if !byKey[settings.KeyWorkerPoolSize].Restart {
		t.Fatalf("worker.pool_size must carry the restart flag: %+v", byKey[settings.KeyWorkerPoolSize])
	}
	if byKey[settings.KeyRunMaxTokens].Restart {
		t.Fatalf("run.max_tokens must not carry the restart flag: %+v", byKey[settings.KeyRunMaxTokens])
	}
	if byKey[settings.KeyRunMaxTokens].Type != "integer" || byKey[settings.KeyRunDeadline].Type != "duration" || byKey[settings.KeyDefaultModel].Type != "string" {
		t.Fatalf("type field must mirror the registry: %+v %+v %+v", byKey[settings.KeyRunMaxTokens], byKey[settings.KeyRunDeadline], byKey[settings.KeyDefaultModel])
	}
	if byKey[settings.KeyRunMaxTokens].Default != "48000" {
		t.Fatalf("default field = %q, want 48000", byKey[settings.KeyRunMaxTokens].Default)
	}

	// The bounds phase 7 adds (docs/WEB-REDESIGN.md): an integer setting's min
	// and max arrive as JSON numbers, a duration's as compact Go duration text
	// ("1s", "24h" — never "24h0m0s"), and a plain string setting carries
	// neither, so its payload does not grow a pair of meaningless zeroes.
	if min, ok := byKey[settings.KeyRunMaxTokens].Min.(float64); !ok || min != 1 {
		t.Fatalf("run.max_tokens min = %v (%T), want 1", byKey[settings.KeyRunMaxTokens].Min, byKey[settings.KeyRunMaxTokens].Min)
	}
	if max, ok := byKey[settings.KeyRunMaxTokens].Max.(float64); !ok || max != 1_000_000 {
		t.Fatalf("run.max_tokens max = %v (%T), want 1000000", byKey[settings.KeyRunMaxTokens].Max, byKey[settings.KeyRunMaxTokens].Max)
	}
	if min, ok := byKey[settings.KeyRunDeadline].Min.(string); !ok || min != "1s" {
		t.Fatalf("run.deadline min = %v (%T), want \"1s\"", byKey[settings.KeyRunDeadline].Min, byKey[settings.KeyRunDeadline].Min)
	}
	if max, ok := byKey[settings.KeyRunDeadline].Max.(string); !ok || max != "8760h" {
		t.Fatalf("run.deadline max = %v (%T), want \"8760h\"", byKey[settings.KeyRunDeadline].Max, byKey[settings.KeyRunDeadline].Max)
	}
	if max, ok := byKey[settings.KeyToolBashTimeout].Max.(string); !ok || max != "24h" {
		t.Fatalf("tools.bash_timeout max = %v (%T), want \"24h\"", byKey[settings.KeyToolBashTimeout].Max, byKey[settings.KeyToolBashTimeout].Max)
	}
	// model.effort is the one closed set: allowed must be serialised so the
	// screen can draw a ToggleGroup instead of a text input.
	if allowed := byKey[settings.KeyDefaultEffort].Allowed; len(allowed) != 3 || allowed[0] != "low" || allowed[1] != "high" || allowed[2] != "max" {
		t.Fatalf("model.effort allowed = %v, want [low high max]", allowed)
	}
	// A string setting with no bounds or set must omit min/max/allowed
	// entirely — the payload's own keys prove it, not just the Go zero value.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, row := range raw {
		key := strings.Trim(string(row["key"]), `"`)
		d, ok := settings.Lookup(key)
		if !ok {
			t.Fatalf("row for unknown key %q", key)
		}
		_, hasMin := row["min"]
		_, hasMax := row["max"]
		_, hasAllowed := row["allowed"]
		wantBounds := d.Type == settings.TypeInteger || d.Type == settings.TypeDuration
		if hasMin != wantBounds || hasMax != wantBounds {
			t.Fatalf("%s: min/max present = %v/%v, want %v (type %s)", key, hasMin, hasMax, wantBounds, d.Type)
		}
		if hasAllowed != (len(d.Allowed) > 0) {
			t.Fatalf("%s: allowed present = %v, want %v", key, hasAllowed, len(d.Allowed) > 0)
		}
	}
}

// entryByKey fetches GET /api/settings and returns the entry for key.
func entryByKey(t *testing.T, srv *httptest.Server, key string) settingEntry {
	t.Helper()
	getResp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var got []settingEntry
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", key, got)
	return settingEntry{}
}

func TestPutSettingThenGetShowsItSet(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := doWrite(t, srv, http.MethodPut, "/api/settings/google.vision_model", `{"value":"gemini-3.6-flash"}`, map[string]string{
		"Content-Type": "application/json",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got status %d, want 200", resp.StatusCode)
	}

	entry := entryByKey(t, srv, settings.KeyGoogleVisionModel)
	if entry.Key != settings.KeyGoogleVisionModel || !entry.Set || entry.Value != "gemini-3.6-flash" {
		t.Fatalf("google.vision_model after PUT = %+v, want set with the written value", entry)
	}
	if !entry.Override {
		t.Fatalf("a value differing from the default must be marked as an override: %+v", entry)
	}
}

// TestPutSettingRejectsOutOfRangeValue pins the "one bound, enforced in Go"
// property: a value the registry rejects comes back as a 400 carrying the
// registry's message — the same message harness config set prints — so the
// screen can surface it verbatim.
func TestPutSettingRejectsOutOfRangeValue(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := doWrite(t, srv, http.MethodPut, "/api/settings/tools.bash_timeout", `{"value":"-5s"}`, map[string]string{
		"Content-Type": "application/json",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "out of range") {
		t.Fatalf("error body %q does not carry the registry's validation message", body)
	}

	// The rejected value must not have been stored.
	entry := entryByKey(t, srv, settings.KeyToolBashTimeout)
	if entry.Set {
		t.Fatalf("rejected value was stored: %+v", entry)
	}
}

func TestPutSettingUnknownKeyReturns400(t *testing.T) {
	srv, st, _ := newTestServer(t)
	resp := doWrite(t, srv, http.MethodPut, "/api/settings/deepsek.api_key", `{"value":"sk-..."}`, map[string]string{
		"Content-Type": "application/json",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error response Content-Type = %q, want JSON", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "unknown setting") {
		t.Fatalf("error body %q does not carry the UnknownKeyError message", body)
	}
	// The typo'd key must not have been stored.
	if _, ok, err := st.Setting(context.Background(), "deepsek.api_key"); err != nil || ok {
		t.Fatalf("unknown key was stored (ok=%v err=%v)", ok, err)
	}
}

func TestPutSettingContentTypeRequired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		name string
		ct   string
	}{
		{"missing", ""},
		{"wrong", "text/plain"},
		{"form-encoded", "application/x-www-form-urlencoded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.ct != "" {
				headers["Content-Type"] = tc.ct
			}
			resp := doWrite(t, srv, http.MethodPut, "/api/settings/deepseek.api_key", `{"value":"sk-x"}`, headers)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q: got status %d, want 415", tc.ct, resp.StatusCode)
			}
		})
	}
}

func TestSettingsWritesOriginCheck(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// A foreign Origin is refused with 403 on both writing methods.
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		resp := doWrite(t, srv, m, "/api/settings/deepseek.api_key", `{"value":"sk-x"}`, map[string]string{
			"Content-Type": "application/json",
			"Origin":       "https://evil.example",
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with foreign Origin: got status %d, want 403", m, resp.StatusCode)
		}
	}

	// A same-origin write passes.
	resp := doWrite(t, srv, http.MethodPut, "/api/settings/google.vision_model", `{"value":"gemini-3.7-flash"}`, map[string]string{
		"Content-Type": "application/json",
		"Origin":       srv.URL,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin PUT: got status %d, want 200", resp.StatusCode)
	}
}

func TestDeleteSettingUnsetsAndRejectsUnknownKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	headers := map[string]string{"Content-Type": "application/json"}

	// Set a key, then delete it; the GET shows it unset again.
	resp := doWrite(t, srv, http.MethodPut, "/api/settings/deepseek.api_key", `{"value":"sk-x"}`, headers)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got status %d, want 200", resp.StatusCode)
	}
	resp = doWrite(t, srv, http.MethodDelete, "/api/settings/deepseek.api_key", "", headers)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: got status %d, want 200", resp.StatusCode)
	}

	getResp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var got []settingEntry
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got[0].Set {
		t.Fatalf("deepseek.api_key still set after DELETE: %+v", got[0])
	}

	// Deleting an unknown key is a 400 carrying UnknownKeyError's message.
	resp = doWrite(t, srv, http.MethodDelete, "/api/settings/not.a.key", "", headers)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE unknown key: got status %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "unknown setting") {
		t.Fatalf("error body %q does not carry the UnknownKeyError message", body)
	}
}

// --- session writes ---

// sessionVersion fetches one session's row and returns its version, standing
// in for the client reading GET /api/sessions/{id} before a write — the
// value it must echo back in If-Match (docs/DATA-API.md).
func sessionVersion(t *testing.T, srv *httptest.Server, id string) int {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions/%s: got status %d, want 200", id, resp.StatusCode)
	}
	var got hub.SessionState
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version < 1 {
		t.Fatalf("session %s: expected a positive version, got %d", id, got.Version)
	}
	return got.Version
}

// TestPatchSessionClosesAbandonedSession pins the endpoint's success path:
// a running session that has been quiet past the idle threshold — or has
// never appended an event at all — can be closed into a terminal status,
// which sets finished_at and bumps the version, and the close is visible to
// a fresh GET.
func TestPatchSessionClosesAbandonedSession(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()

	// An abandoned session: a worker appended one event two hours ago and
	// died. The event is backdated so the row is genuinely quiet.
	mustCreateSession(t, st, "abandoned", time.Now().Add(-2*time.Hour))
	appendAndPublish(t, st, h, "abandoned", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "hi"}},
	})
	backdateEvents(t, dbPath, "abandoned", time.Now().Add(-2*time.Hour))

	// A session that never appended an event has nothing recent and passes
	// the idle check the same way.
	mustCreateSession(t, st, "never-heard-from", time.Now().Add(-2*time.Hour))

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"with old events", "abandoned"},
		{"with no events", "never-heard-from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := sessionVersion(t, srv, tc.id)
			resp := doWrite(t, srv, http.MethodPatch, "/api/sessions/"+tc.id, `{"status":"cancelled"}`, map[string]string{
				"Content-Type": "application/json",
				"If-Match":     strconv.Itoa(before),
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PATCH: got status %d, want 200", resp.StatusCode)
			}
			var got hub.SessionState
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Status != store.StatusCancelled || got.FinishedAt == nil {
				t.Fatalf("expected cancelled with finished_at, got %+v", got)
			}
			if got.Version != before+1 {
				t.Fatalf("version = %d, want %d (one mutation)", got.Version, before+1)
			}

			// A fresh read agrees, and the event log is untouched.
			sess, err := st.GetSession(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if sess.Status != store.StatusCancelled || sess.FinishedAt == nil {
				t.Fatalf("store row not closed: %+v", sess)
			}
			events, err := st.GetEvents(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range events {
				if ev.Kind == store.KindRunFinished || ev.Kind == store.KindError {
					t.Fatalf("close must not append events, found %s", ev.Kind)
				}
			}
		})
	}
}

// TestPatchSessionRefusesLiveSession pins the precondition that matters
// (docs/DATA-API.md): a running session whose most recent event is newer
// than the idle threshold is presumed live, and the close is a 409 whose
// message names when the last event was. The row is untouched.
func TestPatchSessionRefusesLiveSession(t *testing.T) {
	srv, st, h := newTestServer(t)
	ctx := context.Background()
	mustCreateSession(t, st, "live", time.Now())
	appendAndPublish(t, st, h, "live", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "hi"}},
	})

	before := sessionVersion(t, srv, "live")
	resp := doWrite(t, srv, http.MethodPatch, "/api/sessions/live", `{"status":"cancelled"}`, map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(before),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("PATCH on a live session: got status %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "most recent event at") {
		t.Fatalf("409 must say when the last event was, got %q", body)
	}

	sess, err := st.GetSession(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusRunning || sess.FinishedAt != nil || sess.Version != before {
		t.Fatalf("a refused close must not touch the row, got %+v", sess)
	}
}

// TestSessionWritesGuardsAndPreconditions is the table the task pins for
// both session write endpoints: the guards every write carries (415 without
// a JSON content type, 403 cross-origin) and the write preconditions (428
// without If-Match, 400 on a malformed or non-terminal body).
func TestSessionWritesGuardsAndPreconditions(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, tc := range []struct {
		name    string
		method  string
		body    string
		headers map[string]string
		want    int
	}{
		{
			name: "PATCH missing content type", method: http.MethodPatch,
			body: `{"status":"cancelled"}`, headers: map[string]string{"If-Match": "1"},
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "PATCH wrong content type", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "text/plain", "If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "PATCH cross-origin", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name: "PATCH missing If-Match", method: http.MethodPatch,
			body: `{"status":"cancelled"}`, headers: map[string]string{"Content-Type": "application/json"},
			want: http.StatusPreconditionRequired,
		},
		{
			name: "PATCH malformed If-Match", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "abc"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH non-terminal status", method: http.MethodPatch,
			body:    `{"status":"running"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH unknown status", method: http.MethodPatch,
			body:    `{"status":"dancing"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH invalid JSON", method: http.MethodPatch,
			body:    `not json`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "DELETE missing content type", method: http.MethodDelete,
			headers: map[string]string{"If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "DELETE cross-origin", method: http.MethodDelete,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name: "DELETE missing If-Match", method: http.MethodDelete,
			headers: map[string]string{"Content-Type": "application/json"},
			want:    http.StatusPreconditionRequired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, tc.method, "/api/sessions/sess-1", tc.body, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			// Every refusal is a JSON error body.
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("error Content-Type = %q, want JSON", ct)
			}
		})
	}

	// The rejected writes left the row alone.
	sess, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusRunning || sess.Version != 1 {
		t.Fatalf("guarded writes must not touch the row, got %+v", sess)
	}
}

// TestSessionWritesRejectStaleVersion pins the optimistic-concurrency
// mechanism (docs/DATA-API.md) end to end: a write carrying a version older
// than the row's current one is a 412 that names the current version, and
// the row survives — for PATCH and DELETE alike.
func TestSessionWritesRejectStaleVersion(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	mustCreateSession(t, st, "sess-1", time.Now()) // version 1

	// The run's own terminal write bumps the row to version 2 — the exact
	// race the mechanism exists for: an operator who read the session before
	// the run finished must not land a write on the changed row.
	if err := st.UpdateSessionStatus(ctx, "sess-1", store.StatusOK, &staleFinishedAt); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		method string
		body   string
	}{
		{"PATCH", http.MethodPatch, `{"status":"cancelled"}`},
		{"DELETE", http.MethodDelete, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, tc.method, "/api/sessions/sess-1", tc.body, map[string]string{
				"Content-Type": "application/json",
				"If-Match":     "1", // stale: the row is at version 2
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("stale %s: got status %d, want 412", tc.method, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "current version 2") {
				t.Fatalf("412 must name the current version, got %q", body)
			}
		})
	}

	sess, err := st.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK || sess.Version != 2 {
		t.Fatalf("stale writes must not touch the row, got %+v", sess)
	}

	// The same write with the current version succeeds.
	resp := doWrite(t, srv, http.MethodPatch, "/api/sessions/sess-1", `{"status":"cancelled"}`, map[string]string{
		"Content-Type": "application/json",
		"If-Match":     "2",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh PATCH: got status %d, want 200", resp.StatusCode)
	}
}

// TestDeleteSessionRemovesFinishedSession pins DELETE's success path: a
// terminal session can be deleted with its current version, and the row and
// its event log are gone afterwards.
func TestDeleteSessionRemovesFinishedSession(t *testing.T) {
	srv, st, h := newTestServer(t)
	ctx := context.Background()
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "hi"}},
	})
	if err := st.UpdateSessionStatus(ctx, "sess-1", store.StatusOK, &staleFinishedAt); err != nil {
		t.Fatal(err)
	}

	v := sessionVersion(t, srv, "sess-1") // 2
	resp := doWrite(t, srv, http.MethodDelete, "/api/sessions/sess-1", "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(v),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: got status %d, want 200", resp.StatusCode)
	}
	var okBody map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&okBody); err != nil {
		t.Fatal(err)
	}
	if !okBody["ok"] {
		t.Fatalf("DELETE response = %+v, want ok:true", okBody)
	}

	if _, err := st.GetSession(ctx, "sess-1"); err != store.ErrNotFound {
		t.Fatalf("session should be gone, got %v", err)
	}
	events, err := st.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events left, got %d", len(events))
	}
}

// TestDeleteSessionRefusesRunning pins the surfaced store rule: DELETE
// refuses a running session with 409 regardless of idleness — an abandoned
// row is closed with PATCH first and deleted afterwards.
func TestDeleteSessionRefusesRunning(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	mustCreateSession(t, st, "sess-1", time.Now())

	resp := doWrite(t, srv, http.MethodDelete, "/api/sessions/sess-1", "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     "1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE on a running session: got status %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "still running") {
		t.Fatalf("409 must say the session is still running, got %q", body)
	}
	if _, err := st.GetSession(ctx, "sess-1"); err != nil {
		t.Fatalf("session should survive a refused delete: %v", err)
	}
}

// TestSessionWritesNotFound pins 404 for both write endpoints on an unknown
// session id.
func TestSessionWritesNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		method string
		body   string
	}{
		{http.MethodPatch, `{"status":"cancelled"}`},
		{http.MethodDelete, ""},
	} {
		resp := doWrite(t, srv, tc.method, "/api/sessions/does-not-exist", tc.body, map[string]string{
			"Content-Type": "application/json",
			"If-Match":     "1",
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown session: got status %d, want 404", tc.method, resp.StatusCode)
		}
	}
}

// staleFinishedAt is a non-nil finished_at for tests that finish a session
// through the store without caring about the exact time.
var staleFinishedAt = time.Now().UTC()

// --- work requests and workspace leases (phase 3) ---

// mustCreateWorkRequest claims requestID, creates sessionID, appends one
// event, and attaches the session — the row shape of a request whose attempt
// actually started. The event is backdated to eventAt, so the request's
// liveness is under the test's control: a recent eventAt is a live request,
// an old one a dead one.
func mustCreateWorkRequest(t *testing.T, st *store.Store, h *hub.Hub, dbPath, requestID, sessionID string, eventAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, requestID, 1, eventAt); err != nil {
		t.Fatalf("claim %s: %v", requestID, err)
	}
	mustCreateSession(t, st, sessionID, eventAt)
	appendAndPublish(t, st, h, sessionID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "hi"}},
	})
	backdateEvents(t, dbPath, sessionID, eventAt)
	if err := st.SetWorkRequestSession(ctx, requestID, sessionID); err != nil {
		t.Fatalf("attach session: %v", err)
	}
}

// requestVersion fetches one work-request row and returns its version,
// standing in for the client reading GET /api/requests/{request_id} before a
// write — the value it must echo back in If-Match (docs/DATA-API.md).
func requestVersion(t *testing.T, srv *httptest.Server, requestID string) int {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/requests/" + requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/requests/%s: got status %d, want 200", requestID, resp.StatusCode)
	}
	var got workRequestRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version < 1 {
		t.Fatalf("request %s: expected a positive version, got %d", requestID, got.Version)
	}
	return got.Version
}

// TestGetWorkRequestRow pins GET /api/requests/{request_id}: the idempotency
// row itself — request id, session id, status, result, received_at,
// finished_at, delivery count, version — distinct from the /status poll
// snapshot. A terminal row carries its stored result.
func TestGetWorkRequestRow(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	claimedAt := time.Now().UTC().Add(-time.Hour)
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, claimedAt); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.SetWorkRequestSession(ctx, "req-1", "sess-attempt"); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	finishedAt := time.Now().UTC()
	result := json.RawMessage(`{"status":"failed","error":{"code":"workspace_setup","message":"clone refused"}}`)
	if matched, err := st.FinishWorkRequest(ctx, "req-1", "sess-attempt", "failed", result, finishedAt); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	resp, err := http.Get(srv.URL + "/api/requests/req-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	var got workRequestRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-1" || got.SessionID != "sess-attempt" || got.Status != "failed" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if string(got.Result) != string(result) {
		t.Fatalf("expected the stored result back, got %s", got.Result)
	}
	if !got.ReceivedAt.Equal(claimedAt) || got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("unexpected timestamps: received %v finished %v", got.ReceivedAt, got.FinishedAt)
	}
	if got.Version != 3 {
		t.Fatalf("version = %d, want 3 (claim, attach, finish)", got.Version)
	}
}

func TestGetWorkRequestRowNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/requests/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
}

// TestPatchWorkRequestClosesDeadRequest pins the success path: a running
// request whose session has been quiet past the idle threshold — or that has
// no session at all — can be closed into a terminal status, which sets
// finished_at and bumps the version, and the close is visible to a fresh GET.
func TestPatchWorkRequestClosesDeadRequest(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()

	// A request whose worker died mid-run: last event two hours ago.
	mustCreateWorkRequest(t, st, h, dbPath, "req-abandoned", "sess-abandoned", time.Now().Add(-2*time.Hour))
	// A request that never ran: claimed, no session ever attached.
	if _, err := st.ClaimWorkRequest(ctx, "req-never-ran", 1, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, tc := range []struct {
		name      string
		requestID string
	}{
		{"with idle session", "req-abandoned"},
		{"with no session", "req-never-ran"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := requestVersion(t, srv, tc.requestID)
			resp := doWrite(t, srv, http.MethodPatch, "/api/requests/"+tc.requestID, `{"status":"cancelled"}`, map[string]string{
				"Content-Type": "application/json",
				"If-Match":     strconv.Itoa(before),
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PATCH: got status %d, want 200", resp.StatusCode)
			}
			var got workRequestRow
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Status != "cancelled" || got.FinishedAt == nil {
				t.Fatalf("expected cancelled with finished_at, got %+v", got)
			}
			if got.Version != before+1 {
				t.Fatalf("version = %d, want %d (one mutation)", got.Version, before+1)
			}

			row, err := st.GetWorkRequest(ctx, tc.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != "cancelled" || row.FinishedAt == nil {
				t.Fatalf("store row not closed: %+v", row)
			}
		})
	}
}

// TestPatchWorkRequestRefusesLiveRequest pins the precondition that matters
// (docs/DATA-API.md phase 3): a running request whose session's most recent
// event is newer than the idle threshold is being run by a live pool worker
// right now, and the close is a 409 whose message names when the session was
// last heard from. The row is untouched.
func TestPatchWorkRequestRefusesLiveRequest(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()
	mustCreateWorkRequest(t, st, h, dbPath, "req-live", "sess-live", time.Now())

	before := requestVersion(t, srv, "req-live")
	resp := doWrite(t, srv, http.MethodPatch, "/api/requests/req-live", `{"status":"cancelled"}`, map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(before),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("PATCH on a live request: got status %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "most recent event at") {
		t.Fatalf("409 must say when the session was last heard from, got %q", body)
	}

	row, err := st.GetWorkRequest(ctx, "req-live")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.WorkRequestStatusRunning || row.FinishedAt != nil || row.Version != before {
		t.Fatalf("a refused close must not touch the row, got %+v", row)
	}
}

// TestPatchWorkRequestRecloseKeepsTerminalStatus pins the idempotent re-close:
// a request already terminal keeps the status it finished with — a retried
// PATCH is a version bump and nothing else.
func TestPatchWorkRequestRecloseKeepsTerminalStatus(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	if matched, err := st.FinishWorkRequest(ctx, "req-1", "sess-1", "ok", json.RawMessage(`{"status":"ok"}`), time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	before := requestVersion(t, srv, "req-1")
	resp := doWrite(t, srv, http.MethodPatch, "/api/requests/req-1", `{"status":"cancelled"}`, map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(before),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: got status %d, want 200", resp.StatusCode)
	}
	var got workRequestRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" {
		t.Fatalf("a re-close must keep the status the request finished with, got %q", got.Status)
	}
	if got.Version != before+1 {
		t.Fatalf("version = %d, want %d", got.Version, before+1)
	}
}

// TestRequestWritesGuardsAndPreconditions is the table the task pins for both
// work-request write endpoints: the guards every write carries (415 without a
// JSON content type, 403 cross-origin), the write preconditions (428 without
// If-Match, 400 on a malformed or non-terminal body), and the 412/428
// concurrency paths.
func TestRequestWritesGuardsAndPreconditions(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()
	mustCreateWorkRequest(t, st, h, dbPath, "req-1", "sess-1", time.Now().Add(-2*time.Hour))

	for _, tc := range []struct {
		name    string
		method  string
		body    string
		headers map[string]string
		want    int
	}{
		{
			name: "PATCH missing content type", method: http.MethodPatch,
			body: `{"status":"cancelled"}`, headers: map[string]string{"If-Match": "1"},
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "PATCH wrong content type", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "text/plain", "If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "PATCH cross-origin", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name: "PATCH missing If-Match", method: http.MethodPatch,
			body: `{"status":"cancelled"}`, headers: map[string]string{"Content-Type": "application/json"},
			want: http.StatusPreconditionRequired,
		},
		{
			name: "PATCH malformed If-Match", method: http.MethodPatch,
			body:    `{"status":"cancelled"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "abc"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH non-terminal status", method: http.MethodPatch,
			body:    `{"status":"running"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH unknown status", method: http.MethodPatch,
			body:    `{"status":"dancing"}`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "PATCH invalid JSON", method: http.MethodPatch,
			body:    `not json`,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1"},
			want:    http.StatusBadRequest,
		},
		{
			name: "DELETE missing content type", method: http.MethodDelete,
			headers: map[string]string{"If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "DELETE cross-origin", method: http.MethodDelete,
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name: "DELETE missing If-Match", method: http.MethodDelete,
			headers: map[string]string{"Content-Type": "application/json"},
			want:    http.StatusPreconditionRequired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, tc.method, "/api/requests/req-1", tc.body, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("error Content-Type = %q, want JSON", ct)
			}
		})
	}

	// The rejected writes left the row alone: still running, and the version
	// is whatever the guarded writes found it at (the row was claimed and
	// attached before this test's writes began).
	row, err := st.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.WorkRequestStatusRunning || row.Version != 2 {
		t.Fatalf("guarded writes must not touch the row, got %+v", row)
	}
}

// TestRequestWritesRejectStaleVersion pins the optimistic-concurrency
// mechanism end to end: a write carrying a version older than the row's
// current one is a 412 that names the current version, for PATCH and DELETE
// alike.
func TestRequestWritesRejectStaleVersion(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()
	mustCreateWorkRequest(t, st, h, dbPath, "req-1", "sess-1", time.Now().Add(-2*time.Hour))

	// The worker's own terminal write bumps the row to a later version — the
	// exact race the mechanism exists for: an operator who read the request
	// before it finished must not land a write on the changed row.
	if _, err := st.ClaimWorkRequest(ctx, "req-1", 2, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	current := requestVersion(t, srv, "req-1")

	for _, tc := range []struct {
		name   string
		method string
		body   string
	}{
		{"PATCH", http.MethodPatch, `{"status":"cancelled"}`},
		{"DELETE", http.MethodDelete, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, tc.method, "/api/requests/req-1", tc.body, map[string]string{
				"Content-Type": "application/json",
				"If-Match":     "1", // stale: the row is at the re-claimed version
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("stale %s: got status %d, want 412", tc.method, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), fmt.Sprintf("current version %d", current)) {
				t.Fatalf("412 must name the current version, got %q", body)
			}
		})
	}

	row, err := st.GetWorkRequest(ctx, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.WorkRequestStatusRunning || row.Version != current {
		t.Fatalf("stale writes must not touch the row, got %+v", row)
	}
}

// TestDeleteWorkRequestRemovesRow pins DELETE's success path: a dead request —
// terminal, running with an idle session, or never run at all — can be
// deleted with its current version, and the row is gone afterwards.
func TestDeleteWorkRequestRemovesRow(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()

	// Terminal: finished normally.
	mustCreateWorkRequest(t, st, h, dbPath, "req-terminal", "sess-terminal", time.Now().Add(-2*time.Hour))
	if matched, err := st.FinishWorkRequest(ctx, "req-terminal", "sess-terminal", "ok", json.RawMessage(`{"status":"ok"}`), time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}
	// Dead: running with an idle session.
	mustCreateWorkRequest(t, st, h, dbPath, "req-dead", "sess-dead", time.Now().Add(-2*time.Hour))
	// Never ran: running with no session.
	if _, err := st.ClaimWorkRequest(ctx, "req-never-ran", 1, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, tc := range []struct {
		name      string
		requestID string
	}{
		{"terminal", "req-terminal"},
		{"running with idle session", "req-dead"},
		{"running with no session", "req-never-ran"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := requestVersion(t, srv, tc.requestID)
			resp := doWrite(t, srv, http.MethodDelete, "/api/requests/"+tc.requestID, "", map[string]string{
				"Content-Type": "application/json",
				"If-Match":     strconv.Itoa(v),
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("DELETE: got status %d, want 200", resp.StatusCode)
			}
			var okBody map[string]bool
			if err := json.NewDecoder(resp.Body).Decode(&okBody); err != nil {
				t.Fatal(err)
			}
			if !okBody["ok"] {
				t.Fatalf("DELETE response = %+v, want ok:true", okBody)
			}
			if _, err := st.GetWorkRequest(ctx, tc.requestID); err != store.ErrNotFound {
				t.Fatalf("row should be gone, got %v", err)
			}
		})
	}
}

// TestDeleteWorkRequestRefusesLive pins the delete precondition: a request
// whose session is still live is being run by a live worker right now, and
// deleting its row would swallow the worker's result — the delete is a 409
// naming the last event's time, and the row survives.
func TestDeleteWorkRequestRefusesLive(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()
	mustCreateWorkRequest(t, st, h, dbPath, "req-live", "sess-live", time.Now())

	v := requestVersion(t, srv, "req-live")
	resp := doWrite(t, srv, http.MethodDelete, "/api/requests/req-live", "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(v),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE on a live request: got status %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "most recent event at") {
		t.Fatalf("409 must say when the session was last heard from, got %q", body)
	}
	if _, err := st.GetWorkRequest(ctx, "req-live"); err != nil {
		t.Fatalf("a refused delete must leave the row, got %v", err)
	}
}

// TestRequestWritesNotFound pins 404 for both write endpoints on an unknown
// request id.
func TestRequestWritesNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		method string
		body   string
	}{
		{http.MethodPatch, `{"status":"cancelled"}`},
		{http.MethodDelete, ""},
	} {
		resp := doWrite(t, srv, tc.method, "/api/requests/does-not-exist", tc.body, map[string]string{
			"Content-Type": "application/json",
			"If-Match":     "1",
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown request: got status %d, want 404", tc.method, resp.StatusCode)
		}
	}
}

// TestGetLeasesListsTable pins GET /api/leases: every lease row, in
// workspace order, with session id, timestamps, and version.
func TestGetLeasesListsTable(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	if err := st.AcquireWorkspaceLease(ctx, "/tmp/ws-b", "sess-2"); err != nil {
		t.Fatalf("acquire ws-b: %v", err)
	}
	if err := st.AcquireWorkspaceLease(ctx, "/tmp/ws-a", "sess-1"); err != nil {
		t.Fatalf("acquire ws-a: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	var got []workspaceLeaseRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 leases, got %d: %+v", len(got), got)
	}
	if got[0].Workspace != "/tmp/ws-a" || got[0].SessionID != "sess-1" {
		t.Fatalf("expected ws-a first with sess-1, got %+v", got[0])
	}
	if got[1].Workspace != "/tmp/ws-b" || got[1].SessionID != "sess-2" {
		t.Fatalf("expected ws-b second with sess-2, got %+v", got[1])
	}
	for _, l := range got {
		if l.Version != 1 {
			t.Fatalf("a freshly acquired lease must be at version 1, got %+v", l)
		}
		if l.AcquiredAt.IsZero() || l.HeartbeatAt.IsZero() {
			t.Fatalf("expected timestamps set, got %+v", l)
		}
	}
}

// backdateLeaseHeartbeat rewrites one lease's heartbeat_at to at, so the
// lease is genuinely quiet past the idle threshold. It opens a second
// connection to the same SQLite file, the same trick backdateEvents uses.
func backdateLeaseHeartbeat(t *testing.T, dbPath, workspace string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}
	if _, err := db.Exec(`UPDATE workspace_leases SET heartbeat_at = ? WHERE workspace = ?`, at.UTC().Format(time.RFC3339Nano), workspace); err != nil {
		t.Fatalf("backdate lease heartbeat: %v", err)
	}
}

// leaseVersion fetches GET /api/leases and returns one workspace's lease
// version, standing in for the client reading the table before a write.
func leaseVersion(t *testing.T, srv *httptest.Server, workspace string) int {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/leases: got status %d, want 200", resp.StatusCode)
	}
	var got []workspaceLeaseRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, l := range got {
		if l.Workspace == workspace {
			if l.Version < 1 {
				t.Fatalf("lease %s: expected a positive version, got %d", workspace, l.Version)
			}
			return l.Version
		}
	}
	t.Fatalf("no lease for %s in %+v", workspace, got)
	return 0
}

// TestDeleteLeaseReleasesStranded pins the success path: a lease whose
// heartbeat is older than the idle threshold is stranded — its session is
// gone or dead — and the release removes the row.
func TestDeleteLeaseReleasesStranded(t *testing.T) {
	srv, st, _, dbPath := newTestServerWithDB(t)
	ctx := context.Background()

	if err := st.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	backdateLeaseHeartbeat(t, dbPath, "/tmp/ws", time.Now().Add(-2*time.Hour))

	v := leaseVersion(t, srv, "/tmp/ws")
	resp := doWrite(t, srv, http.MethodDelete, "/api/leases/"+url.PathEscape("/tmp/ws"), "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(v),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: got status %d, want 200", resp.StatusCode)
	}
	var okBody map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&okBody); err != nil {
		t.Fatal(err)
	}
	if !okBody["ok"] {
		t.Fatalf("DELETE response = %+v, want ok:true", okBody)
	}
	leases, err := st.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("expected the lease gone, got %+v", leases)
	}
}

// TestDeleteLeaseRefusesLive pins the precondition that matters: a lease
// whose heartbeat is newer than the idle threshold is held by a live session
// right now, and the release is a 409 whose message names the last heartbeat.
// The row is untouched.
func TestDeleteLeaseRefusesLive(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	if err := st.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	v := leaseVersion(t, srv, "/tmp/ws")
	resp := doWrite(t, srv, http.MethodDelete, "/api/leases/"+url.PathEscape("/tmp/ws"), "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     strconv.Itoa(v),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE on a live lease: got status %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "last heartbeat at") {
		t.Fatalf("409 must name the last heartbeat, got %q", body)
	}
	leases, err := st.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Version != v {
		t.Fatalf("a refused release must not touch the row, got %+v", leases)
	}
}

// TestLeaseWritesGuardsAndPreconditions is the table for the lease release
// endpoint: the guards every write carries (415 without a JSON content type,
// 403 cross-origin), the If-Match precondition (428 missing, 412 stale), and
// the 404 on an unknown workspace.
func TestLeaseWritesGuardsAndPreconditions(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if err := st.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "missing content type",
			headers: map[string]string{"If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name:    "wrong content type",
			headers: map[string]string{"Content-Type": "text/plain", "If-Match": "1"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name:    "cross-origin",
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "1", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name:    "missing If-Match",
			headers: map[string]string{"Content-Type": "application/json"},
			want:    http.StatusPreconditionRequired,
		},
		{
			name:    "malformed If-Match",
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "abc"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "stale If-Match",
			headers: map[string]string{"Content-Type": "application/json", "If-Match": "99"},
			want:    http.StatusPreconditionFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, http.MethodDelete, "/api/leases/"+url.PathEscape("/tmp/ws"), "", tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("error Content-Type = %q, want JSON", ct)
			}
		})
	}

	// The rejected writes left the lease alone.
	leases, err := st.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Version != 1 {
		t.Fatalf("guarded writes must not touch the row, got %+v", leases)
	}

	// An unknown workspace is a 404.
	resp := doWrite(t, srv, http.MethodDelete, "/api/leases/"+url.PathEscape("/tmp/unknown"), "", map[string]string{
		"Content-Type": "application/json",
		"If-Match":     "1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE unknown lease: got status %d, want 404", resp.StatusCode)
	}
}
