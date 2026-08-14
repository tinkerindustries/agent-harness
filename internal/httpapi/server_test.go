package httpapi

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	_ "modernc.org/sqlite"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/redact"
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

	// A stop path allows POST (the run-control write); every other method
	// still 405s there, naming the three allowed methods in Allow. The gate
	// must let POST through even when the session id is unknown — the
	// handler, not the gate, owns the 404 (docs/RUN-CONTROL.md).
	for _, stopPath := range []string{"/api/sessions/sess-1/stop", "/api/sessions/does-not-exist/stop"} {
		resp := doWrite(t, srv, http.MethodPost, stopPath, `{}`, map[string]string{"Content-Type": "application/json"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("POST %s: gate refused a permitted write", stopPath)
		}
		for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			req, err := http.NewRequest(m, srv.URL+stopPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, stopPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, stopPath, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
				t.Errorf("%s %s: Allow = %q, want %q", m, stopPath, got, "GET, HEAD, POST")
			}
		}
	}

	// A steer path allows POST the same way (docs/RUN-CONTROL.md);
	// every other method still 405s there with the same Allow header, and the
	// gate must not widen isSessionPath to cover it — POST /api/sessions/{id}
	// itself stays a 405 (asserted in the session-path block above).
	for _, steerPath := range []string{"/api/sessions/sess-1/steer", "/api/sessions/does-not-exist/steer"} {
		resp := doWrite(t, srv, http.MethodPost, steerPath, `{}`, map[string]string{"Content-Type": "application/json"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("POST %s: gate refused a permitted write", steerPath)
		}
		for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			req, err := http.NewRequest(m, srv.URL+steerPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, steerPath, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got status %d, want 405", m, steerPath, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
				t.Errorf("%s %s: Allow = %q, want %q", m, steerPath, got, "GET, HEAD, POST")
			}
		}
	}

	// The runs collection allows POST (docs/RUN-CONTROL.md "POST
	// /api/runs"); every other method still 405s there with the same Allow
	// header, and a deeper path (/api/runs/<something>) has no write route at
	// all — starting a run is exactly one action on exactly one path.
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", `{}`, map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Errorf("POST /api/runs: gate refused a permitted write")
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		req, err := http.NewRequest(m, srv.URL+"/api/runs", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /api/runs: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/runs: got status %d, want 405", m, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
			t.Errorf("%s /api/runs: Allow = %q, want %q", m, got, "GET, HEAD, POST")
		}
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/runs/extra", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/runs/extra: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/runs/extra: got status %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("POST /api/runs/extra: Allow = %q, want %q", got, "GET, HEAD")
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

// fetchSessionPage GETs /api/sessions with qs and decodes the envelope —
// every response from the collection is a Page now, never a bare array
// (docs/DATA-API.md "Pagination").
func fetchSessionPage(t *testing.T, srv *httptest.Server, qs string) Page[hub.SessionState] {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/sessions" + qs)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions%s: got status %d, want 200", qs, resp.StatusCode)
	}
	var page Page[hub.SessionState]
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestListSessionsNewestFirst(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	mustCreateSession(t, st, "older", now.Add(-time.Hour))
	mustCreateSession(t, st, "newer", now)

	page := fetchSessionPage(t, srv, "")
	if page.Total != 2 {
		t.Fatalf("expected total 2, got %d", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	if page.Items[0].ID != "newer" || page.Items[1].ID != "older" {
		t.Fatalf("expected newest first, got %s then %s", page.Items[0].ID, page.Items[1].ID)
	}
	if page.Limit != defaultPageLimit || page.Offset != 0 {
		t.Fatalf("expected default limit %d offset 0, got limit %d offset %d", defaultPageLimit, page.Limit, page.Offset)
	}
	if page.HasMore || page.Next != nil {
		t.Fatalf("expected no more on 2 rows, got has_more %v next %v", page.HasMore, page.Next)
	}
}

func TestListSessionsDefaultPage(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		mustCreateSession(t, st, "sess-"+strconv.Itoa(i), now.Add(-time.Duration(i)*time.Minute))
	}

	page := fetchSessionPage(t, srv, "")
	if len(page.Items) != defaultPageLimit {
		t.Fatalf("expected %d items on the default page, got %d", defaultPageLimit, len(page.Items))
	}
	if page.Total != 25 {
		t.Fatalf("expected total 25, got %d", page.Total)
	}
	if !page.HasMore {
		t.Fatal("expected has_more on the default page of 25 rows")
	}
	if page.Next == nil || *page.Next != defaultPageLimit {
		t.Fatalf("expected next %d, got %v", defaultPageLimit, page.Next)
	}

	// The second page holds the remaining 5 rows and reports no more.
	page = fetchSessionPage(t, srv, "?offset=20")
	if len(page.Items) != 5 || page.Total != 25 {
		t.Fatalf("expected 5 of 25 on the second page, got %d of %d", len(page.Items), page.Total)
	}
	if page.HasMore || page.Next != nil {
		t.Fatalf("expected no more on the last page, got has_more %v next %v", page.HasMore, page.Next)
	}
}

func TestListSessionsExplicitLimitOffset(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		mustCreateSession(t, st, "sess-"+strconv.Itoa(i), now.Add(-time.Duration(i)*time.Minute))
	}

	page := fetchSessionPage(t, srv, "?limit=3&offset=6")
	if page.Limit != 3 || page.Offset != 6 {
		t.Fatalf("expected limit 3 offset 6 echoed back, got limit %d offset %d", page.Limit, page.Offset)
	}
	if len(page.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(page.Items))
	}
	if page.Items[0].ID != "sess-6" || page.Items[2].ID != "sess-8" {
		t.Fatalf("expected the window to start at sess-6, got %s first", page.Items[0].ID)
	}
	if page.Total != 10 {
		t.Fatalf("expected total 10, got %d", page.Total)
	}

	// A limit over the max clamps to the default, never a 400 — the events
	// endpoint's forgiving shape.
	page = fetchSessionPage(t, srv, "?limit=99999")
	if page.Limit != defaultPageLimit {
		t.Fatalf("expected over-max limit to clamp to %d, got %d", defaultPageLimit, page.Limit)
	}
}

func TestListSessionsStatusSplit(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	mustCreateSession(t, st, "run-1", now)
	mustCreateSession(t, st, "done-1", now.Add(-time.Minute))
	mustCreateSession(t, st, "done-2", now.Add(-2*time.Minute))
	finishTestSession(t, st, "done-1")
	finishTestSession(t, st, "done-2")

	page := fetchSessionPage(t, srv, "?status=running")
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "run-1" {
		t.Fatalf("expected only run-1 under status=running, got %+v", page)
	}

	page = fetchSessionPage(t, srv, "?status=finished")
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("expected 2 finished rows, got total %d len %d", page.Total, len(page.Items))
	}
	for _, s := range page.Items {
		if s.Status == store.StatusRunning {
			t.Fatalf("expected no running rows under status=finished, got %q", s.Status)
		}
	}

	// No status: everything, still newest first.
	page = fetchSessionPage(t, srv, "")
	if page.Total != 3 {
		t.Fatalf("expected 3 rows with no status filter, got %d", page.Total)
	}
}

func TestListSessionsQueryMatches(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	mustCreateSession(t, st, "sess-1", now)
	mustCreateSession(t, st, "sess-2", now.Add(-time.Minute))
	mustCreateSession(t, st, "sess-3", now.Add(-2*time.Minute))
	if _, err := st.ClaimWorkRequest(context.Background(), "req-special-9", 1, time.Now()); err != nil {
		t.Fatalf("claim work request: %v", err)
	}
	if err := st.SetWorkRequestSession(context.Background(), "req-special-9", "sess-3"); err != nil {
		t.Fatalf("set work request session: %v", err)
	}

	// Workspace match ("/tmp/sess-1") — the query matches the workspace, not
	// the id, here.
	page := fetchSessionPage(t, srv, "?q=%2Ftmp%2Fsess-1")
	if page.Total != 1 || page.Items[0].ID != "sess-1" {
		t.Fatalf("expected only sess-1 for the workspace query, got %+v", page)
	}

	// Work-request match, found only through the join.
	page = fetchSessionPage(t, srv, "?q=req-special-9")
	if page.Total != 1 || page.Items[0].ID != "sess-3" {
		t.Fatalf("expected only sess-3 for the request-id query, got %+v", page)
	}

	// A query matching nothing is an empty page, not an error.
	page = fetchSessionPage(t, srv, "?q=no-such-row")
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("expected an empty page for a no-match query, got %+v", page)
	}
}

