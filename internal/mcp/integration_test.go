package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/config"
	"github.com/mrgeoffrich/agent-harness/internal/httpapi"
	"github.com/mrgeoffrich/agent-harness/internal/queue"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// capturePublisher is the Publisher test double replacing the old
// capturingJS: it records the marshalled request it was handed — so a test
// can assert the exact bytes handleLaunch produces — and can be told to
// fail the publish or to run a callback once a launch lands (the way a
// worker's claim used to be simulated by publishing an accepted message).
type capturePublisher struct {
	mu        sync.Mutex
	published []byte
	err       error
	onPublish func(queue.Request)
}

func (c *capturePublisher) Publish(ctx context.Context, req queue.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.published = append([]byte(nil), data...)
	cb := c.onPublish
	c.mu.Unlock()
	if cb != nil {
		cb(req)
	}
	return nil
}

// lastPublished returns the bytes of the most recent published request.
func (c *capturePublisher) lastPublished() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.published...)
}

// fail makes every Publish call return err from now on.
func (c *capturePublisher) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// openTestStore opens a fresh store in a temp dir, the same way the worker
// and store packages' tests do.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newWorkRequestAPIServer builds the minimal real httpapi.Server that
// serves GET /api/requests/{request_id} — the endpoint every RESULTS
// waiter now reads — over st. Static is a NotFoundHandler because the mux
// registers "/" against it and nothing here serves the web UI.
func newWorkRequestAPIServer(st *store.Store) *httpapi.Server {
	return &httpapi.Server{Store: st, Static: http.NotFoundHandler()}
}

// newIntegrationService wires a Service whose HTTP calls land on a real
// httpapi.Server backed by a fresh store — the shape `harness serve` runs —
// with a fresh Registry and a capturePublisher per test. The MCP service
// reads work requests over loopback HTTP exactly as it does in production;
// the store is where a test seeds the rows the tools read (the pool creates
// them at claim time in production). No broker or queue seam is involved
// anywhere.
func newIntegrationService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	return newIntegrationServiceOverStore(t, st), st
}

// newIntegrationServiceOverStore is newIntegrationService with a
// caller-supplied store, so a test can seed rows before the service exists
// (the "fresh process collecting a long-finished result" shape).
func newIntegrationServiceOverStore(t *testing.T, st *store.Store) *Service {
	t.Helper()
	apiSrv := httptest.NewServer(newWorkRequestAPIServer(st).Handler())
	t.Cleanup(apiSrv.Close)
	return &Service{
		Publisher: &capturePublisher{},
		Cfg: config.MCPConfig{
			PermissionCeiling: "full",
			FlashModel:        "test-flash",
			AcceptedWaitMS:    300,
			HarnessBaseURL:    apiSrv.URL,
			HarnessPublicURL:  "http://127.0.0.1:8080",
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Registry:   NewRegistry(),
		Store:      st,
	}
}

// seedWorkRequest creates a work_requests row the way the worker pool would
// have left it: claimed, with sessionID attached (unless empty), and
// finished with res — the durable state a collect now reads instead of a
// published final result.
func seedWorkRequest(t *testing.T, st *store.Store, requestID, sessionID string, res queue.Result) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.ClaimWorkRequest(ctx, requestID, 1, time.Now()); err != nil {
		t.Fatalf("claim %s: %v", requestID, err)
	}
	if sessionID != "" {
		if err := st.SetWorkRequestSession(ctx, requestID, sessionID); err != nil {
			t.Fatalf("attach session %s: %v", requestID, err)
		}
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := st.FinishWorkRequest(ctx, requestID, sessionID, res.Status, data, res.FinishedAt)
	if err != nil || !matched {
		t.Fatalf("finish %s: matched=%v err=%v", requestID, matched, err)
	}
}

// TestHandleLaunchQueuedOutcome is the first of the three launch outcomes
// docs/DESIGN.md describes: published successfully, nothing claims it
// within the wait, which is normal rather than an error. The real API
// server answers 404 for the request — the "pool has not claimed it"
// signal — so the launch reports queued.
func TestHandleLaunchQueuedOutcome(t *testing.T) {
	svc, _ := newIntegrationService(t)

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "Queued test run",
		Description:    "queued test", Prompt: "do nothing", Repos: testLaunchRepos(),
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	lo, ok := res.StructuredContent.(launchOutput)
	if !ok {
		t.Fatalf("expected launchOutput structured content, got %T", res.StructuredContent)
	}
	if lo.Status != "queued" {
		t.Fatalf("expected status queued (nothing has claimed the request), got %q", lo.Status)
	}
	if lo.SessionID != "" {
		t.Fatalf("expected no session id for a queued request, got %q", lo.SessionID)
	}

	records := svc.Registry.list()
	if len(records) != 1 || records[0].RequestID != lo.RequestID || records[0].Status != "queued" {
		t.Fatalf("expected the registry to record the queued launch, got %+v", records)
	}
}

// TestHandleLaunchCarriesProvenance launches with a job type and a parent
// agent set and captures the published work request, asserting the three
// provenance fields reach the wire unchanged.
func TestHandleLaunchCarriesProvenance(t *testing.T) {
	svc, _ := newIntegrationService(t)
	captured := svc.Publisher.(*capturePublisher)

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "Provenance test run",
		Description:    "provenance test", Prompt: "do something", Repos: testLaunchRepos(),
		JobType:         agentmeta.JobTypeOrchestration,
		ParentAgentType: "claude-code",
		ParentAgentID:   "sess-parent-1",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}

	var got queue.Request
	if err := json.Unmarshal(captured.lastPublished(), &got); err != nil {
		t.Fatalf("parse published work request: %v", err)
	}
	if got.JobType != agentmeta.JobTypeOrchestration {
		t.Fatalf("expected job type %q on the published request, got %q", agentmeta.JobTypeOrchestration, got.JobType)
	}
	if got.ParentAgentType != "claude-code" || got.ParentAgentID != "sess-parent-1" {
		t.Fatalf("expected parent agent claude-code/sess-parent-1 on the published request, got %q/%q", got.ParentAgentType, got.ParentAgentID)
	}
	if got.ParentIsUser {
		t.Fatal("expected parent_is_user to be false on the published request: an MCP launch is never a person starting the run")
	}
}

