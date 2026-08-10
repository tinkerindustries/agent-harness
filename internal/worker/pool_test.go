package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// testNATSURL and connectOrSkip mirror internal/queue's test helpers: the
// broker in docker-compose.test.yml, skipped when unreachable so
// `go test ./...` passes without Docker. NATS_URL is ignored on purpose;
// it names the deployment's broker, whose running pool would consume these
// requests and reject them against its own workspace roots.
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
		t.Skipf("no test NATS JetStream server reachable at %s (scripts/test.sh): %v", testNATSURL(), err)
	}
	t.Cleanup(nc.Close)
	return nc, js
}

func uniqueID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// testHarness wires a Pool to a real local JetStream server and a fake
// DeepSeek HTTP server, isolated per test by a fresh store and a fresh
// durable consumer state (streams are cleaned up in t.Cleanup).
type testHarness struct {
	pool *Pool
	js   jetstream.JetStream
	root string
	hits *hitCounter
}

type hitCounter struct {
	mu sync.Mutex
	n  int
}

func (h *hitCounter) inc() {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
}

func (h *hitCounter) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// plainAnswerServer answers every request (streaming or not) with answer
// and no tool calls, ending the run at "ok" after a single sub-turn. delay
// stalls the response so a test can keep a run "in flight" long enough to
// race a duplicate request against it.
func plainAnswerServer(t *testing.T, answer string, delay time.Duration, hits *hitCounter) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.inc()
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		if !probe.Stream {
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: answer}, FinishReason: deepseek.FinishStop}},
				Usage:   &deepseek.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(c deepseek.ChatCompletionChunk) {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		content := answer
		writeChunk(deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Role: "assistant", Content: &content}}},
		})
		finish := deepseek.FinishStop
		writeChunk(deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: &finish}},
			Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func newTestHarness(t *testing.T, serverURL string, poolSize int) *testHarness {
	t.Helper()
	nc, js := connectOrSkip(t)
	_ = nc

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	consumer, err := queue.EnsureStreams(ctx, js, poolSize, queue.DefaultResultsMaxAge, queue.DefaultMaxDeliveryAttempts)
	if err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	t.Cleanup(func() {
		js.DeleteStream(context.Background(), queue.StreamWork)
		js.DeleteStream(context.Background(), queue.StreamResults)
	})

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	root := filepath.Join(dir, "roots")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	runner := &session.Runner{
		Store:      st,
		Client:     deepseek.NewClient(serverURL, "test-key"),
		Prices:     testPrices(),
		FlashModel: "test-model",
	}

	pool := &Pool{
		Store:             st,
		Runner:            runner,
		JS:                js,
		Consumer:          consumer,
		WorkspaceRoot:     resolvedRoot,
		PrepareWorkspace:  fakePrepareWorkspace,
		DefaultModel:      "test-model",
		DefaultEffort:     deepseek.EffortHigh,
		DefaultThinking:   true,
		DefaultMaxTokens:  4000,
		DefaultDeadline:   20 * time.Second,
		PriceTableDate:    "2026-08-09",
		Size:              poolSize,
		HeartbeatInterval: 30 * time.Millisecond,
		LeasePollInterval: 50 * time.Millisecond,
		RetryLaterDelay:   300 * time.Millisecond,
	}

	return &testHarness{pool: pool, js: js, root: resolvedRoot}
}

func testPrices() *pricing.Table {
	return &pricing.Table{
		CapturedAt: "2026-08-09",
		Models: map[string]pricing.ModelPrices{
			"test-model": {InputCacheHitPerMillionUSD: 0.003625, InputCacheMissPerMillionUSD: 0.435, OutputPerMillionUSD: 0.87},
		},
	}
}

