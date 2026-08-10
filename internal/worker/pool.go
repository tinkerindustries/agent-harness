// Package worker is the harness's worker pool: it pulls work requests off
// the WORK stream, runs each as a session.Runner call, and publishes the
// result to the RESULTS stream (docs/DESIGN.md §4.10).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/workspace"
)

// Runner runs one session to a terminal result. *session.Runner implements
// it; the interface exists so the pool's tests can drive a run that ignores
// its context entirely — a genuinely wedged run — without calling an API.
type Runner interface {
	Run(ctx context.Context, opts session.RunOptions) (*session.RunResult, error)
}

// Pool pulls from Consumer and dispatches each message to a session
// goroutine, bounded by Size. Nothing here holds per-request state outside
// the handler for that request — the property docs/DESIGN.md §4.5 asks
// every caller of session.Runner to preserve.
type Pool struct {
	Store    *store.Store
	Runner   Runner
	JS       jetstream.JetStream
	Consumer jetstream.Consumer

	// WorkspaceRoot is the parent directory each run's own workspace is
	// created under, named for its session id (docs/DESIGN.md §4.10).
	WorkspaceRoot    string
	DefaultModel     string
	DefaultEffort    string
	DefaultThinking  bool
	DefaultMaxTokens int
	DefaultDeadline  time.Duration
	PriceTableDate   string

	// Settings, when set, is where the per-request defaults resolve from:
	// run.max_tokens, run.deadline, model.default, and model.effort are read
	// through the store on every request, so a key changed with `harness
	// config set` takes effect on the next request without a restart. Nil is
	// the test path: the Default* fields above, then the built-in values
	// below, apply.
	Settings *settings.Resolver

	// Size bounds concurrent runs. It must equal the consumer's
	// MaxAckPending (docs/DESIGN.md §4.10) so JetStream never delivers more
	// than the pool can work on; Size is the local backstop, not the flow
	// controller.
	Size int

	// MaxDeliveryAttempts must equal the consumer's MaxDeliver. The server
	// enforces the ceiling; the pool needs to know it so the last attempt
	// can publish a terminal result before the message goes away, rather
	// than leaving the caller waiting on a request that will never be
	// delivered again (docs/DESIGN.md §4.10). Zero means
	// queue.DefaultMaxDeliveryAttempts.
	MaxDeliveryAttempts int

	// HeartbeatInterval, LeasePollInterval, and RetryLaterDelay have
	// production defaults and are overridable so tests do not have to wait
	// on them.
	HeartbeatInterval time.Duration
	LeasePollInterval time.Duration
	RetryLaterDelay   time.Duration

	// PrepareWorkspace builds one run's workspace. It defaults to
	// workspace.Prepare and is overridable so a test can drive the pool
	// without cloning over the network.
	PrepareWorkspace func(ctx context.Context, root, sessionID string, repos []queue.Repo) (string, error)

	// StopGracePeriod overrides run.stop_grace_period for a stop's
	// force-finish escalation. Zero (the production default) resolves the
	// setting through Settings; tests set it so they do not wait 30 seconds.
	StopGracePeriod time.Duration

	wg sync.WaitGroup

	ctrl     *Controller
	ctrlOnce sync.Once

	haltMu     sync.Mutex
	stopPull   func()
	halted     atomic.Bool
	haltReason atomic.Pointer[string]
}

func (p *Pool) size() int {
	if p.Size > 0 {
		return p.Size
	}
	return 4
}

func (p *Pool) heartbeatInterval() time.Duration {
	if p.HeartbeatInterval > 0 {
		return p.HeartbeatInterval
	}
	return 20 * time.Second
}

func (p *Pool) leasePollInterval() time.Duration {
	if p.LeasePollInterval > 0 {
		return p.LeasePollInterval
	}
	return 2 * time.Second
}

func (p *Pool) retryLaterDelay() time.Duration {
	if p.RetryLaterDelay > 0 {
		return p.RetryLaterDelay
	}
	return 5 * time.Second
}

