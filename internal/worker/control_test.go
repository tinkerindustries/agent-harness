package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/queue"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The run-control tests drive the pool with fake runners standing in for
// *session.Runner, so a run can be wedged in a way no HTTP fake ever could:
// blocking on a channel the test controls, ignoring ctx entirely. Each fake
// creates the session row a real run would, because the store fences and the
// escalation's CancelRunningSession operate on it.

// ctxBlockingRunner is a healthy, cancellable run: it blocks until ctx is
// done and then reports the cancellation, the way a real run answers a
// cancelled runCtx at its next check point (docs/RUN-CONTROL.md "A healthy
// run and a wedged one are different problems").
type ctxBlockingRunner struct {
	store *store.Store
}

func (r *ctxBlockingRunner) Create(ctx context.Context, opts session.RunOptions) error {
	return fakeCreateSession(r.store, ctx, opts)
}

func (r *ctxBlockingRunner) FailSetup(context.Context, string, error) error { return nil }

// Resume satisfies the Runner seam. These three doubles exist to exercise
// stop, and stopping a resumed run is the same code path as stopping a fresh
// one (the registry entry is keyed on the session id either way), so none of
// them needs a distinct resume behaviour; the tests that cover the resume
// branch itself drive it through a runner of their own.
func (r *ctxBlockingRunner) Resume(context.Context, session.ResumeOptions) (*session.RunResult, error) {
	return nil, errors.New("resume: not used by this test double")
}