// TestHandleLaunchRunningOutcome is the second outcome: the pool claims the
// request within the wait window, so the launch reports "running" with a
// session id and a transcript URL. No real worker exists in this test, so
// the capturePublisher seeds the work_requests row — claim plus session
// attach, the two writes the pool makes before the accepted message used to
// be published — the moment the publish lands, timed off the request the
// publish carries.
func TestHandleLaunchRunningOutcome(t *testing.T) {
	svc, st := newIntegrationService(t)
	svc.Cfg.AcceptedWaitMS = 3000
	captured := svc.Publisher.(*capturePublisher)

	fakeSessionID := "sess-fake-running"
	captured.onPublish = func(req queue.Request) {
		if _, err := st.ClaimWorkRequest(context.Background(), req.RequestID, 1, time.Now()); err != nil {
			t.Fatalf("simulate claim: %v", err)
		}
		if err := st.SetWorkRequestSession(context.Background(), req.RequestID, fakeSessionID); err != nil {
			t.Fatalf("simulate session attach: %v", err)
		}
	}

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "Running test run",
		Description:    "running test", Prompt: "do something", Repos: testLaunchRepos(),
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	lo := res.StructuredContent.(launchOutput)
	if lo.Status != "running" {
		t.Fatalf("expected status running once the pool claimed the request, got %q", lo.Status)
	}
	if lo.SessionID != fakeSessionID {
		t.Fatalf("expected session id %q, got %q", fakeSessionID, lo.SessionID)
	}
	if lo.TranscriptURL == "" {
		t.Fatal("expected a transcript URL once a session id is known")
	}

	records := svc.Registry.list()
	if len(records) != 1 || records[0].Status != "running" || records[0].SessionID != fakeSessionID {
		t.Fatalf("expected the registry to reflect the running state, got %+v", records)
	}
}

// TestHandleLaunchPublishFailure is the third outcome, the only genuine
// error: the publish itself fails, so the launch reports a tool error.
func TestHandleLaunchPublishFailure(t *testing.T) {
	svc, _ := newIntegrationService(t)
	svc.Publisher.(*capturePublisher).fail(errPublishTest)

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "Publish failure test",
		Description:    "should fail", Prompt: "do something", Repos: testLaunchRepos(),
	})
	if err != nil {
		t.Fatalf("unexpected protocol error (should be a tool error, not a protocol one): %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result once the publish fails")
	}
	if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "publish work request") {
		t.Fatalf("expected the refusal to name the publish, got: %s", res.Content[0].(*mcpsdk.TextContent).Text)
	}
}