func (p *Pool) prepareWorkspace() func(context.Context, string, string, []queue.Repo) (string, error) {
	if p.PrepareWorkspace != nil {
		return p.PrepareWorkspace
	}
	return workspace.Prepare
}

// defaultDeadline is the wall clock a run that names no deadline gets. It
// resolves run.deadline from the settings registry when a resolver is
// attached — sized to let a full 400-sub-turn budget run at roughly six
// seconds a sub-turn — and falls back to 60 minutes otherwise (the
// registry default, mirrored here for the test path).
func (p *Pool) defaultDeadline(ctx context.Context) time.Duration {
	if p.DefaultDeadline > 0 {
		return p.DefaultDeadline
	}
	if p.Settings != nil {
		if v, err := p.Settings.Duration(ctx, settings.KeyRunDeadline); err == nil {
			return v
		}
	}
	return 60 * time.Minute
}

func (p *Pool) defaultModel(ctx context.Context) string {
	if p.DefaultModel != "" {
		return p.DefaultModel
	}
	if p.Settings != nil {
		if v, err := p.Settings.String(ctx, settings.KeyDefaultModel); err == nil {
			return v
		}
	}
	return "deepseek-v4-pro"
}

func (p *Pool) defaultEffort(ctx context.Context) string {
	if p.DefaultEffort != "" {
		return p.DefaultEffort
	}
	if p.Settings != nil {
		if v, err := p.Settings.String(ctx, settings.KeyDefaultEffort); err == nil {
			return v
		}
	}
	return "high"
}

func (p *Pool) defaultMaxTokens(ctx context.Context) int {
	if p.DefaultMaxTokens > 0 {
		return p.DefaultMaxTokens
	}
	if p.Settings != nil {
		if v, err := p.Settings.Int(ctx, settings.KeyRunMaxTokens); err == nil {
			return v
		}
	}
	return 48000
}

// Run pulls and processes messages until ctx is done. On shutdown it stops
// pulling new work but lets in-flight runs finish and publish normally —
// an agent run takes minutes, and cutting one off on a routine restart
// would waste it for no reason. Only a process that dies outright leaves a
// message for the spent-request path to pick up.
func (p *Pool) Run(ctx context.Context) error {
	sem := make(chan struct{}, p.size())
	consumeCtx, err := p.Consumer.Consume(func(msg jetstream.Msg) {
		sem <- struct{}{}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			// The release is an idempotent closure (sync.Once over <-sem)
			// rather than a plain defer, so the stop escalation can free the
			// pool slot for a run whose own goroutine is wedged and whose
			// deferred release would otherwise never run (docs/RUN-CONTROL.md
			// "Half two"). The run goroutine's own deferred call becomes a
			// no-op once the escalation has released the slot.
			release := sync.OnceFunc(func() { <-sem })
			defer release()
			p.handle(msg, release)
		}()
	}, jetstream.PullMaxMessages(p.size()))
	if err != nil {
		return errors.New("worker: consume: " + err.Error())
	}
	p.haltMu.Lock()
	p.stopPull = consumeCtx.Stop
	p.haltMu.Unlock()

	<-ctx.Done()
	consumeCtx.Stop()
	p.wg.Wait()
	return nil
}

// Halt stops the pool from pulling any further work; runs already in flight
// keep going and still publish their results normally. A 402 from DeepSeek
// means the account balance is gone, and every other queued request would
// hit the identical wall, so the pool stops instead of failing them one at a
// time (docs/DESIGN.md §4.5, §4.10). Calling Halt more than once, or before
// Run has started pulling, is safe; only the first call's reason sticks.
func (p *Pool) Halt(reason string) {
	if !p.halted.CompareAndSwap(false, true) {
		return
	}
	p.haltReason.Store(&reason)
	p.haltMu.Lock()
	stop := p.stopPull
	p.haltMu.Unlock()
	if stop != nil {
		stop()
	}
	log.Printf("worker: pool halted: %s", reason)
}

