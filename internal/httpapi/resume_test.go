package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// mustCreateResumableSession makes a session row a resume can actually be
// published for: mustCreateSession's own rows carry permission_mode
// "default", a value the queue retired, and TestResumeSessionRefusesALegacy
// PermissionMode below is where that case is pinned on purpose.
func mustCreateResumableSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	err := st.CreateSession(t.Context(), store.Session{
		ID:             id,
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      "/tmp/" + id,
		PermissionMode: "full",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		CreatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// mustFinishSession moves a session to a terminal status, the state a resume
// is for.
func mustFinishSession(t *testing.T, st *store.Store, id, status string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.FinishSession(t.Context(), id, status, "done", "it did the thing", &now); err != nil {
		t.Fatalf("finish session %s: %v", id, err)
	}
}

// A resume publishes a work request naming the session, so it reaches a
// worker through the same queue every other run arrives on
// (docs/RUN-CONTROL.md "Continuing"). The published request carries the
// session's own permission mode and model — Runner.Resume reads both back
// off the frozen row, and carrying them here is what lets the request
// satisfy queue.Request.Validate unchanged.
func TestResumeSessionAcceptsAndPublishes(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, st := newStartTestServer(t, pub)
	mustCreateResumableSession(t, st, "sess-1")
	mustFinishSession(t, st, "sess-1", store.StatusOK)

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"now change the front panel"}`, controlAuth)
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
	if got["session_id"] != "sess-1" || got["request_id"] == "" {
		t.Fatalf("202 body = %+v, want the session id and a generated request id", got)
	}

	if len(pub.requests) != 1 {
		t.Fatalf("publisher saw %d requests, want 1", len(pub.requests))
	}
	published := pub.requests[0]
	want := queue.Request{
		RequestID:       published.RequestID,
		ResumeSessionID: "sess-1",
		Prompt:          "now change the front panel",
		Model:           "deepseek-v4-pro",
		PermissionMode:  "full",
		// Copied off the row like the rest: a continuation is the same kind
		// of job the session already was.
		JobType:      "implementation",
		ParentIsUser: true,
	}
	if !reflect.DeepEqual(published, want) {
		t.Fatalf("published request = %+v, want %+v", published, want)
	}
	// No repos, and that is the point: the session keeps the workspace it
	// already has.
	if len(published.Repos) != 0 {
		t.Fatalf("published repos = %+v, want none", published.Repos)
	}
}

// Every terminal status is continuable, not just a clean finish: a run that
// failed, timed out or was stopped is exactly the kind worth talking your way
// out of rather than restarting from a fresh clone.
func TestResumeSessionAcceptsEveryTerminalStatus(t *testing.T) {
	for _, status := range []string{store.StatusOK, store.StatusFailed, store.StatusTimeout, store.StatusCancelled, store.StatusMaxTurns} {
		t.Run(status, func(t *testing.T) {
			pub := &fakeRunPublisher{}
			srv, st := newStartTestServer(t, pub)
			mustCreateResumableSession(t, st, "sess-1")
			mustFinishSession(t, st, "sess-1", status)

			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"keep going"}`, controlAuth)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status %s: got %d, want 202 (body %s)", status, resp.StatusCode, body)
			}
		})
	}
}