func TestListSessionsBadStatusIs400(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/sessions?status=bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"running", "finished"} {
		if !strings.Contains(body["error"], want) {
			t.Fatalf("expected the 400 to name %q, got %q", want, body["error"])
		}
	}
}

func TestListSessionsTotalCountsFilterNotPage(t *testing.T) {
	srv, st, _ := newTestServer(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		mustCreateSession(t, st, "run-"+strconv.Itoa(i), now.Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		mustCreateSession(t, st, "done-"+strconv.Itoa(i), now.Add(-time.Duration(10+i)*time.Minute))
		finishTestSession(t, st, "done-"+strconv.Itoa(i))
	}

	page := fetchSessionPage(t, srv, "?status=finished&limit=2")
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 rows on the page, got %d", len(page.Items))
	}
	if page.Total != 3 {
		t.Fatalf("expected total 3 (the filter's count, not the page's), got %d", page.Total)
	}
	if !page.HasMore || page.Next == nil || *page.Next != 2 {
		t.Fatalf("expected has_more with next 2, got has_more %v next %v", page.HasMore, page.Next)
	}
}

// finishTestSession closes a session into a terminal status, the minimal
// stand-in for a run finishing.
func finishTestSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	finished := time.Now().UTC()
	if err := st.UpdateSessionStatus(context.Background(), id, store.StatusOK, &finished); err != nil {
		t.Fatalf("finish session %s: %v", id, err)
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
		{Kind: store.KindToolCall, Payload: store.ToolCallPayload{ID: "call-create", Name: "TaskCreate",
			Arguments: `{"tasks":[{"subject":"fix it","description":"fix the thing","status":"in_progress","activeForm":"Fixing it"}]}`}},
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{ToolCallID: "call-create", Name: "TaskCreate", Content: "[~] #1 fix it"}},
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
	if len(got.Todos) != 1 || got.Todos[0].Subject != "fix it" || got.ActiveForm != "Fixing it" {
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

// A session's tool output is whatever its commands printed, so a run that
// needs a token in its container can put one straight into the log, as
// `head -2 .env` did (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md).
// The port must not serve it back, over either transport.
func TestEventsRedactCredentialsOnBothTransports(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	token := "ghp_" + strings.Repeat("a", 36)
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{{
		Kind:    store.KindToolResult,
		Payload: store.ToolResultPayload{ToolCallID: "call_1", Name: "Bash", Content: "GITHUB_TOKEN=" + token},
	}})

	page := fetchEventsPage(t, srv, "/api/sessions/sess-1/events")
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), token) {
		t.Fatalf("the events endpoint served the token: %s", body)
	}
	if !strings.Contains(string(body), "[redacted]") {
		t.Fatalf("expected the placeholder in the page: %s", body)
	}
	// The rest of the payload must survive: redaction that swallowed the
	// surrounding output would make the transcript useless.
	if !strings.Contains(string(body), "GITHUB_TOKEN=") {
		t.Fatalf("redaction ate the surrounding output: %s", body)
	}

	resp, err := http.Get(srv.URL + "/api/sessions/sess-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), token) {
		t.Fatalf("the stream served the token: %s", buf[:n])
	}
}

// A live delta must reach the stream as a *named* event with no id. The
// name keeps it off the browser's onmessage handler, which folds committed
// events and would double the text when the real reasoning_delta arrives;
// the missing id keeps it out of Last-Event-ID, which must only ever name a
// committed seq or a reconnect would skip whatever came in between.
func TestSessionStreamWritesLiveDeltasAsNamedEventsWithoutIDs(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp, err := http.Get(srv.URL + "/api/sessions/sess-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler time to subscribe before publishing, or the delta
	// races the subscription and the read below blocks.
	time.Sleep(100 * time.Millisecond)
	h.PublishLive("sess-1", hub.LiveDelta{SubTurn: 2, Channel: hub.ChannelReasoning, Text: "weighing it"})

	sr := newSSEReader(resp.Body)
	frame, err := sr.next()
	if err != nil {
		t.Fatal(err)
	}
	if frame.event != "live" {
		t.Fatalf("live delta must be a named SSE event, got event %q", frame.event)
	}
	if frame.id != "" {
		t.Fatalf("live delta must carry no id, got: %q", frame.id)
	}
	for _, want := range []string{`"sub_turn":2`, `"channel":"reasoning"`, `"text":"weighing it"`} {
		if !strings.Contains(frame.data, want) {
			t.Fatalf("live frame missing %s, got: %q", want, frame.data)
		}
	}
}

// The `replayed` marker names the seam between a session's history and its
// live tail. The browser cannot work that seam out for itself — a long replay
// arrives across several reads, so "the first events I saw" is a fraction of
// the backlog — and the frontend animates only what arrived after it
// (web/src/hooks.ts useArrivals). It is shaped like a live delta for the same
// two reasons: named, so it never reaches onmessage and cannot be mistaken
// for a committed event, and without an id, so it cannot move Last-Event-ID
// and make a reconnect skip whatever came after it.
func TestSessionStreamMarksTheEndOfTheHistoryReplay(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "go"}},
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
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

	// Every historical event first, then the marker: a frame that arrived
	// before it is backlog, and the ordering is the whole contract.
	sr := newSSEReader(resp.Body)
	var ids []string
	for {
		frame, err := sr.nextFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if frame.event == "replayed" {
			if frame.id != "" {
				t.Fatalf("the marker must carry no id, got %q", frame.id)
			}
			break
		}
		ids = append(ids, frame.id)
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("expected the whole history before the marker, got %v", ids)
	}

	// And an event committed afterwards still arrives, on the same
	// connection: the marker separates the two halves, it does not end the
	// stream.
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "hi"}},
	})
	frame, err := sr.next()
	if err != nil {
		t.Fatalf("read live frame: %v", err)
	}
	if frame.id != "3" {
		t.Fatalf("expected seq 3 after the marker, got id %q", frame.id)
	}
}

