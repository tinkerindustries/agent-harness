package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// fakeHarness stands in for `harness serve` for deepseek_stop's tests: it
// serves exactly the endpoints the tool reaches — GET /api/control-token,
// GET /api/requests/{request_id}, POST /api/sessions/{id}/stop — and records
// what it saw, so the tests can pin the wire shape (path, bearer, body)
// without a real server. No NATS is involved: these are pure HTTP-client
// tests, unlike the launch/collect tests that need the broker.
type fakeHarness struct {
	// controlToken is what GET /api/control-token returns.
	controlToken string
	// resolvedSession is what GET /api/requests/{request_id} returns as the
	// request's session_id. Empty stands in for a request with no session.
	resolvedSession string
	// stopStatus is what POST .../stop answers; stopBody its body.
	stopStatus int
	stopBody   string

	controlTokenFetches int
	posts               []string // session ids posted to .../stop
	reasons             []string
	authorizations      []string
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{stopStatus: http.StatusAccepted}
}

func (f *fakeHarness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/control-token":
		f.controlTokenFetches++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": f.controlToken})
	case strings.HasPrefix(r.URL.Path, "/api/requests/"):
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workRequestRow{
			RequestID: strings.TrimPrefix(r.URL.Path, "/api/requests/"),
			SessionID: f.resolvedSession,
			Status:    "running",
		})
	case strings.HasPrefix(r.URL.Path, "/api/sessions/") && strings.HasSuffix(r.URL.Path, "/stop"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/stop")
		f.posts = append(f.posts, id)
		f.authorizations = append(f.authorizations, r.Header.Get("Authorization"))
		var body struct{ Reason string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.reasons = append(f.reasons, body.Reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.stopStatus)
		if f.stopStatus == http.StatusAccepted {
			json.NewEncoder(w).Encode(stopOutput{SessionID: id, Stopping: true})
		} else {
			w.Write([]byte(f.stopBody))
		}
	default:
		http.NotFound(w, r)
	}
}