// The three refusals Runner.Resume itself makes, stated by the handler so a
// caller hears them now rather than a worker discovering them later.
func TestResumeSessionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		wantStatus int
		wantIn     string
	}{
		{"a running session wants steer", store.StatusRunning, http.StatusConflict, "steer it instead"},
		{"a creating session is not ready", store.StatusCreating, http.StatusConflict, "steer it instead"},
		{"a compacted session names its child", store.StatusCompacted, http.StatusConflict, "resume its child session instead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakeRunPublisher{}
			srv, st := newStartTestServer(t, pub)
			mustCreateResumableSession(t, st, "sess-1")
			if tc.status != store.StatusRunning {
				if err := st.UpdateSessionStatus(t.Context(), "sess-1", tc.status, nil); err != nil {
					t.Fatalf("set status %s: %v", tc.status, err)
				}
			}

			resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"keep going"}`, controlAuth)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("got status %d, want %d (body %s)", resp.StatusCode, tc.wantStatus, body)
			}
			if !strings.Contains(string(body), tc.wantIn) {
				t.Fatalf("body %q, want it to contain %q", body, tc.wantIn)
			}
			if len(pub.requests) != 0 {
				t.Fatalf("a refused resume published %d requests, want 0", len(pub.requests))
			}
		})
	}
}

// An unknown session is a 404, and an empty message a 400 — the same two the
// steer endpoint beside it makes.
func TestResumeSessionRejectsUnknownSessionAndEmptyText(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, st := newStartTestServer(t, pub)
	mustCreateResumableSession(t, st, "sess-1")
	mustFinishSession(t, st, "sess-1", store.StatusOK)

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/nope/resume", `{"text":"keep going"}`, controlAuth)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got status %d, want 404", resp.StatusCode)
	}

	for _, body := range []string{`{"text":""}`, `{"text":"   "}`, `{}`} {
		resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", body, controlAuth)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: got status %d, want 400", body, resp.StatusCode)
		}
	}
	if len(pub.requests) != 0 {
		t.Fatalf("publisher saw %d requests, want 0", len(pub.requests))
	}
}

// Authentication and the missing-capability case fail closed, exactly as the
// start and stop endpoints do.
func TestResumeSessionFailsClosed(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, st := newStartTestServer(t, pub)
	mustCreateResumableSession(t, st, "sess-1")
	mustFinishSession(t, st, "sess-1", store.StatusOK)

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"go"}`, map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: got status %d, want 401", resp.StatusCode)
	}
	resp = doWrite(t, srv, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"go"}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer wrong"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got status %d, want 401", resp.StatusCode)
	}

	// No publisher wired: a 503, the same shape POST /api/runs uses, because
	// a server that cannot enqueue anything cannot continue a session either.
	// The Server is built without the field rather than with a nil fake: a
	// typed nil in an interface is not nil, and would sail past the check.
	dir := t.TempDir()
	st2, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	api := &Server{
		Store: st2, Hub: hub.New(), Settings: settings.NewResolver(st2),
		Static:       http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		ControlToken: "test-control-token",
	}
	srv2 := httptest.NewServer(api.Handler())
	t.Cleanup(srv2.Close)
	mustCreateResumableSession(t, st2, "sess-1")
	mustFinishSession(t, st2, "sess-1", store.StatusOK)
	resp = doWrite(t, srv2, http.MethodPost, "/api/sessions/sess-1/resume", `{"text":"go"}`, controlAuth)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no publisher: got status %d, want 503 (body %s)", resp.StatusCode, body)
	}
}

// A session whose stored permission mode this build retired cannot be
// continued, and the refusal says so about the session rather than reading as
// a complaint about the message somebody just typed. Nothing in the caller's
// request is wrong, so it is a 409 and not a 400.
func TestResumeSessionRefusesALegacyPermissionMode(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, st := newStartTestServer(t, pub)
	// mustCreateSession's rows carry permission_mode "default", the value the
	// queue retired (queue.Request.Validate).
	mustCreateSession(t, st, "sess-old", time.Now())
	mustFinishSession(t, st, "sess-old", store.StatusOK)

	resp := doWrite(t, srv, http.MethodPost, "/api/sessions/sess-old/resume", `{"text":"keep going"}`, controlAuth)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got status %d, want 409 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "sess-old cannot be continued") {
		t.Fatalf("body %q, want it to name the session as the reason", body)
	}
	if len(pub.requests) != 0 {
		t.Fatalf("a refused resume published %d requests, want 0", len(pub.requests))
	}
}