// Live deltas are the same model output as the committed events, arriving
// earlier, so they get the same masking. A secret redacted in the log but
// streamed in the clear here would be no redaction at all.
func TestSessionStreamRedactsLiveDeltas(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp, err := http.Get(srv.URL + "/api/sessions/sess-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	token := "ghp_" + strings.Repeat("q", 36)
	h.PublishLive("sess-1", hub.LiveDelta{SubTurn: 1, Channel: hub.ChannelContent, Text: "the token is " + token})

	sr := newSSEReader(resp.Body)
	frame, err := sr.next()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frame.data, token) {
		t.Fatalf("the live stream served the token: %q", frame.data)
	}
	if !strings.Contains(frame.data, "[redacted]") {
		t.Fatalf("expected the placeholder in the live frame: %q", frame.data)
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

// fetchEventsPage GETs the events endpoint and decodes the page, failing the
// test on any transport or status error.
func fetchEventsPage(t *testing.T, srv *httptest.Server, qs string) eventsPage {
	t.Helper()
	resp, err := http.Get(srv.URL + qs)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got status %d, want 200", qs, resp.StatusCode)
	}
	var page eventsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

// TestEventsPagingArrivesAtTheEndExactlyOnce drives a log longer than the
// page size purely on the paging metadata — keep asking with the next cursor
// while has_more is true — and asserts it arrives at the end exactly once:
// the final page is the first one whose has_more is false and it holds
// exactly the remaining events, with no empty trailing page and no off-by-one.
// The second scenario is the edge the old "a full page means more" heuristic
// answered wrong: a log that ends exactly on a page boundary has no more even
// though its last page is full.
func TestEventsPagingArrivesAtTheEndExactlyOnce(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	mustCreateSession(t, st, "sess-exact", time.Now())
	for i := 0; i < 7; i++ {
		appendAndPublish(t, st, h, "sess-1", []store.EventInput{
			{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: strconv.Itoa(i)}},
		})
	}
	for i := 0; i < 6; i++ {
		appendAndPublish(t, st, h, "sess-exact", []store.EventInput{
			{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: strconv.Itoa(i)}},
		})
	}

	// page follows the cursor from from=1 while has_more is true and
	// returns the seqs seen and how many pages the log took.
	page := func(id string) (seqs []int64, pages int) {
		t.Helper()
		from := int64(1)
		for {
			p := fetchEventsPage(t, srv, fmt.Sprintf("/api/sessions/%s/events?from=%d&limit=3", id, from))
			for _, ev := range p.Events {
				seqs = append(seqs, ev.Seq)
			}
			pages++
			if !p.HasMore {
				if p.Next != nil {
					t.Fatalf("%s: a page with has_more=false must not carry a next cursor, got %d", id, *p.Next)
				}
				return seqs, pages
			}
			if p.Next == nil {
				t.Fatalf("%s: a page with has_more=true must carry a next cursor", id)
			}
			from = *p.Next
		}
	}

	seqs, pages := page("sess-1")
	if pages != 3 || !slices.Equal(seqs, []int64{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("sess-1: %d pages, seqs %v; want 3 pages covering 1..7", pages, seqs)
	}

	seqs, pages = page("sess-exact")
	if pages != 2 || !slices.Equal(seqs, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("sess-exact: %d pages, seqs %v; want 2 pages covering 1..6 with the full second page reporting no more", pages, seqs)
	}
}

// TestEventsKindFilterPagingReportsMoreMatchingEvents pins the composition
// of ?kind= with paging (docs/DATA-API.md "events"): has_more on a filtered
// page means more *matching* events, not more events of any kind. A log
// whose tool traffic is interleaved with content deltas is paged on the
// tool_call kind, and a kind that matches once reports no more even while
// the unfiltered log continues past it.
func TestEventsKindFilterPagingReportsMoreMatchingEvents(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	var inputs []store.EventInput
	for i := 0; i < 4; i++ {
		inputs = append(inputs,
			store.EventInput{Kind: store.KindToolCall, Payload: store.ToolCallPayload{ID: "c" + strconv.Itoa(i), Name: "Read", Arguments: `{}`}},
			store.EventInput{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "noise"}},
		)
	}
	inputs = append(inputs, store.EventInput{Kind: store.KindUsage, Payload: store.UsagePayload{SubTurn: 1, CostUSD: 0.001}})
	appendAndPublish(t, st, h, "sess-1", inputs)
	// seqs: 1 tool_call, 2 content_delta, 3 tool_call, 4 content_delta,
	//       5 tool_call, 6 content_delta, 7 tool_call, 8 content_delta,
	//       9 usage. Four tool_call matches, interleaved with non-matches.

	// Page the tool_call kind with limit 2: exactly two pages, each full,
	// the first reporting more and the second — the exact remainder — not.
	from := int64(1)
	var pages []eventsPage
	for i := 0; i < 3; i++ {
		p := fetchEventsPage(t, srv, fmt.Sprintf("/api/sessions/sess-1/events?from=%d&limit=2&kind=tool_call", from))
		pages = append(pages, p)
		if !p.HasMore {
			break
		}
		from = *p.Next
	}
	if len(pages) != 2 {
		t.Fatalf("expected 2 pages of tool_call, got %d", len(pages))
	}
	if !pages[0].HasMore || pages[0].Next == nil {
		t.Fatalf("first filtered page must report more (4 matches, limit 2), got %+v", pages[0])
	}
	if pages[1].HasMore || pages[1].Next != nil {
		t.Fatalf("second filtered page must be the last, got %+v", pages[1])
	}
	if len(pages[1].Events) != 2 {
		t.Fatalf("last filtered page = %d events, want the exact remainder of 2", len(pages[1].Events))
	}
	for _, p := range pages {
		for _, ev := range p.Events {
			if ev.Kind != store.KindToolCall {
				t.Fatalf("kind filter leaked a %s event into the page", ev.Kind)
			}
		}
	}

	// The cursor composes with the filter: a from in the middle of
	// non-matching events resumes at the next match.
	resumed := fetchEventsPage(t, srv, "/api/sessions/sess-1/events?from=6&limit=2&kind=tool_call")
	if len(resumed.Events) != 1 || resumed.Events[0].Seq != 7 || resumed.HasMore {
		t.Fatalf("resumed filtered page = %+v, want just the tool_call at seq 7 with no more", resumed)
	}

	// A kind that matches once reports has_more=false immediately, while the
	// unfiltered log keeps going past it — "more" is about matching events.
	usage := fetchEventsPage(t, srv, "/api/sessions/sess-1/events?from=1&limit=2&kind=usage")
	if len(usage.Events) != 1 || usage.HasMore || usage.Next != nil {
		t.Fatalf("usage filter = %+v, want exactly its one event with no more", usage)
	}
}

// TestEventsUnknownKindReturns400NamingValidKinds pins the filter's
// validation (docs/DATA-API.md "events"): an unknown kind name is a 400 whose
// message names every valid kind, and the valid list comes from
// store.EventKinds — the same list the filter accepts — never a literal
// here. A single bad name poisons the whole list, so a typo in one of many
// kinds is caught rather than silently narrowing the page.
func TestEventsUnknownKindReturns400NamingValidKinds(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, qs := range []string{
		"?kind=bogus",
		"?kind=tool_call,bogus",
		"?kind=ToolCall",     // names are case-sensitive
		"?kind=run-finished", // the kind is run_finished
	} {
		resp, err := http.Get(srv.URL + "/api/sessions/sess-1/events" + qs)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s: got status %d, want 400", qs, resp.StatusCode)
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		msg := got["error"]
		for _, k := range store.EventKinds {
			if !strings.Contains(msg, string(k)) {
				t.Fatalf("GET %s: 400 message %q does not name valid kind %q", qs, msg, k)
			}
		}
	}
}

// TestEventsPathHasNoMutatingRouteInRouter pins the events resource's
// immutability against the routing table itself (docs/DATA-API.md "Events
// are not writable"): the router registers GET on an events path and nothing
// else, so a mutating request falls through to the static "/" catch-all
// rather than a method-specific route. ServeMux.Handler reports the most
// specific pattern a request matches, so a mutating events route added later
// answers with its own pattern here and fails the test — it cannot hide
// behind the method gate, which is a separate layer from the table this
// assertion reads.
func TestEventsPathHasNoMutatingRouteInRouter(t *testing.T) {
	api := &Server{
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	mux := api.routes()

	// The events resource is readable: GET matches the registered pattern.
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/sessions/sess-1/events", nil)
	if _, pat := mux.Handler(req); pat != "GET /api/sessions/{id}/events" {
		t.Fatalf("GET on an events path routes to %q, want the events pattern", pat)
	}

	// No mutating method may have a route on an events path: every one of
	// them must fall through to the static catch-all ("/"). A pattern
	// registered later — an edit button's POST, say — would displace the
	// catch-all here and fail the test.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(m, "http://example.com/api/sessions/sess-1/events", nil)
		if _, pat := mux.Handler(req); pat != "/" {
			t.Errorf("%s on an events path routes to %q, want the static fallthrough \"/\": the events resource must stay read-only", m, pat)
		}
	}
}

// TestEventsPathMutatingMethods405WithAllow pins the wire behaviour of the
// same invariant: PUT, PATCH, POST and DELETE on an events path answer 405
// with an Allow header naming what the path really permits — GET and HEAD —
// exactly like every other path that has no write route (docs/DATA-API.md).
func TestEventsPathMutatingMethods405WithAllow(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		resp := doWrite(t, srv, m, "/api/sessions/sess-1/events", "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s on an events path: got status %d, want 405", m, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s on an events path: Allow = %q, want %q", m, got, "GET, HEAD")
		}
	}
}

// --- SSE: session transcript stream ---

// sseReader reads "id: N\ndata: ...\n\n" frames off body, skipping bare
// keep-alive comment lines, matching the shape internal/httpapi writes.
type sseReader struct {
	r *bufio.Reader
}

type sseFrame struct {
	id    string
	event string
	data  string
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReader(body)}
}