// startPool runs the pool in the background and returns a stop func that
// cancels it and waits for Run to actually return before continuing. A
// bare `go h.pool.Run(ctx)` plus `defer cancel()` races: cancel only asks
// Run to stop, and t.Cleanup closes the store before Run's in-flight
// handlers are guaranteed to have released it.
func (h *testHarness) startPool(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.pool.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// fakePrepareWorkspace stands in for workspace.Prepare so these tests drive
// the pool without cloning anything over the network. The real clone is
// covered in internal/workspace.
func fakePrepareWorkspace(_ context.Context, root, sessionID string, _ []queue.Repo) (string, error) {
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// testRepos is the minimum a request needs to pass validation. What it
// names never gets cloned: fakePrepareWorkspace ignores it.
func testRepos() []queue.Repo {
	return []queue.Repo{{URL: "https://example.com/org/app.git"}}
}

func (h *testHarness) publish(t *testing.T, req queue.Request) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.js.Publish(ctx, queue.RequestSubject(req.RequestID), data); err != nil {
		t.Fatalf("publish %s: %v", req.RequestID, err)
	}
}

func (h *testHarness) publishRaw(t *testing.T, subjectToken string, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.js.Publish(ctx, queue.RequestSubject(subjectToken), data); err != nil {
		t.Fatalf("publish raw %s: %v", subjectToken, err)
	}
}

// fetchFinalResult waits up to timeout for a final result to appear on the
// RESULTS stream for requestID, decoded into a queue.Result.
func (h *testHarness) fetchFinalResult(t *testing.T, requestID string, timeout time.Duration) queue.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	consumer, err := h.js.OrderedConsumer(ctx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.FinalSubject(requestID)},
	})
	if err != nil {
		t.Fatalf("ordered consumer for %s: %v", requestID, err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(timeout))
	if err != nil {
		t.Fatalf("fetch final result for %s: %v", requestID, err)
	}
	for msg := range batch.Messages() {
		var res queue.Result
		if err := json.Unmarshal(msg.Data(), &res); err != nil {
			t.Fatalf("decode result for %s: %v", requestID, err)
		}
		return res
	}
	t.Fatalf("no final result arrived for %s within %s", requestID, timeout)
	return queue.Result{}
}

// countFinalResults counts however many final-result messages arrived for
// requestID within timeout, used to prove a duplicate request_id produced
// exactly one.
func (h *testHarness) countFinalResults(t *testing.T, requestID string, timeout time.Duration) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	consumer, err := h.js.OrderedConsumer(ctx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.FinalSubject(requestID)},
	})
	if err != nil {
		t.Fatalf("ordered consumer for %s: %v", requestID, err)
	}
	batch, err := consumer.Fetch(10, jetstream.FetchMaxWait(timeout))
	if err != nil {
		t.Fatalf("fetch for %s: %v", requestID, err)
	}
	n := 0
	for range batch.Messages() {
		n++
	}
	return n
}

// TestPoolFourConcurrentRequests proves the pool runs concurrently:
// publish four requests at once and get four results back with correct
// usage figures.
func TestPoolFourConcurrentRequests(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "all done", 0, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)
	defer h.startPool(t)()

	const n = 4
	requestIDs := make([]string, n)
	for i := 0; i < n; i++ {
		requestIDs[i] = uniqueID("req-concurrent")
		h.publish(t, queue.Request{
			PermissionMode: "full",
			RequestID:      requestIDs[i], Prompt: fmt.Sprintf("task %d", i), Repos: testRepos(),
		})
	}

	var wg sync.WaitGroup
	results := make([]queue.Result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.fetchFinalResult(t, requestIDs[i], 15*time.Second)
		}(i)
	}
	wg.Wait()

	for i, res := range results {
		if res.Status != queue.StatusOK {
			t.Fatalf("request %d: expected status ok, got %+v", i, res)
		}
		if res.Text != "all done" {
			t.Fatalf("request %d: unexpected text %q", i, res.Text)
		}
		if res.Usage == nil || res.Usage.CacheHitTokens != 100 || res.Usage.CacheMissTokens != 100 {
			t.Fatalf("request %d: unexpected usage %+v", i, res.Usage)
		}
		if res.SessionID == "" {
			t.Fatalf("request %d: expected a session id", i)
		}
	}
}

// TestPoolDuplicateRequestIDRunsOnce proves request_id is an idempotency
// key: publish the same one twice and get one run and one result. The
// fake server stalls so the second publish lands while the first is
// genuinely still in flight, exercising the retry-later path rather than
// the takeover path.
func TestPoolDuplicateRequestIDRunsOnce(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "done once", 700*time.Millisecond, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)
	defer h.startPool(t)()

	requestID := uniqueID("req-dup")
	req := queue.Request{RequestID: requestID, Prompt: "do it once", Repos: testRepos(), PermissionMode: "full"}

	h.publish(t, req)
	time.Sleep(150 * time.Millisecond) // let the first attempt claim the row before the duplicate arrives
	h.publish(t, req)

	res := h.fetchFinalResult(t, requestID, 15*time.Second)
	if res.Status != queue.StatusOK || res.Text != "done once" {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Give any (incorrect) second run a moment it would need to reach the
	// fake server, then confirm it never did.
	time.Sleep(1 * time.Second)
	if hits.count() != 1 {
		t.Fatalf("expected exactly 1 session to have run against the fake server, got %d", hits.count())
	}

	if n := h.countFinalResults(t, requestID, 2*time.Second); n != 1 {
		t.Fatalf("expected exactly 1 final result on the RESULTS stream, got %d", n)
	}
}

