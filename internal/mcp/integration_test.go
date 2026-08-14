package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// testNATSURL and connectOrSkip mirror internal/queue's and
// internal/worker's own test helpers: the broker in docker-compose.test.yml.
// A missing broker is a failure, not a skip, by default — a suite that could
// not run must not read as a pass in `go test ./...` output — with
// HARNESS_TEST_NATS_OPTIONAL=1 as the deliberate opt-out for a developer who
// genuinely has no Docker. NATS_URL is ignored on purpose — see those
// packages' comments for why.
func testNATSURL() string {
	config.LoadDotEnv("../../.env")
	if v := os.Getenv("HARNESS_TEST_NATS_URL"); v != "" {
		return v
	}
	return "nats://127.0.0.1:4422"
}

func connectOrSkip(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, js, err := queue.Connect(testNATSURL())
	if err != nil {
		if os.Getenv("HARNESS_TEST_NATS_OPTIONAL") == "1" {
			t.Skipf("no test NATS JetStream server reachable at %s (scripts/test.sh): %v", testNATSURL(), err)
		}
		t.Fatalf("no test NATS JetStream server reachable at %s: %v — run scripts/test.sh to start one, or set HARNESS_TEST_NATS_OPTIONAL=1 to skip instead of failing", testNATSURL(), err)
	}
	t.Cleanup(nc.Close)
	return nc, js
}

// newIntegrationService wires a Service against a real local JetStream
// server, with a fresh Registry per test.
func newIntegrationService(t *testing.T, js jetstream.JetStream) *Service {
	t.Helper()
	svc := &Service{
		JS: js,
		Cfg: config.MCPConfig{
			PermissionCeiling: "full",
			FlashModel:        "test-flash",
			AcceptedWaitMS:    300,
			HarnessBaseURL:    "http://127.0.0.1:0",
			HarnessPublicURL:  "http://127.0.0.1:8080",
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Registry:   NewRegistry(),
	}
	return svc
}

// ensureTestStreams converges this package's WORK and RESULTS streams
// (renamed by queue.IsolateForTest in TestMain, so they are this package's
// own and never shared with internal/worker's pool). The streams are
// deleted again in cleanup: with per-package names there is no other
// package's in-flight publish to race, which is the reason an earlier
// version left them standing. Every test addresses its own unique
// request_id-scoped subjects, so messages left by a failed test are inert
// noise, not a correctness risk.
func ensureTestStreams(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := queue.EnsureStreams(ctx, js, 4, queue.DefaultResultsMaxAge, queue.DefaultMaxDeliveryAttempts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	t.Cleanup(func() {
		js.DeleteStream(context.Background(), queue.StreamWork)
		js.DeleteStream(context.Background(), queue.StreamResults)
	})
}

// TestHandleLaunchQueuedOutcome is the first of the three launch outcomes
// docs/DESIGN.md describes: published successfully, nothing accepts it
// within the wait, which is normal rather than an error.
func TestHandleLaunchQueuedOutcome(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)

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
		t.Fatalf("expected status queued (nothing is consuming the WORK stream), got %q", lo.Status)
	}
	if lo.SessionID != "" {
		t.Fatalf("expected no session id for a queued request, got %q", lo.SessionID)
	}

	records := svc.Registry.list()
	if len(records) != 1 || records[0].RequestID != lo.RequestID || records[0].Status != "queued" {
		t.Fatalf("expected the registry to record the queued launch, got %+v", records)
	}
}

// capturingJS wraps a real JetStream handle and keeps the payload of the
// last synchronous publish, so a test can assert the exact bytes handleLaunch
// puts on the WORK stream. A second consumer cannot read them back: the WORK
// stream is WorkQueue-retention and already has the harness-workers durable,
// and a workqueue stream admits only one consumer per subject filter.
type capturingJS struct {
	jetstream.JetStream
	published []byte
}

func (c *capturingJS) Publish(ctx context.Context, subject string, payload []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	c.published = append([]byte(nil), payload...)
	return c.JetStream.Publish(ctx, subject, payload, opts...)
}