// next returns the next frame that carries transcript content, skipping the
// `replayed` marker that separates the history replay from the live tail.
// Tests that assert on positions in the stream are about the events, not the
// seam; nextFrame is what a test asserting on the marker itself uses.
func (s *sseReader) next() (sseFrame, error) {
	for {
		frame, err := s.nextFrame()
		if err != nil {
			return sseFrame{}, err
		}
		if frame.event != "replayed" {
			return frame, nil
		}
	}
}

func (s *sseReader) nextFrame() (sseFrame, error) {
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
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
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

// TestCreatingSessionStreamStaysOpen asserts the live-status widening on the
// transcript stream: a session still cloning has a "creating" row and no
// events, and a browser that opened it must get a stream that stays open
// (the post-replay and keepalive status checks use store.IsLive) rather than
// one that closes instantly as though the run were over.
func TestCreatingSessionStreamStaysOpen(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	if err := st.UpdateSessionStatus(context.Background(), "sess-1", store.StatusCreating, nil); err != nil {
		t.Fatalf("mark creating: %v", err)
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
	// The replay marker is the whole history for a session with no events.
	for {
		frame, err := sr.nextFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if frame.event == "replayed" {
			break
		}
	}

	// The stream must stay open while the row is creating; a terminal check
	// that read "not running" as finished would close it here.
	done := make(chan error, 1)
	go func() {
		_, err := sr.next()
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("expected a creating session's stream to stay open, got %v", err)
	case <-time.After(500 * time.Millisecond):
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
	var snap hub.ListRow
	if err := json.Unmarshal([]byte(frame.data), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ID != "sess-1" || snap.Status != store.StatusRunning {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	// The snapshot is the projection, not the full row — the same shape the
	// updates below arrive in. A snapshot carrying more than the updates do
	// would seed fields that revert the first time a session moved.
	if strings.Contains(frame.data, `"recent_tool_calls"`) || strings.Contains(frame.data, `"permission_mode"`) {
		t.Fatalf("expected the list snapshot projected to hub.ListRow, got %s", frame.data)
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
	var update hub.ListRow
	if err := json.Unmarshal([]byte(frame.data), &update); err != nil {
		t.Fatal(err)
	}
	if update.ID != "sess-2" {
		t.Fatalf("expected update for sess-2, got %+v", update)
	}
}

// TestSessionStreamCarriesStateFrames asserts the other half of the split:
// somebody watching one session is told about every change to its row, in
// full, on the stream they are already connected to. Before this the detail
// screen re-fetched GET /api/sessions/{id} only when its connection flipped,
// so its cost and cache figures sat still for the length of a run.
//
// The frame is named and carries no id, exactly like a live delta: it must
// not reach the client's onmessage fold, and it must not move Last-Event-ID,
// which may only ever name a committed seq.
func TestSessionStreamCarriesStateFrames(t *testing.T) {
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

	sr := newSSEReader(resp.Body)
	// The replay seam first — an empty session has no history before it.
	// nextFrame rather than next, which skips the marker by design.
	frame, err := sr.nextFrame()
	if err != nil {
		t.Fatalf("read replay marker: %v", err)
	}
	if frame.event != "replayed" {
		t.Fatalf("expected the replayed marker first, got %+v", frame)
	}

	sess := mustGetSession(t, st, "sess-1")
	sess.Summary = "wired it up"
	sess.RecentToolCalls = []store.RecentToolCall{{Name: "Bash", Arguments: `{"command":"echo hi"}`}}
	h.PublishSessionState(hub.BuildSessionState(sess, store.SessionUsageSummary{SubTurns: 3}, "req-1", "2026-01-01"))

	frame, err = sr.next()
	if err != nil {
		t.Fatalf("read state frame: %v", err)
	}
	if frame.event != "state" {
		t.Fatalf("expected a named state frame, got %+v", frame)
	}
	if frame.id != "" {
		t.Fatalf("a state frame must carry no id, got %q", frame.id)
	}
	var got hub.SessionState
	if err := json.Unmarshal([]byte(frame.data), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "sess-1" || got.SubTurns != 3 {
		t.Fatalf("unexpected state frame: %+v", got)
	}
	// Everything the list feed drops is here — this is the feed for a caller
	// who asked about this one session.
	if got.Summary != "wired it up" || got.PriceTableDate != "2026-01-01" || len(got.RecentToolCalls) != 1 {
		t.Fatalf("expected the whole row on the session stream, got %+v", got)
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
	if !key.Set || key.Value != redact.Secret("sk-very-secret-1234") || !key.Secret || key.Override != true {
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

	// A setting's bounds: an integer setting's min and max arrive as JSON
	// numbers, a duration's as compact Go duration text ("1s", "24h" — never
	// "24h0m0s"), and a plain string setting carries neither, so its payload
	// does not grow a pair of meaningless zeroes.
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
// refuses a live session — running, or creating while a worker is preparing
// its workspace — with 409 regardless of idleness; an abandoned
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
	if !strings.Contains(string(body), "still live") {
		t.Fatalf("409 must say the session is still live, got %q", body)
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

// --- work requests and workspace leases ---

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

// TestListWorkRequestsListsTable pins GET /api/requests (docs/DATA-API.md):
// every work_requests row, newest first by received_at, each in the same
// shape as GET /api/requests/{request_id} with the version a subsequent
// write must echo back in If-Match — and including the sessionless running
// row a request whose worker died during workspace preparation leaves
// behind, which is exactly the row the operations screen could not find
// while it had to enumerate requests through sessions.
func TestListWorkRequestsListsTable(t *testing.T) {
	srv, st, h, dbPath := newTestServerWithDB(t)
	ctx := context.Background()

	// Oldest: a request that never ran — claimed, no session ever attached.
	if _, err := st.ClaimWorkRequest(ctx, "req-sessionless", 1, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("claim sessionless: %v", err)
	}
	// Middle: a running request whose worker died mid-run (idle session).
	mustCreateWorkRequest(t, st, h, dbPath, "req-dead", "sess-dead", time.Now().UTC().Add(-time.Hour))
	// Newest: a request that ran to completion.
	mustCreateWorkRequest(t, st, h, dbPath, "req-finished", "sess-finished", time.Now().UTC().Add(-30*time.Minute))
	if matched, err := st.FinishWorkRequest(ctx, "req-finished", "sess-finished", "ok", json.RawMessage(`{"status":"ok"}`), time.Now().UTC()); err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	resp, err := http.Get(srv.URL + "/api/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	var got []workRequestRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d: %+v", len(got), got)
	}
	wantOrder := []string{"req-finished", "req-dead", "req-sessionless"}
	for i, want := range wantOrder {
		if got[i].RequestID != want {
			t.Fatalf("row %d = %q, want %q (newest first by received_at)", i, got[i].RequestID, want)
		}
	}
	// The sessionless running request appears — the hole this endpoint fills.
	if got[2].RequestID != "req-sessionless" || got[2].SessionID != "" || got[2].Status != "running" {
		t.Fatalf("expected the sessionless running request in the list, got %+v", got[2])
	}
	if got[2].Version != 1 {
		t.Fatalf("sessionless row version = %d, want 1 (one claim)", got[2].Version)
	}
	// Every row carries the version and received_at a list row must.
	for _, row := range got {
		if row.Version < 1 {
			t.Fatalf("row %s: expected a positive version, got %d", row.RequestID, row.Version)
		}
		if row.ReceivedAt.IsZero() {
			t.Fatalf("row %s: expected received_at set", row.RequestID)
		}
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
// (docs/DATA-API.md): a running request whose session's most recent
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

// --- run control: stop ---

// fakeRunController is the test double for RunController: a set of running
// sessions, the Stop calls it received, and an optional error every Stop
// returns. It stands in for *worker.Pool so these tests need no worker
// package and no broker.
type fakeRunController struct {
	running map[string]bool
	stops   []stopCall
	err     error // when set, every Stop returns it
}

// stopCall is one recorded Stop(sessionID, reason) invocation.
type stopCall struct {
	sessionID string
	reason    string
}

func (f *fakeRunController) Stop(sessionID, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.stops = append(f.stops, stopCall{sessionID, reason})
	return nil
}

func (f *fakeRunController) Running(sessionID string) bool {
	return f.running[sessionID]
}

// newControlTestServer builds a test server with the run-control bearer token
// set and ctrl wired in — the server shape the stop endpoint needs.
// The token is fixed for the test; the controller's running set drives the
// preconditions.
func newControlTestServer(t *testing.T, ctrl *fakeRunController) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
		Run: ctrl, ControlToken: "test-control-token",
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

// controlAuth is the header set a passing stop request carries: JSON content
// type plus the test's bearer token.
var controlAuth = map[string]string{"Content-Type": "application/json", "Authorization": "Bearer test-control-token"}

// TestStopRequiresBearerToken pins the authentication on POST
// /api/sessions/{id}/stop (docs/RUN-CONTROL.md "Authentication"): a missing
// Authorization header is 401, a wrong token is 401, and only the configured
// token reaches the controller. The constant-time comparison is the handler's
// concern; here the wire behaviour is pinned.
func TestStopRequiresBearerToken(t *testing.T) {
	ctrl := &fakeRunController{running: map[string]bool{"sess-1": true}}
	srv, st := newControlTestServer(t, ctrl)
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"right token", "Bearer test-control-token", http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			if tc.header != "" {
				headers["Authorization"] = tc.header
			}
			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{"reason":"operator intervened"}`, headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("401 Content-Type = %q, want JSON", ct)
				}
			}
		})
	}

	if len(ctrl.stops) != 1 || ctrl.stops[0].sessionID != "sess-1" || ctrl.stops[0].reason != "operator intervened" {
		t.Fatalf("controller saw %+v, want one Stop(sess-1, operator intervened)", ctrl.stops)
	}
}

// TestStopFailsClosedWithoutToken pins the fail-closed property: a Server
// built without the startup token generation — every test that does not set
// ControlToken — answers 503 to the stop endpoint. A missing credential must
// not let the request through.
func TestStopFailsClosedWithoutToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		Run:    &fakeRunController{running: map[string]bool{"sess-1": true}},
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{}`, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer test-control-token",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
}

// TestStopCarriesTheSharedWriteGuards pins that the stop endpoint runs the
// same guards every other write carries — 415 without a JSON content type,
// 403 cross-origin — before anything else, exactly like the settings and
// session handlers (docs/DATA-API.md "The guards every write carries"), and
// that a refused request never reaches the controller.
func TestStopCarriesTheSharedWriteGuards(t *testing.T) {
	ctrl := &fakeRunController{running: map[string]bool{"sess-1": true}}
	srv, st := newControlTestServer(t, ctrl)
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "missing content type",
			headers: map[string]string{"Authorization": "Bearer test-control-token"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "wrong content type",
			headers: map[string]string{
				"Content-Type": "text/plain", "Authorization": "Bearer test-control-token",
			},
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "cross-origin",
			headers: map[string]string{
				"Content-Type": "application/json", "Authorization": "Bearer test-control-token",
				"Origin": "https://evil.example",
			},
			want: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{}`, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if len(ctrl.stops) != 0 {
		t.Fatalf("guarded requests must not reach the controller, got %+v", ctrl.stops)
	}
}

// TestStopPreconditionsAndIdempotence pins the stop endpoint's preconditions
// (docs/RUN-CONTROL.md "The HTTP surface"): 404 for a session that does not
// exist in the store, 409 naming the session's actual status when this
// process is not running it — the honest answer both for a session that
// already finished and for one being run by nothing at all — and 202
// {"session_id", "stopping": true} for an accepted stop, and again for a
// repeat stop, because stopping is idempotent: a second stop for a run
// already stopping is another 202, not a 409.
func TestStopPreconditionsAndIdempotence(t *testing.T) {
	ctrl := &fakeRunController{running: map[string]bool{"sess-1": true}}
	srv, st := newControlTestServer(t, ctrl)
	ctx := context.Background()
	mustCreateSession(t, st, "sess-1", time.Now())
	mustCreateSession(t, st, "finished", time.Now())
	mustCreateSession(t, st, "stuck", time.Now()) // running in the store, run by nothing at all
	finishedAt := time.Now().UTC()
	if err := st.UpdateSessionStatus(ctx, "finished", store.StatusOK, &finishedAt); err != nil {
		t.Fatalf("finish session: %v", err)
	}

	// 404: the session does not exist in the store, and no run owns it either.
	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/does-not-exist/stop", `{}`, controlAuth)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got status %d, want 404", resp.StatusCode)
	}

	// 409 naming the session's actual status: one that already finished, and
	// one being run by nothing at all.
	for _, tc := range []struct {
		sessionID string
		status    string
	}{
		{"finished", store.StatusOK},
		{"stuck", store.StatusRunning},
	} {
		resp := doWrite(t, srv, http.MethodPost, "/api/sessions/"+tc.sessionID+"/stop", `{}`, controlAuth)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s: got status %d, want 409 (body %s)", tc.sessionID, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "status "+tc.status) {
			t.Fatalf("%s: 409 must name the session's actual status, got %q", tc.sessionID, body)
		}
	}

	// 202: accepted, with the session id and stopping flag.
	resp = doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{"reason":"enough"}`, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("accepted stop: got status %d, want 202", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got["session_id"] != "sess-1" || got["stopping"] != true {
		t.Fatalf("202 body = %+v, want {\"session_id\":\"sess-1\",\"stopping\":true}", got)
	}

	// 202 again: a repeat stop is idempotent, not a conflict.
	resp = doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{}`, controlAuth)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("repeat stop: got status %d, want 202", resp.StatusCode)
	}

	if len(ctrl.stops) != 2 {
		t.Fatalf("controller saw %d stops, want 2: %+v", len(ctrl.stops), ctrl.stops)
	}
	if ctrl.stops[0].reason != "enough" {
		t.Fatalf("first stop reason = %q, want %q", ctrl.stops[0].reason, "enough")
	}
}

// TestStopRunWithNoSessionRowYet is the case the registry is asked about
// before the store: a run is registered before its workspace is prepared, and
// its session row is not created until the session loop starts. So a run
// wedged in a git clone is registered and stoppable while no row exists for
// it — and looking the store up first would answer 404 for exactly the run an
// operator most needs to end (docs/RUN-CONTROL.md "Half two").
func TestStopRunWithNoSessionRowYet(t *testing.T) {
	ctrl := &fakeRunController{running: map[string]bool{"sess-cloning": true}}
	srv, _ := newControlTestServer(t, ctrl)

	// No mustCreateSession: the store has never heard of this session.
	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-cloning/stop", `{"reason":"clone is wedged"}`, controlAuth)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a registered run with no session row: got status %d, want 202 (body %s)", resp.StatusCode, body)
	}
	if len(ctrl.stops) != 1 || ctrl.stops[0].sessionID != "sess-cloning" {
		t.Fatalf("expected one stop for sess-cloning, got %+v", ctrl.stops)
	}
	if ctrl.stops[0].reason != "clone is wedged" {
		t.Fatalf("stop reason = %q, want %q", ctrl.stops[0].reason, "clone is wedged")
	}
}

// TestStopSurfacesControllerFailure pins the one error path the handler owns
// beyond the preconditions: a controller whose Stop fails is a 500 carrying
// the error's message (docs/RUN-CONTROL.md). The handler never parses the
// error's text to distinguish "no such run" — it asks Running first, so this
// 500 is reserved for failures that are genuinely unexpected.
func TestStopSurfacesControllerFailure(t *testing.T) {
	ctrl := &fakeRunController{
		running: map[string]bool{"sess-1": true},
		err:     errors.New("pool is shutting down"),
	}
	srv, st := newControlTestServer(t, ctrl)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/stop", `{}`, controlAuth)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "pool is shutting down") {
		t.Fatalf("500 must carry the controller's error message, got %q", body)
	}
}

// TestGetControlTokenServesLoopbackOnly pins GET /api/control-token
// (docs/RUN-CONTROL.md "Authentication"): the token is served to a loopback
// RemoteAddr — the shape the browser and a same-host MCP server get it in —
// and a non-local caller is 403.
func TestGetControlTokenServesLoopbackOnly(t *testing.T) {
	srv, _ := newControlTestServer(t, &fakeRunController{})

	resp, err := http.Get(srv.URL + "/api/control-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback GET: got status %d, want 200", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "test-control-token" {
		t.Fatalf("token = %q, want the configured token", got["token"])
	}

	// A non-local (public) RemoteAddr is refused with 403.
	api := &Server{
		ControlToken: "test-control-token",
		Static:       http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/control-token", nil)
	req.RemoteAddr = "192.0.2.1:4567"
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-local GET: got status %d, want 403", rr.Code)
	}
}

// TestGetControlTokenServesDockerNATedCaller pins the reason
// isLocalCallerAddr also trusts private-range addresses, not just loopback:
// docker-compose publishes the harness's port as 127.0.0.1:8180, but a
// browser on the host hitting that address arrives inside the container
// NAT'd through the compose network's gateway (e.g. 172.22.0.1), not as
// 127.0.0.1. Rejecting that RemoteAddr is what produced "run control not
// configured" for a genuinely local caller.
func TestGetControlTokenServesDockerNATedCaller(t *testing.T) {
	api := &Server{
		ControlToken: "test-control-token",
		Static:       http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/control-token", nil)
	req.RemoteAddr = "172.22.0.1:54321"
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("docker-gateway GET: got status %d, want 200", rr.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "test-control-token" {
		t.Fatalf("token = %q, want the configured token", got["token"])
	}
}

// --- run control: steer ---

// TestSteerRequiresBearerToken pins the authentication on POST
// /api/sessions/{id}/steer (docs/RUN-CONTROL.md "Authentication"): a missing
// Authorization header is 401, a wrong token is 401, and only the configured
// token reaches the store write.
func TestSteerRequiresBearerToken(t *testing.T) {
	srv, st := newControlTestServer(t, &fakeRunController{})
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"right token", "Bearer test-control-token", http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			if tc.header != "" {
				headers["Authorization"] = tc.header
			}
			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", `{"text":"be terse"}`, headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("401 Content-Type = %q, want JSON", ct)
				}
			}
		})
	}
}

// TestSteerFailsClosedWithoutToken pins the fail-closed property for steer:
// a Server built without the startup token generation answers 503, exactly
// like stop — the run-control surface must never let a request through
// without a credential.
func TestSteerFailsClosedWithoutToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	mustCreateSession(t, st, "sess-1", time.Now())

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", `{"text":"be terse"}`, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer test-control-token",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
}

// TestSteerCarriesTheSharedWriteGuards pins that the steer endpoint runs the
// same guards every other write carries — 415 without a JSON content type,
// 403 cross-origin — before the bearer check, and that a refused request
// never appends a steer_message event.
func TestSteerCarriesTheSharedWriteGuards(t *testing.T) {
	srv, st := newControlTestServer(t, &fakeRunController{})
	mustCreateSession(t, st, "sess-1", time.Now())

	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "missing content type",
			headers: map[string]string{"Authorization": "Bearer test-control-token"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "wrong content type",
			headers: map[string]string{
				"Content-Type": "text/plain", "Authorization": "Bearer test-control-token",
			},
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "cross-origin",
			headers: map[string]string{
				"Content-Type": "application/json", "Authorization": "Bearer test-control-token",
				"Origin": "https://evil.example",
			},
			want: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", `{"text":"be terse"}`, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	events, err := st.GetEvents(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == store.KindSteerMessage {
			t.Fatalf("a guarded request appended a steer_message event: %+v", e)
		}
	}
}

// TestSteerPreconditions pins the steer endpoint's preconditions
// (docs/RUN-CONTROL.md "The HTTP surface"): 400 for empty or whitespace-only
// text, 404 for a session that does not exist, 409 naming the session's
// status when it is not running — a steer for a finished run would sit in the
// log forever, unapplied and unexplained — and 202 {"session_id", "seq"} for
// an accepted steer. Two steers are two instructions, so the second gets its
// own, higher seq rather than a duplicate of the first.
func TestSteerPreconditions(t *testing.T) {
	srv, st := newControlTestServer(t, &fakeRunController{})
	ctx := context.Background()
	mustCreateSession(t, st, "sess-1", time.Now()) // running by default
	mustCreateSession(t, st, "finished", time.Now())
	finishedAt := time.Now().UTC()
	if err := st.UpdateSessionStatus(ctx, "finished", store.StatusOK, &finishedAt); err != nil {
		t.Fatalf("finish session: %v", err)
	}

	// 400: empty and whitespace-only text.
	for _, body := range []string{`{"text":""}`, `{"text":"   "}`} {
		resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", body, controlAuth)
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: got status %d, want 400 (body %s)", body, resp.StatusCode, text)
		}
	}

	// 404: the session does not exist.
	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/does-not-exist/steer", `{"text":"hi"}`, controlAuth)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got status %d, want 404", resp.StatusCode)
	}

	// 409 naming the session's actual status: one that already finished.
	resp = doWrite(t, srv, http.MethodPost, "/api/sessions/finished/steer", `{"text":"hi"}`, controlAuth)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("finished session: got status %d, want 409 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "status ok") {
		t.Fatalf("409 must name the session's actual status, got %q", body)
	}

	// A creating session refuses the same way, but the message says its
	// workspace is still being prepared — not that the run is over, which a
	// bare "not running" would read as.
	mustCreateSession(t, st, "creating-sess", time.Now())
	if err := st.UpdateSessionStatus(ctx, "creating-sess", store.StatusCreating, nil); err != nil {
		t.Fatalf("mark creating: %v", err)
	}
	resp = doWrite(t, srv, http.MethodPost, "/api/sessions/creating-sess/steer", `{"text":"hi"}`, controlAuth)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("creating session: got status %d, want 409 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "still being prepared") {
		t.Fatalf("409 must say the workspace is still being prepared, got %q", body)
	}

	// 202 with the seq the text landed at; the second steer lands at a
	// higher seq (steering is not idempotent — two steers, two instructions).
	var firstSeq, secondSeq int64
	for i, body := range []string{`{"text":"first steer"}`, `{"text":"second steer"}`} {
		resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", body, controlAuth)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("steer %d: got status %d, want 202", i, resp.StatusCode)
		}
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got["session_id"] != "sess-1" {
			t.Fatalf("202 body = %+v, want session_id sess-1", got)
		}
		seq, ok := got["seq"].(float64)
		if !ok || seq <= 0 {
			t.Fatalf("202 body = %+v, want a positive seq", got)
		}
		if i == 0 {
			firstSeq = int64(seq)
		} else {
			secondSeq = int64(seq)
		}
	}
	if secondSeq <= firstSeq {
		t.Fatalf("second steer seq %d must be higher than the first %d", secondSeq, firstSeq)
	}

	// The events landed in the store with the seqs the response named.
	events, err := st.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	texts := map[int64]string{}
	for _, e := range events {
		if e.Kind != store.KindSteerMessage {
			continue
		}
		var p store.SteerMessagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		texts[e.Seq] = p.Text
	}
	if texts[firstSeq] != "first steer" || texts[secondSeq] != "second steer" {
		t.Fatalf("stored steer_message events by seq = %v, want %d->first steer, %d->second steer", texts, firstSeq, secondSeq)
	}
}

// TestSteerAppendsEventAndPublishes pins the handler's two effects beyond
// the 202: the steer_message event is committed to the store (the loop's one
// read source) and fanned out to the session's SSE stream, so the transcript
// shows the steer as pending the moment it is accepted rather than at the
// next sub-turn (docs/RUN-CONTROL.md "How the loop picks one up").
func TestSteerAppendsEventAndPublishes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New()
	api := &Server{
		Store: st, Hub: h, Settings: settings.NewResolver(st),
		Static:       http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		ControlToken: "test-control-token",
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	mustCreateSession(t, st, "sess-1", time.Now())

	events, cancel := h.Subscribe("sess-1")
	defer cancel()

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/steer", `{"text":"be terse","source":"mcp"}`, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	seq, ok := got["seq"].(float64)
	if !ok || seq <= 0 {
		t.Fatalf("202 body = %+v, want a positive seq", got)
	}

	select {
	case frame := <-events:
		ev := frame.Event
		if ev.Kind != store.KindSteerMessage || ev.Seq != int64(seq) {
			t.Fatalf("hub event = %+v, want the steer_message at seq %d", ev, int64(seq))
		}
		var p store.SteerMessagePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Text != "be terse" || p.Source != "mcp" {
			t.Fatalf("steer payload = %+v, want text \"be terse\" from mcp", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the steer_message event was not published to the session's stream")
	}

	// The loop's read source: the committed event, text verbatim.
	eventsList, err := st.GetEvents(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsList) != 1 || eventsList[0].Seq != int64(seq) {
		t.Fatalf("store events = %+v, want exactly the steer_message at seq %d", eventsList, int64(seq))
	}
}

// --- run control: start ---

// fakeRunPublisher is the test double for RunPublisher: records the requests
// it was asked to publish, and an optional error every call returns. It
// stands in for the adapter cmd/harness wires over the JetStream handle, so
// these tests need no broker.
type fakeRunPublisher struct {
	requests []queue.Request
	err      error // when set, every PublishRequest returns it
}

func (f *fakeRunPublisher) PublishRequest(_ context.Context, req queue.Request) error {
	if f.err != nil {
		return f.err
	}
	f.requests = append(f.requests, req)
	return nil
}

// newStartTestServer builds a test server with the run-control bearer token
// set and pub wired in — the server shape the start endpoint needs. The
// token is fixed for the test; the fake publisher records what the handler
// asked it to publish.
func newStartTestServer(t *testing.T, pub *fakeRunPublisher) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
		Publisher: pub, ControlToken: "test-control-token",
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

// aValidWorkRequest is a request body that passes queue.Request.Validate: the
// browser form's minimum (prompt, one repo, a permission mode).
const aValidWorkRequest = `{"prompt":"do the thing","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"readonly"}`

// TestStartRunRequiresBearerToken pins the authentication on POST /api/runs
// (docs/RUN-CONTROL.md "Authentication"): a missing Authorization header is
// 401, a wrong token is 401, and only the configured token reaches the
// publisher.
func TestStartRunRequiresBearerToken(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"right token", "Bearer test-control-token", http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			if tc.header != "" {
				headers["Authorization"] = tc.header
			}
			resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("401 Content-Type = %q, want JSON", ct)
				}
			}
		})
	}

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1: %+v", len(pub.requests), pub.requests)
	}
}

// TestStartRunFailsClosedWithoutToken pins the fail-closed property: a Server
// built without the startup token generation answers 503 to the start
// endpoint, exactly like stop and steer — the run-control surface must never
// let a request through without a credential.
func TestStartRunFailsClosedWithoutToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pub := &fakeRunPublisher{}
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static:    http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		Publisher: pub,
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer test-control-token",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
	if len(pub.requests) != 0 {
		t.Fatalf("a refused request must not reach the publisher, got %+v", pub.requests)
	}
}

// TestStartRunFailsClosedWithoutPublisher pins the other fail-closed shape:
// a Server built with a token but no publisher wired — every test server that
// does not set the field — answers 503 to the start endpoint. No publisher
// means no run can be started, and that must look unavailable, not succeed
// silently.
func TestStartRunFailsClosedWithoutPublisher(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Static:       http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		ControlToken: "test-control-token",
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, controlAuth)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "no run publisher is wired") {
		t.Fatalf("503 must say the publisher is not wired, got %q", body)
	}
}

// TestStartRunCarriesTheSharedWriteGuards pins that the start endpoint runs
// the same guards every other write carries — 415 without a JSON content
// type, 403 cross-origin — before the bearer check, and that a refused
// request never reaches the publisher.
func TestStartRunCarriesTheSharedWriteGuards(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "missing content type",
			headers: map[string]string{"Authorization": "Bearer test-control-token"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "wrong content type",
			headers: map[string]string{
				"Content-Type": "text/plain", "Authorization": "Bearer test-control-token",
			},
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "cross-origin",
			headers: map[string]string{
				"Content-Type": "application/json", "Authorization": "Bearer test-control-token",
				"Origin": "https://evil.example",
			},
			want: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if len(pub.requests) != 0 {
		t.Fatalf("guarded requests must not reach the publisher, got %+v", pub.requests)
	}
}

// TestStartRunValidationRejectsBadBody pins that POST /api/runs validates
// with the queue's own rules, never a copy (docs/RUN-CONTROL.md "POST
// /api/runs"): a body that fails queue.Request.Validate is a 400 carrying
// the validator's message, and the publisher is never called. A malformed
// JSON body is a 400 too.
func TestStartRunValidationRejectsBadBody(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	for _, tc := range []struct {
		name   string
		body   string
		wantIn string
	}{
		{"not JSON", `{`, "invalid JSON body"},
		{"no repos", `{"prompt":"do it","repos":[],"permission_mode":"readonly"}`, "queue: repos is required"},
		{"bad repo url", `{"prompt":"do it","repos":[{"url":"/etc/passwd"}],"permission_mode":"readonly"}`, "must be an http(s), ssh, git"},
		{"no permission mode", `{"prompt":"do it","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":""}`, "queue: permission_mode is required"},
		{"unknown permission mode", `{"prompt":"do it","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"admin"}`, "must be readonly or full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWrite(t, srv, http.MethodPost, "/api/runs", tc.body, controlAuth)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400 (body %s)", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.wantIn) {
				t.Fatalf("400 must carry the validator's message, got %q (want a mention of %q)", body, tc.wantIn)
			}
		})
	}

	if len(pub.requests) != 0 {
		t.Fatalf("a rejected request must not reach the publisher, got %+v", pub.requests)
	}
}

// TestStartRunGeneratesRequestID pins the idempotency-key story for a
// browser start (docs/RUN-CONTROL.md "POST /api/runs"): request_id is
// optional on this surface, and a body without one is accepted under a
// generated "web-" id — the browser form has no idempotency key to offer,
// but the published request must carry one, because the queue requires it.
func TestStartRunGeneratesRequestID(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, body)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := got["request_id"]
	if !strings.HasPrefix(id, "web-") {
		t.Fatalf("202 request_id = %q, want a generated web- id", id)
	}
	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	if pub.requests[0].RequestID != id {
		t.Fatalf("published RequestID = %q, want the id the 202 named (%q)", pub.requests[0].RequestID, id)
	}
}

// TestStartRunCarriesTitleFields pins that the four session fields flow
// through POST /api/runs untouched: startRunBody embeds queue.Request, so a
// body carrying a title, description, and phase position reaches the
// published request with no handler change.
func TestStartRunCarriesTitleFields(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"prompt":"do it","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"full","title":"Add session title fields","description":"Carry a title, description, and phase position from every producer onto the session row.","phase":2,"total_phases":5}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	resp.Body.Close()

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	if published.Title != "Add session title fields" {
		t.Fatalf("published Title = %q, want the submitted title", published.Title)
	}
	if published.Description != "Carry a title, description, and phase position from every producer onto the session row." {
		t.Fatalf("published Description = %q, want the submitted description", published.Description)
	}
	if published.Phase != 2 || published.TotalPhases != 5 {
		t.Fatalf("published phase = %d/%d, want 2/5", published.Phase, published.TotalPhases)
	}
	// The handler's provenance stamp still applies on top of the body.
	if !published.ParentIsUser {
		t.Fatal("published ParentIsUser = false, want the handler's true stamp")
	}
}

// TestStartRunAcceptsWithoutPrompt pins that a browser start may create the
// run without an initial prompt (docs/RUN-CONTROL.md "Start"): the prompt is
// optional on this surface, so a body that is otherwise complete is accepted
// and published with an empty Prompt, and the operator types the first
// message into the session once it appears.
func TestStartRunAcceptsWithoutPrompt(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"full"}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	resp.Body.Close()
	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	if published.Prompt != "" {
		t.Fatalf("published Prompt = %q, want empty for a promptless start", published.Prompt)
	}
	if len(published.Repos) != 1 || published.Repos[0].URL != "https://github.com/org/app.git" {
		t.Fatalf("published repos = %+v, want the submitted repo", published.Repos)
	}
	if published.PermissionMode != "full" {
		t.Fatalf("published PermissionMode = %q, want full", published.PermissionMode)
	}
}

// TestStartRunAcceptsAndPublishes pins the happy path end to end: a full
// work request (every optional field) is decoded, validated, and handed to
// the publisher verbatim, and the 202 names the request_id the caller
// supplied — which is how a caller that has an idempotency key gets the same
// deduplication every other producer gets. A publisher failure is a 500.
func TestStartRunAcceptsAndPublishes(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"request_id":"req-from-browser","prompt":"do it","repos":[{"url":"https://github.com/org/app.git","branch":"dev"}],"permission_mode":"full","model":"deepseek-v4-pro","effort":"max","deny":["git push"],"max_sub_turns":42,"deadline_ms":3600000,"job_type":"implementation"}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got["request_id"] != "req-from-browser" {
		t.Fatalf("202 body = %+v, want the supplied request_id echoed back", got)
	}

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	want := queue.Request{
		RequestID:      "req-from-browser",
		Prompt:         "do it",
		Repos:          []queue.Repo{{URL: "https://github.com/org/app.git", Branch: "dev"}},
		PermissionMode: "full",
		Model:          "deepseek-v4-pro",
		Effort:         "max",
		Deny:           []string{"git push"},
		MaxSubTurns:    42,
		DeadlineMS:     3600000,
		JobType:        "implementation",
		// The handler stamps the provenance: a browser start is a person
		// starting the run, with no parent agent and — with no operator name
		// configured — no id.
		ParentIsUser: true,
	}
	if !reflect.DeepEqual(published, want) {
		t.Fatalf("published request = %+v, want %+v", published, want)
	}

	// A publisher failure surfaces as a 500 carrying its message.
	failing := &fakeRunPublisher{err: errors.New("stream is read-only")}
	srv2, _ := newStartTestServer(t, failing)
	resp2 := doWrite(t, srv2, http.MethodPost, "/api/runs", aValidWorkRequest, controlAuth)
	gotBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusInternalServerError {
		t.Fatalf("publisher failure: got status %d, want 500 (body %s)", resp2.StatusCode, gotBody)
	}
	if !strings.Contains(string(gotBody), "stream is read-only") {
		t.Fatalf("500 must carry the publisher's error message, got %q", gotBody)
	}
}

// TestStartRunStampsProvenanceOverBody pins the overwrite rule (D5): a body
// that asserts its own provenance — parent_is_user false, a parent agent
// type and id — is accepted, and the published request carries the
// handler's stamp instead: a person started the run, so parent_is_user is
// true and both other fields are empty. The body's values are discarded,
// never answered with a 400 that would force the frontend to carry a field
// it must not send.
func TestStartRunStampsProvenanceOverBody(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"prompt":"do it","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"full","parent_is_user":false,"parent_agent_type":"claude-code","parent_agent_id":"sess-lie"}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	resp.Body.Close()

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	if !published.ParentIsUser {
		t.Fatalf("published ParentIsUser = false, want the handler's true stamp")
	}
	if published.ParentAgentType != "" || published.ParentAgentID != "" {
		t.Fatalf("published parent agent = %q/%q, want both empty under the handler's stamp", published.ParentAgentType, published.ParentAgentID)
	}
}

// TestStartRunStampsConfiguredOperator pins identity.operator on the
// published request (D2, D7): with the setting set to "geoff", the
// published ParentAgentID is "geoff"; with a malformed value (one containing
// a space), the published id is "" and the start still returns 202 — an
// unconfigured or malformed operator name degrades to an unnamed person,
// never a failed start.
func TestStartRunStampsConfiguredOperator(t *testing.T) {
	ctx := context.Background()

	t.Run("valid name", func(t *testing.T) {
		pub := &fakeRunPublisher{}
		srv, st := newStartTestServer(t, pub)
		if err := settings.NewResolver(st).Set(ctx, settings.KeyIdentityOperator, "geoff"); err != nil {
			t.Fatalf("set identity.operator: %v", err)
		}
		resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, controlAuth)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("got status %d, want 202", resp.StatusCode)
		}
		if got := pub.requests[0].ParentAgentID; got != "geoff" {
			t.Fatalf("published ParentAgentID = %q, want geoff", got)
		}
		if !pub.requests[0].ParentIsUser {
			t.Fatalf("published ParentIsUser = false, want true")
		}
	})

	t.Run("malformed name", func(t *testing.T) {
		pub := &fakeRunPublisher{}
		srv, st := newStartTestServer(t, pub)
		if err := settings.NewResolver(st).Set(ctx, settings.KeyIdentityOperator, "geoff the great"); err != nil {
			t.Fatalf("set identity.operator: %v", err)
		}
		resp := doWrite(t, srv, http.MethodPost, "/api/runs", aValidWorkRequest, controlAuth)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("got status %d, want 202", resp.StatusCode)
		}
		if got := pub.requests[0].ParentAgentID; got != "" {
			t.Fatalf("published ParentAgentID = %q, want empty for a malformed operator name", got)
		}
		if !pub.requests[0].ParentIsUser {
			t.Fatalf("published ParentIsUser = false, want true")
		}
	})
}