// Halted reports whether Halt has been called and why, for the queue health
// endpoint (docs/DESIGN.md §5.8), which surfaces an empty account as a
// state rather than leaving an
// operator inferring it from a run of failed requests.
func (p *Pool) Halted() (bool, string) {
	if !p.halted.Load() {
		return false, ""
	}
	if r := p.haltReason.Load(); r != nil {
		return true, *r
	}
	return true, ""
}

func (p *Pool) maxDeliveryAttempts() uint64 {
	if p.MaxDeliveryAttempts > 0 {
		return uint64(p.MaxDeliveryAttempts)
	}
	return queue.DefaultMaxDeliveryAttempts
}

// retryLater defers a request to a later delivery, except on the delivery
// the consumer's MaxDeliver makes the last one — there is no later delivery
// then, and a bare Nak would drop the request without the caller ever
// learning why. docs/DESIGN.md §4.10: "On the last delivery attempt, publish
// failed and Term."
//
// code and message describe the transient failure that stopped this attempt;
// they only reach anyone on the final attempt, which is the one where the
// caller has no other way to find out.
func (p *Pool) retryLater(msg jetstream.Msg, requestID, code, message string) {
	if requestID == "" || deliveryCount(msg) < p.maxDeliveryAttempts() {
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}
	log.Printf("worker: %s exhausted %d delivery attempts (%s); publishing a failed result and terminating the message",
		requestID, p.maxDeliveryAttempts(), code)
	// Record the failure under no session, the same way a validation
	// failure does. finish matches the row on its session id, so a row
	// still carrying the dead attempt's session id would not match and the
	// result would never be published.
	if err := p.Store.SetWorkRequestSession(context.Background(), requestID, ""); err != nil {
		log.Printf("worker: clear session for exhausted request %s: %v", requestID, err)
	}
	now := time.Now().UTC()
	p.finish(msg, requestID, "", queue.Result{
		RequestID: requestID,
		Status:    queue.StatusFailed,
		Error: &queue.ResultError{
			Code:    code,
			Message: message,
		},
		StartedAt: now, FinishedAt: now,
	}, true)
}

func deliveryCount(msg jetstream.Msg) uint64 {
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return 1
	}
	return meta.NumDelivered
}

// handle is one message's whole lifecycle: parse, claim the idempotency
// row, and either run a session, record a validation failure, republish a
// terminal row, or defer to a later delivery. releaseSlot is the run's
// idempotent pool-slot release, threaded down to the run path so the stop
// escalation can free the slot of a run that wedges (docs/RUN-CONTROL.md
// "Half two"); the other paths never register and never release.
func (p *Pool) handle(msg jetstream.Msg, releaseSlot func()) {
	defer p.recoverPanic(msg)

	numDelivered := deliveryCount(msg)

	req, err := queue.ParseRequest(msg.Data())
	if err != nil {
		log.Printf("worker: malformed request body, terminating message: %v", err)
		msg.Term()
		return
	}
	if req.RequestID == "" {
		log.Printf("worker: request has no request_id, terminating message")
		msg.Term()
		return
	}

	verr := req.Validate()

	ctx := context.Background()
	outcome, err := p.Store.ClaimWorkRequest(ctx, req.RequestID, numDelivered, time.Now().UTC())
	if err != nil {
		log.Printf("worker: claim %s: %v", req.RequestID, err)
		p.retryLater(msg, req.RequestID, "claim_failed",
			fmt.Sprintf("could not claim the idempotency row after %d delivery attempts: %v", p.maxDeliveryAttempts(), err))
		return
	}

	if !outcome.Claimed {
		switch outcome.Refusal {
		case store.RefusalTerminal:
			p.republish(msg, outcome.Existing)
			return
		case store.RefusalSpent:
			// The request already ran once — its row carries the session id.
			// A work request is single-use: close the abandoned session, tell
			// the caller, and terminate the message.
			p.handleSpent(msg, req, outcome)
			return
		default:
			// A duplicate publish while the original is still genuinely
			// running and has not attached its session yet (a sessionless
			// running row only falls through here when this message is on its
			// first delivery; a redelivered one would have been claimed as an
			// attempt that died during preparation). Nak would redeliver this
			// exact message and bump its own NumDelivered, which is
			// indistinguishable from the server's own "nobody is heartbeating
			// this" signal — after one such cycle shouldClaim would wrongly
			// read this message as abandoned and start a second session for a
			// request that never stopped being owned. Wait and heartbeat
			// instead, so the only way NumDelivered ever climbs past 1 is
			// JetStream deciding so on its own.
			p.waitForResolution(msg, req)
			return
		}
	}

	if verr != nil {
		p.recordValidationFailure(ctx, msg, req.RequestID, verr)
		return
	}

	p.run(msg, req, releaseSlot)
}