// stopTestService wires a Service whose HTTP calls land on the fake harness.
func stopTestService(t *testing.T, f *fakeHarness, controlToken string) (*Service, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	svc := &Service{
		Cfg: config.MCPConfig{
			HarnessBaseURL:   srv.URL,
			HarnessPublicURL: srv.URL,
			ControlToken:     controlToken,
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	return svc, srv
}

func callStop(t *testing.T, svc *Service, in stopInput) *mcpsdk.CallToolResult {
	t.Helper()
	res, _, err := svc.handleStop(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	return res
}

// TestHandleStopBySessionID is the plain shape: a session_id, a configured
// token, and a 202. The bearer comes from the config and never touches
// /api/control-token, and the reason is carried verbatim.
func TestHandleStopBySessionID(t *testing.T) {
	f := newFakeHarness()
	svc, _ := stopTestService(t, f, "configured-token")

	res := callStop(t, svc, stopInput{SessionID: "sess-1", Reason: "operator intervened"})

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if f.controlTokenFetches != 0 {
		t.Fatalf("expected the configured token to skip GET /api/control-token, fetched %d times", f.controlTokenFetches)
	}
	if len(f.posts) != 1 || f.posts[0] != "sess-1" {
		t.Fatalf("expected one stop for sess-1, got %v", f.posts)
	}
	if f.authorizations[0] != "Bearer configured-token" {
		t.Fatalf("Authorization = %q, want the configured token as a bearer", f.authorizations[0])
	}
	if f.reasons[0] != "operator intervened" {
		t.Fatalf("reason = %q, want operator intervened", f.reasons[0])
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	for _, want := range []string{"stopping", "session_id: sess-1", "not necessarily ended"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the tool text to contain %q, got: %s", want, text)
		}
	}
	lo, ok := res.StructuredContent.(stopOutput)
	if !ok {
		t.Fatalf("expected stopOutput structured content, got %T", res.StructuredContent)
	}
	if lo.SessionID != "sess-1" || !lo.Stopping {
		t.Fatalf("unexpected structured content: %+v", lo)
	}
}

// TestHandleStopResolvesRequestID pins request_id resolution: the tool looks
// the request up over GET /api/requests/{request_id} — the same lookup
// deepseek_result's request-to-session resolution uses — and stops the
// session the row names, never guessing.
func TestHandleStopResolvesRequestID(t *testing.T) {
	f := newFakeHarness()
	f.resolvedSession = "sess-resolved"
	svc, _ := stopTestService(t, f, "configured-token")

	res := callStop(t, svc, stopInput{RequestID: "req-1"})

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if len(f.posts) != 1 || f.posts[0] != "sess-resolved" {
		t.Fatalf("expected the stop to land on the resolved session sess-resolved, got %v", f.posts)
	}
}

// TestHandleStopRequestWithNoSession reports a request that never produced a
// session plainly instead of reaching for the stop endpoint with nothing to
// stop — a request still queued, or one whose worker died before its session
// existed, cannot be stopped.
func TestHandleStopRequestWithNoSession(t *testing.T) {
	f := newFakeHarness()
	f.resolvedSession = ""
	svc, _ := stopTestService(t, f, "configured-token")

	res := callStop(t, svc, stopInput{RequestID: "req-1"})

	if !res.IsError {
		t.Fatalf("expected an error result for a sessionless request, got %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "has no session yet") {
		t.Fatalf("expected the error to name the missing session, got: %+v", res.Content)
	}
	if len(f.posts) != 0 {
		t.Fatalf("expected no stop request for a sessionless request, got %v", f.posts)
	}
}

// TestHandleStopFetchesTokenWhenUnconfigured is the fallback path: no token
// in the process config, so the tool fetches one from GET /api/control-token
// on the harness's own base URL — the MCP server normally runs on the same
// host, where the loopback route works.
func TestHandleStopFetchesTokenWhenUnconfigured(t *testing.T) {
	f := newFakeHarness()
	f.controlToken = "loopback-token"
	svc, _ := stopTestService(t, f, "")

	res := callStop(t, svc, stopInput{SessionID: "sess-1"})

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if f.controlTokenFetches != 1 {
		t.Fatalf("expected one control-token fetch, got %d", f.controlTokenFetches)
	}
	if f.authorizations[0] != "Bearer loopback-token" {
		t.Fatalf("Authorization = %q, want the fetched token as a bearer", f.authorizations[0])
	}
}

// TestHandleStopFailsClosedOnEmptyToken pins the one wrinkle the design
// calls out: a harness that never generated a token answers GET
// /api/control-token with 200 and an empty token, and an empty token from
// either source is a plain error — never an empty bearer sent anyway, which
// would 503.
func TestHandleStopFailsClosedOnEmptyToken(t *testing.T) {
	f := newFakeHarness()
	f.controlToken = ""
	svc, _ := stopTestService(t, f, "")

	res := callStop(t, svc, stopInput{SessionID: "sess-1"})

	if !res.IsError {
		t.Fatalf("expected an error result for an empty token, got %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "no control token configured") {
		t.Fatalf("expected the error to say the harness has no control token, got: %+v", res.Content)
	}
	if len(f.posts) != 0 {
		t.Fatalf("expected no stop request without a token, got %v", f.posts)
	}
}

// TestHandleStopCarriesEndpointError surfaces the endpoint's own words: a
// 409 names the session's actual status, and that text is what the caller
// sees rather than a generic failure.
func TestHandleStopCarriesEndpointError(t *testing.T) {
	f := newFakeHarness()
	f.stopStatus = http.StatusConflict
	f.stopBody = `{"error":"session sess-1 is not running in this process (status ok)"}`
	svc, _ := stopTestService(t, f, "configured-token")

	res := callStop(t, svc, stopInput{SessionID: "sess-1"})

	if !res.IsError {
		t.Fatalf("expected an error result for a 409, got %+v", res.Content)
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	if !strings.Contains(text, "409") || !strings.Contains(text, "not running in this process (status ok)") {
		t.Fatalf("expected the endpoint's status and message to surface, got: %s", text)
	}
}

// TestHandleStopRequiresAnIdentifier pins the argument rule: one of
// session_id and request_id must be given.
func TestHandleStopRequiresAnIdentifier(t *testing.T) {
	f := newFakeHarness()
	svc, _ := stopTestService(t, f, "configured-token")

	res := callStop(t, svc, stopInput{})

	if !res.IsError {
		t.Fatalf("expected an error result with neither identifier, got %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "session_id or request_id is required") {
		t.Fatalf("expected the error to name the required argument, got: %+v", res.Content)
	}
}