// TestStartRunWritesAttachmentsBeforePublishing pins the attachment path
// end to end: a browser start carrying images stores their bytes in the
// store's attachments table before the publish, and the published request
// carries the ids — never the bytes — so the NATS request stays small and
// the worker can materialise the files into scratch/attachments/.
func TestStartRunWritesAttachmentsBeforePublishing(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, st := newStartTestServer(t, pub)

	body := `{
		"prompt":"make the page match the mockup",
		"repos":[{"url":"https://github.com/org/app.git"}],
		"permission_mode":"readonly",
		"attachments":[
			{"name":"mockup.png","mime_type":"image/png","data":"` + base64.StdEncoding.EncodeToString([]byte("mockup bytes")) + `"},
			{"name":"dark.webp","mime_type":"image/webp","data":"` + base64.StdEncoding.EncodeToString([]byte("dark bytes")) + `"}
		]
	}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	if len(published.AttachmentIDs) != 2 {
		t.Fatalf("published request carries %d attachment ids, want 2: %+v", len(published.AttachmentIDs), published)
	}

	// Each id the request carries resolves to a stored row with the right
	// name, MIME type, and bytes: the worker reads exactly these back.
	for i, want := range []struct{ name, mime, data string }{
		{"mockup.png", "image/png", "mockup bytes"},
		{"dark.webp", "image/webp", "dark bytes"},
	} {
		att, err := st.GetAttachment(context.Background(), published.AttachmentIDs[i])
		if err != nil {
			t.Fatalf("attachment %d not in the store: %v", i, err)
		}
		if att.Name != want.name || att.MIMEType != want.mime || string(att.Data) != want.data {
			t.Errorf("attachment %d = %+v, want %+v", i, att, want)
		}
	}
}

// TestStartRunRefusesBadAttachments pins the attachment validation: a bad
// extension, a mismatched MIME type, a path-shaped name, and an over-cap
// file are all 400s, and none of them reaches the publisher — a request
// whose attachment fails validation never starts.
func TestStartRunRefusesBadAttachments(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("mockup bytes"))
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "bad extension",
			body: `{"prompt":"p","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"readonly","attachments":[{"name":"mockup.gif","mime_type":"image/gif","data":"` + valid + `"}]}`,
			want: "only PNG, JPEG, and WebP",
		},
		{
			name: "mime mismatch",
			body: `{"prompt":"p","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"readonly","attachments":[{"name":"mockup.png","mime_type":"image/webp","data":"` + valid + `"}]}`,
			want: "does not match",
		},
		{
			name: "path-shaped name",
			body: `{"prompt":"p","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"readonly","attachments":[{"name":"../mockup.png","mime_type":"image/png","data":"` + valid + `"}]}`,
			want: "plain file name",
		},
		{
			name: "not base64",
			body: `{"prompt":"p","repos":[{"url":"https://github.com/org/app.git"}],"permission_mode":"readonly","attachments":[{"name":"mockup.png","mime_type":"image/png","data":"!!!not base64!!!"}]}`,
			want: "not valid base64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakeRunPublisher{}
			srv, _ := newStartTestServer(t, pub)
			resp := doWrite(t, srv, http.MethodPost, "/api/runs", tc.body, controlAuth)
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400 (body %s)", resp.StatusCode, got)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Errorf("400 should name the problem (%q), got %s", tc.want, got)
			}
			if len(pub.requests) != 0 {
				t.Errorf("a refused attachment must not publish, got %+v", pub.requests)
			}
		})
	}
}