func (r *ctxBlockingRunner) Run(ctx context.Context, opts session.RunOptions) (*session.RunResult, error) {
	if err := fakePromoteSession(r.store, ctx, opts); err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// wedgingRunner is a genuinely wedged run: its first Run blocks on a channel
// the test controls, never consulting ctx again, so no context cancellation
// can unblock it — the same shape as the Bash call that backgrounds a
// process holding its output pipe (docs/RUN-CONTROL.md "Half one"). Later
// Runs succeed immediately, which is what lets a test prove a
// force-finished run's pool slot really came back.
type wedgingRunner struct {
	store *store.Store

	gate   chan struct{} // the first Run blocks here
	wedged chan struct{} // closed once the first Run is blocked
	calls  atomic.Int32
	once   sync.Once
}

func (r *wedgingRunner) release() {
	r.once.Do(func() { close(r.gate) })
}

func (r *wedgingRunner) Create(ctx context.Context, opts session.RunOptions) error {
	return fakeCreateSession(r.store, ctx, opts)
}

func (r *wedgingRunner) FailSetup(context.Context, string, error) error { return nil }

// Resume satisfies the Runner seam. These three doubles exist to exercise
// stop, and stopping a resumed run is the same code path as stopping a fresh
// one (the registry entry is keyed on the session id either way), so none of
// them needs a distinct resume behaviour; the tests that cover the resume
// branch itself drive it through a runner of their own.
func (r *wedgingRunner) Resume(context.Context, session.ResumeOptions) (*session.RunResult, error) {
	return nil, errors.New("resume: not used by this test double")
}

func (r *wedgingRunner) Run(ctx context.Context, opts session.RunOptions) (*session.RunResult, error) {
	if err := fakePromoteSession(r.store, ctx, opts); err != nil {
		return nil, err
	}
	if r.calls.Add(1) == 1 {
		close(r.wedged)
		<-r.gate // genuinely wedged: ctx is never consulted again
	}
	return &session.RunResult{SessionID: opts.SessionID, Status: store.StatusOK, Text: "done"}, nil
}

// finishThenBlockRunner completes its session work — the session row is
// created and finished ok, exactly as Runner.Run would leave it — and then
// blocks on a gate ignoring ctx: a run whose session is terminal while its
// goroutine is still alive. That is the gap CancelRunningSession's
// SessionFinishedError exists for (docs/RUN-CONTROL.md "Half two").
type finishThenBlockRunner struct {
	store  *store.Store
	gate   chan struct{}
	wedged chan struct{} // closed once the session row is terminal
	once   sync.Once
}

func (r *finishThenBlockRunner) release() {
	r.once.Do(func() { close(r.gate) })
}

func (r *finishThenBlockRunner) Create(ctx context.Context, opts session.RunOptions) error {
	return fakeCreateSession(r.store, ctx, opts)
}

func (r *finishThenBlockRunner) FailSetup(context.Context, string, error) error { return nil }

// Resume satisfies the Runner seam. These three doubles exist to exercise
// stop, and stopping a resumed run is the same code path as stopping a fresh
// one (the registry entry is keyed on the session id either way), so none of
// them needs a distinct resume behaviour; the tests that cover the resume
// branch itself drive it through a runner of their own.
func (r *finishThenBlockRunner) Resume(context.Context, session.ResumeOptions) (*session.RunResult, error) {
	return nil, errors.New("resume: not used by this test double")
}

func (r *finishThenBlockRunner) Run(ctx context.Context, opts session.RunOptions) (*session.RunResult, error) {
	if err := fakePromoteSession(r.store, ctx, opts); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := r.store.FinishSession(ctx, opts.SessionID, store.StatusOK, "", "the run completed", &now); err != nil {
		return nil, err
	}
	close(r.wedged)
	<-r.gate
	return &session.RunResult{SessionID: opts.SessionID, Status: store.StatusOK, Text: "completed"}, nil
}

// fakeCreateSession makes the session row a real Runner.Create would, the
// minimum the store needs, as "creating" — the status the worker's run path
// inserts before workspace preparation.
func fakeCreateSession(st *store.Store, ctx context.Context, opts session.RunOptions) error {
	return st.CreateSession(ctx, store.Session{
		ID:             opts.SessionID,
		Model:          opts.Model,
		Effort:         opts.Effort,
		Workspace:      opts.Workspace,
		PermissionMode: string(opts.PermissionMode),
		Status:         store.StatusCreating,
	})
}

// fakePromoteSession flips the row fakeCreateSession made to "running", the
// way a real Runner.Run promotes a pre-created row once preparation is done.
func fakePromoteSession(st *store.Store, ctx context.Context, opts session.RunOptions) error {
	return st.PromoteSession(ctx, opts.SessionID, opts.Workspace, "sys", []byte("[]"), opts.ResultSchema)
}

// waitForSessionID polls the work_requests row until the run has attached
// its session id — which happens after the run registered itself in the
// controller — and returns it.
func (h *testHarness) waitForSessionID(t *testing.T, requestID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
		if err == nil && row.SessionID != "" {
			return row.SessionID
		}
		if time.Now().After(deadline) {
			t.Fatalf("session id never attached to %s (last error %v)", requestID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForSessionStatus polls until sessionID's row carries want.
func waitForSessionStatus(t *testing.T, h *testHarness, sessionID, want string, timeout time.Duration) store.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sess, err := h.pool.Store.GetSession(context.Background(), sessionID)
		if err == nil && sess.Status == want {
			return sess
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never reached status %q (last error %v)", sessionID, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForNotRunning polls until the pool no longer reports sessionID running.
// The registry entry is torn down where the message is disposed of, so this
// is also proof the finish sequence — which ends in the ack — ran.
func (h *testHarness) waitForNotRunning(t *testing.T, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for h.pool.Running(sessionID) {
		if time.Now().After(deadline) {
			t.Fatalf("session %s still reported running", sessionID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStopHealthyRunCancels is the soft-stop path: a run that answers a
// cancelled runCtx at its next check point ends on its own inside the grace
// period, its ordinary finish path publishes the cancelled result, and the
// message is acked — nothing in the force-finish path ever runs.
func TestStopHealthyRunCancels(t *testing.T) {
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		return &ctxBlockingRunner{store: st}
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond
	defer h.startPool(t)()

	requestID := uniqueID("req-stop-healthy")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full"})
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)

	// Wait for the session row to reach "running" before stopping: this test
	// is about the soft stop of a live run, so the stop must land inside
	// Runner.Run rather than in workspace preparation. The store-backed claim
	// loop delivers the message fast enough that the session id can be
	// visible while Create is still in flight; a stop landing there is a
	// different, equally valid shape, covered by
	// TestStopDuringPreparationSoftStopCancelsRowAndResult.
	waitForSessionStatus(t, h, sessionID, store.StatusRunning, 5*time.Second)

	if err := h.pool.Stop(sessionID, "the user asked"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v (error: %+v)", res, res.Error)
	}
	if res.Error == nil || res.Error.Code != "cancelled" || res.Error.Message != "the user asked" {
		t.Fatalf("expected error {cancelled, the user asked}, got %+v", res.Error)
	}

	// The work_requests row is recorded cancelled (the one place a result
	// lives) and the message was answered: the run's registry entry is gone
	// with it.
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Status != queue.StatusCancelled {
		t.Fatalf("expected the row to be recorded cancelled, got %q", row.Status)
	}
	h.waitForNotRunning(t, sessionID, 2*time.Second)
}

// TestStopWedgedRunFreesSlotAndCancels is the direct regression test for the
// production incident that shaped this phase (docs/RUN-CONTROL.md "Half
// two"): a run wedged in a syscall no context cancellation can unblock. The
// fake runner blocks on a channel the test never closes while the escalation
// runs, ignoring ctx entirely, so it is genuinely wedged rather than merely
// slow. The session row must reach cancelled within the grace period, a
// cancelled result must be published, the message must be acked — and,
// because Size equals the consumer's MaxAckPending, a follow-up request only
// gets delivered if both the slot and the ack really came back.
func TestStopWedgedRunFreesSlotAndCancels(t *testing.T) {
	runner := &wedgingRunner{gate: make(chan struct{}), wedged: make(chan struct{})}
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		runner.store = st
		return runner
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond
	stop := h.startPool(t)
	// Defers run LIFO: the gate is opened before the pool is told to drain,
	// so Run's wg.Wait cannot hang on the wedged goroutine.
	defer stop()
	defer runner.release()

	requestID := uniqueID("req-wedged")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "wedged task", Repos: testRepos(), PermissionMode: "full"})

	// Wait until the run is genuinely wedged: past the point any context
	// cancellation can reach.
	select {
	case <-runner.wedged:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never wedged")
	}
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)

	stopAt := time.Now()
	if err := h.pool.Stop(sessionID, "the operator gave up waiting"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The grace period passes and the force-finish marks the session row
	// cancelled — only the escalation could have, since the runner is still
	// wedged. A generous bound proves it acted on the escalation's schedule
	// rather than much later.
	sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 5*time.Second)
	if elapsed := time.Since(stopAt); elapsed > 3*time.Second {
		t.Fatalf("session took %s to reach cancelled; the grace period was 150ms", elapsed)
	}
	if sess.FinishedAt == nil {
		t.Fatal("expected the cancelled session to carry finished_at")
	}

	// A cancelled result is recorded and the message acked.
	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "cancelled" || res.Error.Message != "the operator gave up waiting" {
		t.Fatalf("expected error {cancelled, the operator gave up waiting}, got %+v", res.Error)
	}
	h.waitForNotRunning(t, sessionID, 2*time.Second)

	// The slot really came back: with Size 1 the consumer's MaxAckPending is
	// 1, so this follow-up only gets delivered if the wedged run's slot and
	// its ack are both gone. The runner's second call succeeds immediately.
	second := uniqueID("req-after-wedge")
	h.publish(t, queue.Request{RequestID: second, Prompt: "must complete", Repos: testRepos(), PermissionMode: "full"})
	res2 := h.fetchFinalResult(t, second, 10*time.Second)
	if res2.Status != queue.StatusOK {
		t.Fatalf("expected the follow-up request to complete, got %+v", res2)
	}

	// Release the blocked runner so the wedged goroutine can wake and the
	// test does not leak it.
	runner.release()
}

// TestWedgedRunCannotPublishTwice releases the blocked runner after the
// force-finish has landed and asserts the wedged goroutine logs and drops its
// own result instead of finishing a second time: no second record of a
// finish. A second finish would have bumped the work_requests row's version,
// so that is the assertion that catches it.
func TestWedgedRunCannotPublishTwice(t *testing.T) {
	runner := &wedgingRunner{gate: make(chan struct{}), wedged: make(chan struct{})}
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		runner.store = st
		return runner
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond
	stop := h.startPool(t)
	defer stop()
	defer runner.release()

	requestID := uniqueID("req-no-twice")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "wedged task", Repos: testRepos(), PermissionMode: "full"})
	select {
	case <-runner.wedged:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never wedged")
	}
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)

	if err := h.pool.Stop(sessionID, "stop it"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The force-finish lands: session cancelled, result published.
	waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 5*time.Second)
	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	version, result := row.Version, string(row.Result)

	// Now the wedged goroutine wakes. Its message has already been answered;
	// it must drop its own result rather than finish again.
	runner.release()
	time.Sleep(500 * time.Millisecond) // give a (wrong) second finish time to land if it were going to

	row, err = h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Version != version {
		t.Fatalf("expected no second finish: the row version moved from %d to %d", version, row.Version)
	}
	if string(row.Result) != result {
		t.Fatalf("expected the row's stored result untouched, got %q", row.Result)
	}
	if row.Status != queue.StatusCancelled {
		t.Fatalf("expected the row to stay cancelled, got %q", row.Status)
	}
	if sess := waitForSessionStatus(t, h, sessionID, store.StatusCancelled, 2*time.Second); sess.FinishedAt == nil {
		t.Fatal("expected the cancelled session to carry finished_at")
	}
}

// TestStopIsIdempotent: two stops are one cancellation — the second Stop
// returns nil without a second cancellation or a second published result.
func TestStopIsIdempotent(t *testing.T) {
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		return &ctxBlockingRunner{store: st}
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond
	defer h.startPool(t)()

	requestID := uniqueID("req-stop-twice")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "do it", Repos: testRepos(), PermissionMode: "full"})
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)
	// As in TestStopHealthyRunCancels: stop only once the run is inside
	// Runner.Run, so the stop cancels the run rather than its preparation.
	waitForSessionStatus(t, h, sessionID, store.StatusRunning, 5*time.Second)

	if err := h.pool.Stop(sessionID, "first stop"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.pool.Stop(sessionID, "second stop"); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != "cancelled" || res.Error.Message != "first stop" {
		t.Fatalf("expected the first stop's reason to stick, got %+v", res.Error)
	}
	// One cancellation, one recorded finish: the row's version must not move
	// after the result is in.
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	version := row.Version
	time.Sleep(300 * time.Millisecond)
	row, err = h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Version != version {
		t.Fatalf("expected one cancellation and one recorded result, got a second finish (version %d -> %d)", version, row.Version)
	}
}