// TestPoolCarriesProvenanceFromRequestToSession proves the request's job
// type and parent agent reach the RunOptions the pool builds: the session
// row that comes out of the run must carry them.
func TestPoolCarriesProvenanceFromRequestToSession(t *testing.T) {
	srv := plainAnswerServer(t, "done", 0, nil)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)
	defer h.startPool(t)()

	requestID := uniqueID("req-provenance")
	h.publish(t, queue.Request{
		RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full",
		JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1",
	})

	res := h.fetchFinalResult(t, requestID, 15*time.Second)
	if res.Status != queue.StatusOK {
		t.Fatalf("unexpected result: %+v", res)
	}
	sess, err := h.pool.Store.GetSession(context.Background(), res.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.JobType != agentmeta.JobTypeOrchestration ||
		sess.ParentAgentType != "orchestrator" || sess.ParentAgentID != "orch-1" {
		t.Fatalf("unexpected provenance on the session row: %+v", sess)
	}
}

// TestPoolMalformedRequestTermsWithoutRunning is the request and result
// validation Term path: a request that fails validation gets a failed
// result and is never run.
func TestPoolMalformedRequestTermsWithoutRunning(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "should never run", 0, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)
	defer h.startPool(t)()

	requestID := uniqueID("req-invalid")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "go",
		Repos: []queue.Repo{{URL: "ext::sh -c 'touch /tmp/pwned'"}}, PermissionMode: "full"})

	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusFailed {
		t.Fatalf("expected status failed, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "invalid_request" {
		t.Fatalf("expected an invalid_request error, got %+v", res.Error)
	}

	time.Sleep(300 * time.Millisecond)
	if hits.count() != 0 {
		t.Fatalf("expected the fake server never to be called for an invalid request, got %d hits", hits.count())
	}
}