// abandonedSessionIdleThreshold is how long a running session must have
// been quiet before the spent-request path may close it as abandoned. It
// mirrors internal/httpapi's sessionIdleThreshold, the operator-facing
// definition of "abandoned": a redelivery can be false — a heartbeat that
// failed to land while the original attempt was still alive — and the idle
// check is what stops the close from touching a live session. A live run
// appends events continuously, so quiet for this long means abandoned.
const abandonedSessionIdleThreshold = 10 * time.Minute

// handleSpent is the single-use path for a delivery whose request is spent:
// the work_requests row carries a session id, so an attempt actually
// started and nothing may ever run this request again. It closes the
// abandoned session — the row phase 1's CloseSession was built for, and the
// one that until now never got closed, so abandoned sessions sat running in
// the list forever — publishes a failed result telling the caller what
// happened and how to retry, and terminates the message: no further
// delivery can help.
func (p *Pool) handleSpent(msg jetstream.Msg, req queue.Request, outcome store.ClaimOutcome) {
	ctx := context.Background()
	sessionID := outcome.Existing.SessionID

	proceed, started, err := p.closeAbandonedSession(ctx, sessionID)
	if err != nil {
		log.Printf("worker: close abandoned session %s for spent request %s: %v", sessionID, req.RequestID, err)
		p.retryLater(msg, req.RequestID, "abandoned_close_failed",
			fmt.Sprintf("could not close the abandoned session after %d delivery attempts: %v", p.maxDeliveryAttempts(), err))
		return
	}
	if !proceed {
		// The original attempt is still alive (or is actively writing its
		// session row, which for a running session only the run loop does).
		// Leave the row alone and let the existing duplicate-handling path
		// own this message.
		p.waitForResolution(msg, req)
		return
	}

	now := time.Now().UTC()
	if started.IsZero() {
		// The session row was gone (an operator closed and deleted it), so
		// there is no creation time to report; the failure time is the only
		// honest one.
		started = now
	}
	result := queue.Result{
		RequestID: req.RequestID,
		SessionID: sessionID,
		Status:    queue.StatusFailed,
		Error: &queue.ResultError{
			Code: "abandoned",
			Message: "the run was abandoned when its worker died before finishing; " +
				"work requests are single-use, so this request will not run again — " +
				"republish it under a new request_id to retry",
		},
		StartedAt:  started,
		FinishedAt: now,
	}
	p.finish(msg, req.RequestID, sessionID, result, true)
}

// closeAbandonedSession closes sessionID as an abandoned run and reports
// whether the spent-request path may proceed. proceed is true when the
// session is terminal (or gone) and the failure can be recorded and
// published; proceed is false with nil error when the session looks live
// and the message should fall through to the duplicate-handling path
// instead.
//
// The session's version is read immediately before the close, so a
// concurrent write shows up as a VersionConflictError and the close is
// retried once with the fresh version. That distinguishes the two things a
// conflict can mean: an operator closed the session in the gap (the retry
// then succeeds, keeping the terminal status), or the run loop wrote —
// for a running session the only writer that ever touches it — meaning the
// original attempt is alive (the retry hits the idle check, or conflicts
// again, and proceed is false). A session with no events has nothing
// recent and passes the idle check, which is exactly the abandoned-during-
// preparation shape this path exists to close.
func (p *Pool) closeAbandonedSession(ctx context.Context, sessionID string) (proceed bool, started time.Time, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		sess, err := p.Store.GetSession(ctx, sessionID)
		if errors.Is(err, store.ErrNotFound) {
			// The session row is gone (an operator closed and deleted it).
			// The request is still spent — its row keeps the session id — so
			// the failure must still be recorded and published.
			return true, time.Time{}, nil
		}
		if err != nil {
			return false, time.Time{}, err
		}
		_, err = p.Store.CloseSession(ctx, sessionID, store.StatusFailed, sess.Version, time.Now().UTC(), abandonedSessionIdleThreshold)
		if err == nil {
			return true, sess.CreatedAt, nil
		}
		var active *store.ActiveSessionError
		if errors.As(err, &active) {
			return false, time.Time{}, nil
		}
		var conflict *store.VersionConflictError
		if !errors.As(err, &conflict) {
			return false, time.Time{}, err
		}
		// Version conflict: re-read and retry once (loop iteration). Two
		// consecutive conflicts on a row that is still running mean a live
		// run loop is writing it.
	}
	return false, time.Time{}, nil
}