// TestStopUnknownSession: a stop for a session this process does not own is
// ErrRunNotFound, and Running reports false.
func TestStopUnknownSession(t *testing.T) {
	h := newTestHarness(t, "", 1)
	if err := h.pool.Stop("sess-nobody-runs", "why"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
	if h.pool.Running("sess-nobody-runs") {
		t.Fatal("Running must be false for an unknown session")
	}
}

// TestStopAfterRunFinished is the SessionFinishedError race: the run's
// session work completed — its row is terminal ok — while its goroutine is
// still alive, and a stop lands in that gap. The escalation waits out the
// grace period, CancelRunningSession finds the run already finished, and
// leaves it alone: the completed run keeps its own status and no cancelled
// result is published over it. The run's own ok result is what the caller
// gets, once the goroutine is released.
func TestStopAfterRunFinished(t *testing.T) {
	runner := &finishThenBlockRunner{gate: make(chan struct{}), wedged: make(chan struct{})}
	h := newTestHarnessWithRunner(t, "", 1, func(st *store.Store) Runner {
		runner.store = st
		return runner
	})
	h.pool.StopGracePeriod = 150 * time.Millisecond
	stop := h.startPool(t)
	defer stop()
	defer runner.release()

	requestID := uniqueID("req-done")
	h.publish(t, queue.Request{RequestID: requestID, Prompt: "finish me", Repos: testRepos(), PermissionMode: "full"})
	select {
	case <-runner.wedged:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never finished its session work")
	}
	sessionID := h.waitForSessionID(t, requestID, 5*time.Second)

	sess, err := h.pool.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("expected the session row to be terminal ok before the stop, got %q", sess.Status)
	}

	if err := h.pool.Stop(sessionID, "stop anyway"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Let the escalation wait out the grace period and hit the
	// SessionFinishedError race before the goroutine is released.
	time.Sleep(600 * time.Millisecond)
	runner.release()

	res := h.fetchFinalResult(t, requestID, 10*time.Second)
	if res.Status != queue.StatusOK {
		t.Fatalf("expected the completed run to keep its own status ok, got %+v", res)
	}
	if res.Text != "completed" {
		t.Fatalf("expected the run's own result text, got %q", res.Text)
	}
	// Exactly one recorded finish — the run's own ok — and no cancelled
	// result written over it: the row's version must not move.
	row, err := h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	version, result := row.Version, string(row.Result)
	time.Sleep(300 * time.Millisecond)
	row, err = h.pool.Store.GetWorkRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get work request: %v", err)
	}
	if row.Version != version || string(row.Result) != result {
		t.Fatalf("expected the run's own ok result to be the one recorded, got version %d result %s", row.Version, row.Result)
	}
	sess, err = h.pool.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("expected the session to stay ok, got %q", sess.Status)
	}
}