// TestPoolHandleSpentRequestClosesSessionFailsAndTerms drives Pool.handle
// with a fake jetstream.Msg reporting a redelivery, against a work_requests
// row shaped like one a dead process abandoned mid-run: the row carries the
// session id, so the request is single-use and must never run again. The
// spent-request path has to close the abandoned session (which otherwise sat
// running in the list forever), publish a failed result naming the reason
// and the retry, and Term the message. It covers the routing a kill-and-
// restart exercises, without waiting on the real 60s AckWait.
func TestPoolHandleSpentRequestClosesSessionFailsAndTerms(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "must never run", 0, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)

	requestID := uniqueID("req-spent")

	ctx := context.Background()
	if _, err := h.pool.Store.ClaimWorkRequest(ctx, requestID, 1, time.Now()); err != nil {
		t.Fatalf("simulate original claim: %v", err)
	}
	if err := h.pool.Store.SetWorkRequestSession(ctx, requestID, "sess-abandoned-fake"); err != nil {
		t.Fatalf("attach abandoned session: %v", err)
	}
	// The abandoned attempt's session row exists, running, with no events —
	// the abandoned-during-preparation shape that passes the idle check.
	mustCreatePoolSession(t, h, "sess-abandoned-fake")

	reqBody, err := json.Marshal(queue.Request{RequestID: requestID, Prompt: "continue", Repos: testRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	msg := &fakeMsg{data: reqBody, numDelivered: 2}
	h.pool.handle(msg)

	if !msg.wasTermed() {
		t.Fatalf("expected the spent request's message to be termed; acked=%v termed=%v nakked=%v", msg.acked, msg.termed, msg.nakked)
	}

	// The session is closed, not left running forever.
	sess, err := h.pool.Store.GetSession(ctx, "sess-abandoned-fake")
	if err != nil {
		t.Fatalf("get abandoned session: %v", err)
	}
	if sess.Status == store.StatusRunning {
		t.Fatal("expected the abandoned session to be closed, not left running")
	}
	if sess.FinishedAt == nil {
		t.Fatal("expected the abandoned session to carry a finished_at")
	}

	// The work_requests row is terminal with a failed result naming the
	// reason and the retry.
	row, err := h.pool.Store.GetWorkRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != queue.StatusFailed {
		t.Fatalf("expected the row to be recorded failed, got %q", row.Status)
	}
	if !strings.Contains(string(row.Result), "abandoned") ||
		!strings.Contains(string(row.Result), "single-use") ||
		!strings.Contains(string(row.Result), "new request_id") {
		t.Fatalf("expected the stored result to name the reason and the retry, got %s", row.Result)
	}

	// The caller gets the failed result on the RESULTS stream.
	res := h.fetchFinalResult(t, requestID, 5*time.Second)
	if res.Status != queue.StatusFailed {
		t.Fatalf("expected a failed result on the stream, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "abandoned" {
		t.Fatalf("expected error code abandoned, got %+v", res.Error)
	}
	if res.SessionID != "sess-abandoned-fake" {
		t.Fatalf("expected the result to carry the abandoned session id, got %q", res.SessionID)
	}

	// The fake DeepSeek server was never hit: nothing re-ran.
	if hits.count() != 0 {
		t.Fatalf("expected no session to have run for a spent request, got %d hits", hits.count())
	}
}

// TestPoolHandleSpentRequestStillActiveLeavesRowAlone is the false-redelivery
// guard: a redelivery whose abandoned session is actually still live (its
// most recent event is newer than the idle threshold) must not be closed.
// The message falls through to the duplicate-handling path — which polls
// until this request's own deadline, then naks for a later delivery — and
// both rows stay exactly as they were.
func TestPoolHandleSpentRequestStillActiveLeavesRowAlone(t *testing.T) {
	hits := &hitCounter{}
	srv := plainAnswerServer(t, "must never run", 0, hits)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)

	requestID := uniqueID("req-live")

	ctx := context.Background()
	if _, err := h.pool.Store.ClaimWorkRequest(ctx, requestID, 1, time.Now()); err != nil {
		t.Fatalf("simulate original claim: %v", err)
	}
	if err := h.pool.Store.SetWorkRequestSession(ctx, requestID, "sess-still-live"); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	// The original attempt is alive: its session row exists, running, and has
	// a just-appended event, which is newer than the idle threshold.
	mustCreatePoolSession(t, h, "sess-still-live")
	if _, err := h.pool.Store.AppendEvents(ctx, "sess-still-live", []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// A short deadline so the wait-for-resolution fallback gives up quickly
	// and naks for a later delivery instead of running.
	reqBody, err := json.Marshal(queue.Request{
		RequestID: requestID, Prompt: "continue", Repos: testRepos(), PermissionMode: "full",
		DeadlineMS: 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &fakeMsg{data: reqBody, numDelivered: 2}
	h.pool.handle(msg)

	if !msg.wasNakked() {
		t.Fatalf("expected the duplicate to be nakked for a later delivery after the wait deadline; acked=%v termed=%v nakked=%v",
			msg.acked, msg.termed, msg.nakked)
	}
	if msg.wasTermed() {
		t.Fatal("a live attempt must never be failed and termed")
	}

	// The live session is untouched — the whole point of the idle check.
	sess, err := h.pool.Store.GetSession(ctx, "sess-still-live")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.StatusRunning {
		t.Fatalf("expected the live session to stay running, got %q", sess.Status)
	}

	// The work_requests row is untouched too: still running under the live
	// session, and no result was published for a request that is still alive.
	row, err := h.pool.Store.GetWorkRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != store.WorkRequestStatusRunning || row.SessionID != "sess-still-live" {
		t.Fatalf("expected the row to stay running under the live session, got %+v", row)
	}
	if hits.count() != 0 {
		t.Fatalf("expected no session to have run, got %d hits", hits.count())
	}
}

// mustCreatePoolSession creates a running session row for the pool tests,
// the minimum the store needs.
func mustCreatePoolSession(t *testing.T, h *testHarness, id string) {
	t.Helper()
	err := h.pool.Store.CreateSession(context.Background(), store.Session{
		ID:             id,
		Model:          "test-model",
		Effort:         "high",
		Workspace:      filepath.Join(h.root, id),
		PermissionMode: "full",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// fakeMsg implements jetstream.Msg without a broker, for driving
// Pool.handle directly with a controlled delivery count.
type fakeMsg struct {
	data         []byte
	numDelivered uint64

	mu         sync.Mutex
	acked      bool
	termed     bool
	nakked     bool
	inProgress int
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.numDelivered}, nil
}
func (m *fakeMsg) Data() []byte         { return m.data }
func (m *fakeMsg) Headers() nats.Header { return nil }
func (m *fakeMsg) Subject() string      { return "test.subject" }
func (m *fakeMsg) Reply() string        { return "" }

func (m *fakeMsg) Ack() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked = true
	return nil
}
func (m *fakeMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakeMsg) Nak() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nakked = true
	return nil
}
func (m *fakeMsg) NakWithDelay(time.Duration) error { return m.Nak() }
func (m *fakeMsg) InProgress() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inProgress++
	return nil
}
func (m *fakeMsg) Term() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.termed = true
	return nil
}
func (m *fakeMsg) TermWithReason(string) error { return m.Term() }

func (m *fakeMsg) wasAcked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked
}

func (m *fakeMsg) wasNakked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nakked
}