// errPublishTest is the failure the capturePublisher injects for
// TestHandleLaunchPublishFailure.
var errPublishTest = context.DeadlineExceeded

// TestHandleCollectAfterTheFact is the requirement docs/DESIGN.md calls out
// explicitly: a collect works long after the fact and from a fresh process,
// not just one that was listening when the result was written. The
// work_requests row is seeded before the service exists, standing in for a
// result written by a process this one never saw — the durable row has no
// retention window, so this is strictly easier than the old 7-day stream.
func TestHandleCollectAfterTheFact(t *testing.T) {
	st := openTestStore(t)

	requestID := "collect-after-the-fact-" + time.Now().Format("150405.000000000")
	final := queue.Result{
		RequestID: requestID, SessionID: "sess-persisted", Status: queue.StatusOK,
		Text: "the answer", SubTurns: 3, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	}
	seedWorkRequest(t, st, requestID, "sess-persisted", final)

	// A service built after the result already exists, standing in for "a
	// fresh process" that was never listening when the result was written.
	svc := newIntegrationServiceOverStore(t, st)

	res, out, err := svc.handleCollect(context.Background(), nil, collectInput{RequestID: requestID})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if out != nil {
		t.Fatalf("expected no structured content (this result never called Complete), got %v", out)
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	for _, want := range []string{"the answer", "status: ok", "sub_turns: 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the rendered text to contain %q, got: %s", want, text)
		}
	}
}

// TestHandleCollectIsRepeatable calls deepseek_result twice against the
// same persisted final result and confirms neither call has a side effect
// on the run itself (it must be safe to poll).
func TestHandleCollectIsRepeatable(t *testing.T) {
	svc, st := newIntegrationService(t)

	requestID := "collect-repeat-" + time.Now().Format("150405.000000000")
	final := queue.Result{RequestID: requestID, SessionID: "sess-repeat", Status: queue.StatusOK, Text: "done", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	seedWorkRequest(t, st, requestID, "sess-repeat", final)

	for i := 0; i < 2; i++ {
		res, _, err := svc.handleCollect(context.Background(), nil, collectInput{RequestID: requestID})
		if err != nil {
			t.Fatalf("call %d: unexpected protocol error: %v", i, err)
		}
		if res.IsError {
			t.Fatalf("call %d: unexpected error result: %+v", i, res.Content)
		}
		text := res.Content[0].(*mcpsdk.TextContent).Text
		if !strings.Contains(text, "done") {
			t.Fatalf("call %d: expected the stored result text, got: %s", i, text)
		}
	}
}

// TestHandleCollectPendingWhenNothingSeen is deepseek_result's answer for a
// request_id with no final result: a short pending line pointing at
// deepseek_status, never an error. Both shapes are covered: a request the
// pool has not claimed yet (404) and a claimed row whose result is still
// empty (the run is in flight).
func TestHandleCollectPendingWhenNothingSeen(t *testing.T) {
	t.Run("no row yet", func(t *testing.T) {
		svc, _ := newIntegrationService(t)
		res, _, err := svc.handleCollect(context.Background(), nil, collectInput{RequestID: "never-published"})
		if err != nil {
			t.Fatalf("unexpected protocol error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected error result for an unfinished request: %+v", res.Content)
		}
		text := res.Content[0].(*mcpsdk.TextContent).Text
		if !strings.Contains(text, "deepseek_status") {
			t.Fatalf("expected the pending line to point at deepseek_status, got: %s", text)
		}
	})
	t.Run("claimed but not finished", func(t *testing.T) {
		svc, st := newIntegrationService(t)
		ctx := context.Background()
		if _, err := st.ClaimWorkRequest(ctx, "in-flight", 1, time.Now()); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := st.SetWorkRequestSession(ctx, "in-flight", "sess-running"); err != nil {
			t.Fatalf("attach session: %v", err)
		}
		res, _, err := svc.handleCollect(context.Background(), nil, collectInput{RequestID: "in-flight"})
		if err != nil {
			t.Fatalf("unexpected protocol error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected error result for an in-flight request: %+v", res.Content)
		}
		text := res.Content[0].(*mcpsdk.TextContent).Text
		if !strings.Contains(text, "deepseek_status") {
			t.Fatalf("expected the pending line to point at deepseek_status, got: %s", text)
		}
	})
}