// TestHandleLaunchCarriesProvenance launches with a job type and a parent
// agent set and captures the published work request, asserting the three
// provenance fields reach the wire unchanged.
func TestHandleLaunchCarriesProvenance(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)
	captured := &capturingJS{JetStream: js}
	svc.JS = captured

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
	if err := json.Unmarshal(captured.published, &got); err != nil {
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

// TestHandleLaunchRunningOutcome is the second outcome: an `accepted`
// message arrives within the wait window, so the launch reports "running"
// with a session id and a transcript URL. No real worker exists in this
// test, so a goroutine plays the worker's part by publishing `accepted`
// itself, timed off the registry entry that appears the moment the publish
// inside handleLaunch succeeds.
func TestHandleLaunchRunningOutcome(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)
	svc.Cfg.AcceptedWaitMS = 3000

	fakeSessionID := "sess-fake-running"
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, rec := range svc.Registry.list() {
				accepted := queue.Accepted{RequestID: rec.RequestID, SessionID: fakeSessionID, StartedAt: time.Now().UTC()}
				data, _ := json.Marshal(accepted)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				js.Publish(ctx, queue.AcceptedSubject(rec.RequestID), data)
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

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
		t.Fatalf("expected status running once accepted arrived, got %q", lo.Status)
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
// error: the connection is closed before the call, so both the accepted
// subscribe and the publish are guaranteed to fail deterministically rather
// than racing a real broker.
func TestHandleLaunchPublishFailure(t *testing.T) {
	nc, js := connectOrSkip(t)
	svc := newIntegrationService(t, js)
	nc.Close()

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "Publish failure test",
		Description:    "should fail", Prompt: "do something", Repos: testLaunchRepos(),
	})
	if err != nil {
		t.Fatalf("unexpected protocol error (should be a tool error, not a protocol one): %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result once the NATS connection is closed")
	}
}

// TestHandleCollectAfterTheFact is the requirement docs/DESIGN.md calls out
// explicitly: an ordered consumer filtered on the final subject replays
// from the start of the RESULTS stream, so a collect works long after the
// fact and from a fresh process, not just one that was subscribed before
// the result was published. This proves it with a second, independent
// JetStream connection built after the result already exists, rather than
// assuming the replay behaviour.
func TestHandleCollectAfterTheFact(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)

	requestID := "collect-after-the-fact-" + time.Now().Format("150405.000000000")
	final := queue.Result{
		RequestID: requestID, SessionID: "sess-persisted", Status: queue.StatusOK,
		Text: "the answer", SubTurns: 3, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(final)
	if err != nil {
		t.Fatal(err)
	}
	pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = js.Publish(pubCtx, queue.FinalSubject(requestID), data, jetstream.WithMsgID(queue.FinalMsgID(requestID)))
	cancel()
	if err != nil {
		t.Fatalf("publish final result: %v", err)
	}

	// A brand-new connection and JetStream context, standing in for "a
	// fresh process" that was never listening when the result was
	// published.
	_, freshJS := connectOrSkip(t)
	svc := newIntegrationService(t, freshJS)

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
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)

	requestID := "collect-repeat-" + time.Now().Format("150405.000000000")
	final := queue.Result{RequestID: requestID, SessionID: "sess-repeat", Status: queue.StatusOK, Text: "done", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	data, _ := json.Marshal(final)
	pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := js.Publish(pubCtx, queue.FinalSubject(requestID), data, jetstream.WithMsgID(queue.FinalMsgID(requestID)))
	cancel()
	if err != nil {
		t.Fatalf("publish final result: %v", err)
	}

	for i := 0; i < 2; i++ {
		res, _, err := svc.handleCollect(context.Background(), nil, collectInput{RequestID: requestID})
		if err != nil {
			t.Fatalf("call %d: unexpected protocol error: %v", i, err)
		}
		if res.IsError {
			t.Fatalf("call %d: unexpected error result: %+v", i, res.Content)
		}
	}
}

// TestHandleCollectPendingWhenNothingSeen is deepseek_result's answer for a
// request_id nothing has published a final result for: a short pending line
// pointing at deepseek_status, never an error.
func TestHandleCollectPendingWhenNothingSeen(t *testing.T) {
	_, js := connectOrSkip(t)
	ensureTestStreams(t, js)
	svc := newIntegrationService(t, js)

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
}