// waitForResolution holds a message whose request_id is claimed by another,
// live attempt, heartbeating it while polling the row until that attempt
// finishes (then republishes its result) or this request's own deadline
// passes (then Naks as a last resort — see the comment at the call site for
// why that resort is safe). It never runs a session itself.
func (p *Pool) waitForResolution(msg jetstream.Msg, req queue.Request) {
	deadline := p.defaultDeadline(context.Background())
	if req.DeadlineMS > 0 {
		deadline = time.Duration(req.DeadlineMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	hbDone := make(chan struct{})
	go p.heartbeat(msg, hbDone)
	defer close(hbDone)

	ticker := time.NewTicker(p.leasePollInterval())
	defer ticker.Stop()
	for {
		row, err := p.Store.GetWorkRequest(ctx, req.RequestID)
		if err == nil && row.Status != store.WorkRequestStatusRunning {
			p.republish(msg, row)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			p.retryLater(msg, req.RequestID, "duplicate_unresolved",
				fmt.Sprintf("a concurrent attempt held the request and had not finished after %d delivery attempts", p.maxDeliveryAttempts()))
			return
		}
	}
}

func (p *Pool) recoverPanic(msg jetstream.Msg) {
	if r := recover(); r != nil {
		log.Printf("worker: recovered panic handling message: %v\n%s", r, debug.Stack())
		msg.NakWithDelay(p.retryLaterDelay())
	}
}

// recordValidationFailure is the request and result schema, validation and
// its Term path: a request that never
// authorizes itself is recorded as failed under no session (sessionID ""),
// published once, and Term'd so it is never redelivered.
func (p *Pool) recordValidationFailure(ctx context.Context, msg jetstream.Msg, requestID string, verr error) {
	if err := p.Store.SetWorkRequestSession(ctx, requestID, ""); err != nil {
		log.Printf("worker: clear session for invalid request %s: %v", requestID, err)
	}
	now := time.Now().UTC()
	result := queue.Result{
		RequestID: requestID,
		Status:    queue.StatusFailed,
		Error:     &queue.ResultError{Code: "invalid_request", Message: verr.Error()},
		StartedAt: now, FinishedAt: now,
	}
	p.finish(msg, requestID, "", result, true)
}

// run drives one claimed, valid request through workspace preparation and
// the session loop to a terminal result.
func (p *Pool) run(msg jetstream.Msg, req queue.Request, releaseSlot func()) {
	started := time.Now().UTC()
	sessionID := session.NewSessionID()

	hbDone := make(chan struct{})
	var hbOnce sync.Once
	stopHeartbeat := func() { hbOnce.Do(func() { close(hbDone) }) }
	go p.heartbeat(msg, hbDone)
	defer stopHeartbeat()

	deadline := p.defaultDeadline(context.Background())
	if req.DeadlineMS > 0 {
		deadline = time.Duration(req.DeadlineMS) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	// Register before workspace preparation, so a run wedged in a git clone
	// is stoppable too. Deregistration happens where the message is disposed
	// of (finish), not where Runner.Run returns, so the entry outlives the
	// run by exactly as long as its disposal takes (docs/RUN-CONTROL-PLAN.md
	// "Step 3 — The control seam").
	rec := &inflight{
		requestID:     req.RequestID,
		sessionID:     sessionID,
		msg:           msg,
		cancel:        cancel,
		done:          make(chan struct{}),
		releaseSlot:   releaseSlot,
		stopHeartbeat: stopHeartbeat,
		started:       started,
	}
	p.controller().add(rec)
	defer close(rec.done)

	if err := p.Store.SetWorkRequestSession(runCtx, req.RequestID, sessionID); err != nil {
		log.Printf("worker: attach session for %s: %v", req.RequestID, err)
	}
	p.publishAccepted(req.RequestID, sessionID, started)

	// Each attempt clones into a directory of its own, named for its session
	// id, so a redelivery never inherits the half-finished tree of an
	// attempt that died before its session existed.
	ws, err := p.prepareWorkspace()(runCtx, p.WorkspaceRoot, sessionID, req.Repos)
	if err != nil {
		if !rec.answer() {
			// A stop force-finished this run while preparation was wedged;
			// the cancelled result is already on the stream.
			log.Printf("worker: %s: message for session %s already answered; dropping the setup failure", req.RequestID, sessionID)
			return
		}
		result := setupFailedResult(req.RequestID, sessionID, started, err)
		p.finish(msg, req.RequestID, sessionID, result, false)
		return
	}

	// Validate has already rejected an absent or unknown mode.
	mode := tools.Mode(req.PermissionMode)
	model := req.Model
	if model == "" {
		model = p.defaultModel(runCtx)
	}
	effort := req.Effort
	if effort == "" {
		effort = p.defaultEffort(runCtx)
	}

	progressLimiter := queue.NewProgressLimiter(time.Second)
	runResult, runErr := p.Runner.Run(runCtx, session.RunOptions{
		SessionID:       sessionID,
		Model:           model,
		Effort:          effort,
		Thinking:        p.DefaultThinking,
		MaxTokens:       p.defaultMaxTokens(runCtx),
		Workspace:       ws,
		PermissionMode:  mode,
		Deny:            req.Deny,
		Prompt:          req.Prompt,
		ResultSchema:    req.ResultSchema,
		MaxSubTurns:     req.MaxSubTurns,
		JobType:         req.JobType,
		ParentAgentType: req.ParentAgentType,
		ParentAgentID:   req.ParentAgentID,
		Progress: func(sp session.SubTurnProgress) {
			if progressLimiter.Allow(time.Now()) {
				p.publishProgress(req.RequestID, sp)
			}
		},
	})

	// An empty account is distinct from an ordinary run failure: every other
	// queued request is about to hit the same wall, so the pool stops
	// pulling more work instead of finishing (and burning) each one in turn.
	// This request's own session already recorded its failure through
	// Runner.Run's normal error path; leaving the JetStream message unacked
	// here, rather than publishing a terminal result, is what lets it
	// redeliver and run as a fresh attempt once the pool is restarted with
	// balance restored — the one retry this phase keeps, because the
	// request's row still carries the session id and every later delivery
	// goes through the spent-request path.
	if runErr != nil && deepseek.IsInsufficientBalance(runErr) {
		if !rec.answer() {
			// A stop already answered this message; there is nothing to
			// defer to a later delivery.
			return
		}
		p.Halt("account balance exhausted (402 from DeepSeek)")
		log.Printf("worker: %s failed on an empty account; left unacked for retry after the pool restarts", req.RequestID)
		// One delivery is spent per pool restart that still finds the
		// account empty, so this waits for balance across restarts as
		// before — but within the ceiling, and the last attempt says the
		// account was empty instead of the request disappearing.
		p.retryLater(msg, req.RequestID, "insufficient_balance",
			fmt.Sprintf("the DeepSeek account had no balance on each of %d delivery attempts", p.maxDeliveryAttempts()))
		// The run is over; the registry entry goes with it, like any other
		// message answered outside finish.
		p.controller().remove(sessionID)
		return
	}

	if !rec.answer() {
		// A stop force-finished this run while it was wedged; the cancelled
		// result is already on the stream and the message already acked. A
		// wedged goroutine that wakes now must log and drop its own result,
		// not publish a second, contradictory one over the top of the first
		// (docs/RUN-CONTROL.md "Half two").
		log.Printf("worker: %s: message for session %s already answered; dropping the run's own result", req.RequestID, sessionID)
		return
	}
	result := p.classify(req.RequestID, sessionID, started, runResult, runErr, runCtx.Err(), rec.stopping.Load(), rec.stopReason())
	p.finish(msg, req.RequestID, sessionID, result, false)
}

func (p *Pool) heartbeat(msg jetstream.Msg, done <-chan struct{}) {
	t := time.NewTicker(p.heartbeatInterval())
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := msg.InProgress(); err != nil {
				log.Printf("worker: heartbeat: %v", err)
			}
		case <-done:
			return
		}
	}
}

// classify turns a session run's outcome into a queue.Result. A timeout is
// distinguished from an ordinary failure by checking runCtx's own error
// rather than parsing runErr's text, since the context that actually
// expired is the authoritative signal. A stop is distinguished from a
// timeout by the registry's stopping flag, not the context error: a stop
// and a deadline both leave runCtx cancelled, and only the flag can tell
// an operator's decision from a budget running out (docs/RUN-CONTROL.md
// "Half two"). stopped and reason are the run's record's, read by the run
// goroutine.
func (p *Pool) classify(requestID, sessionID string, started time.Time, runResult *session.RunResult, runErr, ctxErr error, stopped bool, reason string) queue.Result {
	res := queue.Result{RequestID: requestID, SessionID: sessionID, StartedAt: started, FinishedAt: time.Now().UTC()}

	if runResult != nil {
		res.SubTurns = runResult.SubTurns
		res.Text = runResult.Text
		res.Result = runResult.Result
		res.CompleteStatus = runResult.CompleteStatus
		res.Usage = &queue.ResultUsage{
			CacheHitTokens:  runResult.Usage.CacheHitTokens,
			CacheMissTokens: runResult.Usage.CacheMissTokens,
			OutputTokens:    runResult.Usage.CompletionTokens,
			ReasoningTokens: runResult.Usage.ReasoningTokens,
			CostUSD:         runResult.Usage.CostUSD,
			PriceTableDate:  p.PriceTableDate,
		}
	}

	switch {
	case runErr == nil && runResult != nil && runResult.Status == store.StatusMaxTurns:
		// Exhausting the sub-turn budget is not an error, but it is not a
		// finished task either. Reporting it as ok would hand the requester a
		// partial run indistinguishable from a complete one.
		res.Status = queue.StatusTimeout
		res.Error = &queue.ResultError{Code: "max_sub_turns", Message: "run stopped at the sub-turn limit without calling Complete"}
	case runErr == nil:
		res.Status = queue.StatusOK
	case stopped:
		res.Status = queue.StatusCancelled
		res.Error = &queue.ResultError{Code: "cancelled", Message: reason}
	case errors.Is(ctxErr, context.DeadlineExceeded):
		res.Status = queue.StatusTimeout
		res.Error = &queue.ResultError{Code: "deadline_exceeded", Message: runErr.Error()}
	default:
		res.Status = queue.StatusFailed
		res.Error = &queue.ResultError{Code: "run_failed", Message: runErr.Error()}
	}
	return res
}

// setupFailedResult reports a request that never reached the session loop
// because its workspace could not be built: a clone that was refused, a
// branch that does not exist, an unwritable root. The session id is carried
// so the failure is attributable to the attempt that owns the directory.
func setupFailedResult(requestID, sessionID string, started time.Time, err error) queue.Result {
	return queue.Result{
		RequestID: requestID, SessionID: sessionID, Status: queue.StatusFailed,
		Error:      &queue.ResultError{Code: "workspace_setup", Message: err.Error()},
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
}

// finish records requestID's outcome, publishes it, and disposes of msg.
// The store write happens before the publish so that a crash between the
// two leaves a terminal row behind: redelivery then republishes the stored
// result instead of running the whole session again. The publish happens
// before the ack (or Term) so a crash between those two redelivers the
// request rather than losing the result docs/DESIGN.md §4.10 asks for.
//
// finish is also where a registered run's registry entry is torn down: the
// entry lives exactly as long as the message does, so a stop arriving while
// finish is still running still finds the run in the registry. The message
// itself is disposed of exactly once by whichever path won the record's
// disposed CompareAndSwap — the run finishing, or the stop escalation —
// which is why finish itself carries no guard of its own.
func (p *Pool) finish(msg jetstream.Msg, requestID, sessionID string, result queue.Result, term bool) {
	defer p.controller().remove(sessionID)

	data, err := json.Marshal(result)
	if err != nil {
		log.Printf("worker: encode result for %s: %v", requestID, err)
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}

	matched, err := p.Store.FinishWorkRequest(context.Background(), requestID, sessionID, result.Status, data, result.FinishedAt)
	if err != nil {
		log.Printf("worker: record finish for %s: %v", requestID, err)
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}
	if !matched {
		// A newer attempt took the row over while this one was still
		// running — a false redelivery, not an actually dead process. This
		// delivery's own job is done either way; acking it just stops it
		// being redelivered again for no purpose. The newer attempt's
		// result is what gets published.
		log.Printf("worker: %s finished under session %q, but the row had already moved on", requestID, sessionID)
		msg.Ack()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.JS.Publish(ctx, queue.FinalSubject(requestID), data, jetstream.WithMsgID(queue.FinalMsgID(requestID))); err != nil {
		log.Printf("worker: publish final result for %s: %v", requestID, err)
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}
	if term {
		msg.Term()
	} else {
		msg.Ack()
	}
}

// republish resends a terminal row's stored result under the same
// Nats-Msg-Id, for a redelivery or a duplicate publish that arrived after
// the original attempt already finished. No store write and no session
// run: the row already says what happened.
func (p *Pool) republish(msg jetstream.Msg, existing store.WorkRequest) {
	if len(existing.Result) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := p.JS.Publish(ctx, queue.FinalSubject(existing.RequestID), existing.Result, jetstream.WithMsgID(queue.FinalMsgID(existing.RequestID))); err != nil {
			log.Printf("worker: republish result for %s: %v", existing.RequestID, err)
			msg.NakWithDelay(p.retryLaterDelay())
			return
		}
	}
	msg.Ack()
}

func (p *Pool) publishAccepted(requestID, sessionID string, started time.Time) {
	data, err := json.Marshal(queue.Accepted{RequestID: requestID, SessionID: sessionID, StartedAt: started})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.JS.Publish(ctx, queue.AcceptedSubject(requestID), data); err != nil {
		log.Printf("worker: publish accepted for %s: %v", requestID, err)
	}
}

func (p *Pool) publishProgress(requestID string, sp session.SubTurnProgress) {
	data, err := json.Marshal(queue.Progress{
		RequestID: requestID,
		SessionID: sp.SessionID,
		SubTurn:   sp.SubTurn,
		ToolCalls: sp.ToolCalls,
		Usage: &queue.ResultUsage{
			CacheHitTokens:  sp.Usage.PromptCacheHitTokens,
			CacheMissTokens: sp.Usage.PromptCacheMissTokens,
			OutputTokens:    sp.Usage.CompletionTokens,
			ReasoningTokens: sp.Usage.ReasoningTokens,
			CostUSD:         sp.Usage.CostUSD,
			PriceTableDate:  p.PriceTableDate,
		},
		ExpectedMissTokens: sp.Usage.ExpectedMissTokens,
		Churned:            sp.Churned,
		ChurnPointIndex:    sp.Usage.ChurnPointIndex,
		Timestamp:          time.Now().UTC(),
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.JS.Publish(ctx, queue.ProgressSubject(requestID), data); err != nil {
		log.Printf("worker: publish progress for %s: %v", requestID, err)
	}
}