func (m *fakeMsg) wasTermed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.termed
}

var _ jetstream.Msg = (*fakeMsg)(nil)

// alwaysToolCallServer answers every request with the same tool call, so a
// run never reaches a no-tool-call response and exhausts its sub-turn budget.
func alwaysToolCallServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(c deepseek.ChatCompletionChunk) {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		writeChunk(deepseek.ChatCompletionChunk{Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
			Role:      "assistant",
			ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call-1", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TodoWrite", Arguments: `{"todos":[]}`}}},
		}}}})
		finish := deepseek.FinishToolCalls
		writeChunk(deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: &finish}},
			Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// A run that exhausts max_sub_turns must not report itself ok. The requester
// otherwise cannot tell a finished task from one that stopped partway.
func TestPoolMaxSubTurnsIsNotReportedAsOK(t *testing.T) {
	srv := alwaysToolCallServer(t)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Stop the pool and let it drain before the test returns, so t.Cleanup
	// does not close the store under an in-flight lease release.
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		h.pool.Run(ctx)
	}()
	defer func() {
		cancel()
		<-poolDone
	}()

	requestID := uniqueID("maxturns")
	h.publish(t, queue.Request{
		PermissionMode: "full",
		RequestID:      requestID,
		Prompt:         "loop forever",
		Repos:          testRepos(),
		MaxSubTurns:    2,
	})

	res := h.fetchFinalResult(t, requestID, 25*time.Second)
	if res.Status == queue.StatusOK {
		t.Fatalf("a run that exhausted max_sub_turns reported status ok; requester cannot tell it from a completed run")
	}
	if res.Status != queue.StatusTimeout {
		t.Fatalf("status = %q, want %q", res.Status, queue.StatusTimeout)
	}
	if res.Error == nil || res.Error.Code != "max_sub_turns" {
		t.Fatalf("error = %+v, want code max_sub_turns", res.Error)
	}
}

// insufficientBalanceServer answers every chat completion with a 402, the
// shape DeepSeek returns for an exhausted account (internal/deepseek's
// parseAPIError envelope).
func insufficientBalanceServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance","type":"insufficient_balance_error"}}`)
	}))
}

// TestPoolHaltsOnInsufficientBalance pins the balance behaviour: a 402 is
// surfaced as an empty account and stops the pool
// rather than failing each queued request in turn (docs/DESIGN.md §4.5).
// The request that hit the 402 is left unacked (no final result published)
// so it redelivers once the pool is restarted with balance restored, and a
// second request queued behind it is never even pulled.
func TestPoolHaltsOnInsufficientBalance(t *testing.T) {
	srv := insufficientBalanceServer(t)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 2)
	defer h.startPool(t)()

	requestID := uniqueID("req-402")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "task", Repos: testRepos(), PermissionMode: "full"})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if halted, reason := h.pool.Halted(); halted {
			if !strings.Contains(reason, "402") {
				t.Fatalf("expected the halt reason to mention 402, got %q", reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pool never halted after a 402")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The halted request itself must not have a terminal result: it was
	// left unacked for redelivery, not marked failed and finished.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	consumer, err := h.js.OrderedConsumer(ctx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.FinalSubject(requestID)},
	})
	if err != nil {
		t.Fatalf("ordered consumer: %v", err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for range batch.Messages() {
		t.Fatal("expected no final result to be published for a request that hit an empty account")
	}

	// A second request, published after the halt, must never be picked up
	// either — the pool stopped pulling, it did not just fail this one.
	second := uniqueID("req-402-second")
	h.publish(t, queue.Request{RequestID: second, Prompt: "task", Repos: testRepos(), PermissionMode: "full"})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	consumer2, err := h.js.OrderedConsumer(ctx2, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.FinalSubject(second)},
	})
	if err != nil {
		t.Fatalf("ordered consumer: %v", err)
	}
	batch2, err := consumer2.Fetch(1, jetstream.FetchMaxWait(1500*time.Millisecond))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for range batch2.Messages() {
		t.Fatal("expected a request published after the halt never to be pulled, let alone finished")
	}
}

// gaveUpCompleteServer answers with a single call to Complete carrying
// status "gave_up", ending the run in one sub-turn.
func gaveUpCompleteServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(c deepseek.ChatCompletionChunk) {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		writeChunk(deepseek.ChatCompletionChunk{Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
			Role:      "assistant",
			ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call-1", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"could not do it","status":"gave_up"}`}}},
		}}}})
		finish := deepseek.FinishToolCalls
		writeChunk(deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: &finish}},
			Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// TestPoolGaveUpPropagatesCompleteStatus is the wire half of the gave_up
// propagation fix (docs/DESIGN.md §4.10): a run whose model called Complete
// with status "gave_up" must carry that on the published queue.Result, not
// just in the session's own event log. Status stays "ok" — gave_up is an
// additive field describing how the model characterised finishing, not a
// fifth terminal status.
func TestPoolGaveUpPropagatesCompleteStatus(t *testing.T) {
	srv := gaveUpCompleteServer(t)
	defer srv.Close()

	h := newTestHarness(t, srv.URL, 4)
	defer h.startPool(t)()

	requestID := uniqueID("req-gave-up")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do the impossible task", Repos: testRepos(), PermissionMode: "full"})

	res := h.fetchFinalResult(t, requestID, 15*time.Second)
	if res.Status != queue.StatusOK {
		t.Fatalf("expected status ok (gave_up is not a new terminal status), got %+v", res)
	}
	if res.CompleteStatus != "gave_up" {
		t.Fatalf("expected complete_status %q on the wire result, got %q", "gave_up", res.CompleteStatus)
	}
	if res.Text != "" {
		t.Fatalf("expected no final assistant text (the run ended on a tool call), got %q", res.Text)
	}
}

// TestPoolLastDeliveryPublishesFailedRatherThanVanishing pins the ceiling
// docs/DESIGN.md §4.10 asks for: "On the last delivery attempt, publish
// failed and Term." The consumer's MaxDeliver stops redelivering after N
// attempts, so without this the request would simply stop existing and a
// caller polling for its result would wait forever.
func TestPoolLastDeliveryPublishesFailedRatherThanVanishing(t *testing.T) {
	srv := plainAnswerServer(t, "unused", 0, &hitCounter{})
	defer srv.Close()
	h := newTestHarness(t, srv.URL, 4)
	h.pool.MaxDeliveryAttempts = 3

	ctx := context.Background()
	requestID := uniqueID("req-exhausted")
	if _, err := h.pool.Store.ClaimWorkRequest(ctx, requestID, 1, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Not the last attempt: defer to a later delivery, as before.
	early := &fakeMsg{numDelivered: 2}
	h.pool.retryLater(early, requestID, "claim_failed", "transient")
	if !early.wasNakked() {
		t.Fatalf("delivery 2 of 3 should nak for a later attempt; acked=%v termed=%v nakked=%v",
			early.acked, early.termed, early.nakked)
	}

	// The last attempt: there is no later delivery, so the caller has to be
	// told something.
	last := &fakeMsg{numDelivered: 3}
	h.pool.retryLater(last, requestID, "claim_failed", "still failing on the last attempt")
	if last.wasNakked() {
		t.Fatal("the last delivery must not nak: nothing would redeliver it and the request would vanish")
	}
	if !last.wasTermed() {
		t.Fatalf("expected the last delivery to be termed; acked=%v termed=%v", last.acked, last.termed)
	}

	row, err := h.pool.Store.GetWorkRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != queue.StatusFailed {
		t.Fatalf("expected the row to be recorded failed, got %q", row.Status)
	}
	if !strings.Contains(string(row.Result), "still failing on the last attempt") {
		t.Fatalf("expected the stored result to carry the reason, got %s", row.Result)
	}
}
