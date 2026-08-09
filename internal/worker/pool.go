// Package worker is the harness's worker pool: it pulls work requests off
// the WORK stream, runs each as a session.Runner call, and publishes the
// result to the RESULTS stream (docs/DESIGN.md §4.10, PLAN.md phase 3).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/workspace"
)

// Pool pulls from Consumer and dispatches each message to a session
// goroutine, bounded by Size. Nothing here holds per-request state outside
// the handler for that request — the property docs/DESIGN.md §4.5 asks a
// phase 3 caller of session.Runner to preserve.
type Pool struct {
	Store    *store.Store
	Runner   *session.Runner
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

	// Size bounds concurrent runs. It must equal the consumer's
	// MaxAckPending (docs/DESIGN.md §4.10) so JetStream never delivers more
	// than the pool can work on; Size is the local backstop, not the flow
	// controller.
	Size int

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

	wg sync.WaitGroup

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

func (p *Pool) defaultDeadline() time.Duration {
	if p.DefaultDeadline > 0 {
		return p.DefaultDeadline
	}
	return 30 * time.Minute
}

func (p *Pool) defaultModel() string {
	if p.DefaultModel != "" {
		return p.DefaultModel
	}
	return "deepseek-v4-pro"
}

func (p *Pool) defaultEffort() string {
	if p.DefaultEffort != "" {
		return p.DefaultEffort
	}
	return "high"
}

func (p *Pool) defaultMaxTokens() int {
	if p.DefaultMaxTokens > 0 {
		return p.DefaultMaxTokens
	}
	return 48000
}

// Run pulls and processes messages until ctx is done. On shutdown it stops
// pulling new work but lets in-flight runs finish and publish normally —
// an agent run takes minutes, and cutting one off on a routine restart
// would waste it for no reason. Only a process that dies outright (the
// kill in PLAN.md's exit criteria) leaves a message for the takeover path
// to pick up.
func (p *Pool) Run(ctx context.Context) error {
	sem := make(chan struct{}, p.size())
	consumeCtx, err := p.Consumer.Consume(func(msg jetstream.Msg) {
		sem <- struct{}{}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-sem }()
			p.handle(msg)
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
// endpoint (docs/DESIGN.md §5.8, PLAN.md phase 6: "Balance ... stops the
// pool") to surface an empty account as a state rather than leave an
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

func deliveryCount(msg jetstream.Msg) uint64 {
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return 1
	}
	return meta.NumDelivered
}

// handle is one message's whole lifecycle: parse, claim the idempotency
// row, and either run a session, record a validation failure, republish a
// terminal row, or defer to a later delivery.
func (p *Pool) handle(msg jetstream.Msg) {
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
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}

	if !outcome.Claimed {
		if outcome.Found && outcome.Existing.Status != store.WorkRequestStatusRunning {
			p.republish(msg, outcome.Existing)
			return
		}
		// A duplicate publish while the original is still genuinely
		// running. Nak would redeliver this exact message and bump its own
		// NumDelivered, which is indistinguishable from the server's own
		// "nobody is heartbeating this" signal — after one such cycle
		// shouldClaim would wrongly read this message as abandoned and
		// take over a request that never stopped running. Wait and
		// heartbeat instead, so the only way NumDelivered ever climbs past
		// 1 is JetStream deciding so on its own.
		p.waitForResolution(msg, req)
		return
	}

	if verr != nil {
		p.recordValidationFailure(ctx, msg, req.RequestID, verr)
		return
	}

	p.run(msg, req, outcome)
}

// waitForResolution holds a message whose request_id is claimed by another,
// live attempt, heartbeating it while polling the row until that attempt
// finishes (then republishes its result) or this request's own deadline
// passes (then Naks as a last resort — see the comment at the call site for
// why that resort is safe). It never runs a session itself.
func (p *Pool) waitForResolution(msg jetstream.Msg, req queue.Request) {
	deadline := p.defaultDeadline()
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
			msg.NakWithDelay(p.retryLaterDelay())
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
// its Term path (PLAN.md phase 3 testing note): a request that never
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
func (p *Pool) run(msg jetstream.Msg, req queue.Request, outcome store.ClaimOutcome) {
	started := time.Now().UTC()
	sessionID := session.NewSessionID()

	hbDone := make(chan struct{})
	go p.heartbeat(msg, hbDone)
	defer close(hbDone)

	deadline := p.defaultDeadline()
	if req.DeadlineMS > 0 {
		deadline = time.Duration(req.DeadlineMS) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	var previousSessionID string
	if outcome.Found {
		previousSessionID = outcome.Existing.SessionID
	}

	if err := p.Store.SetWorkRequestSession(runCtx, req.RequestID, sessionID); err != nil {
		log.Printf("worker: attach session for %s: %v", req.RequestID, err)
	}
	p.publishAccepted(req.RequestID, sessionID, started)

	// Each attempt clones into a directory of its own, named for its session
	// id, so a redelivery never inherits the half-finished tree of the
	// attempt it took over.
	ws, err := p.prepareWorkspace()(runCtx, p.WorkspaceRoot, sessionID, req.Repos)
	if err != nil {
		result := setupFailedResult(req.RequestID, sessionID, started, err)
		p.finish(msg, req.RequestID, sessionID, result, false)
		return
	}

	// Validate has already rejected an absent or unknown mode.
	mode := tools.Mode(req.PermissionMode)
	model := req.Model
	if model == "" {
		model = p.defaultModel()
	}
	effort := req.Effort
	if effort == "" {
		effort = p.defaultEffort()
	}

	progressLimiter := queue.NewProgressLimiter(time.Second)
	runResult, runErr := p.Runner.Run(runCtx, session.RunOptions{
		SessionID:      sessionID,
		Model:          model,
		Effort:         effort,
		Thinking:       p.DefaultThinking,
		MaxTokens:      p.defaultMaxTokens(),
		Workspace:      ws,
		PermissionMode: mode,
		Deny:           req.Deny,
		Prompt:         req.Prompt,
		ResultSchema:   req.ResultSchema,
		MaxSubTurns:    req.MaxSubTurns,
		ParentID:       previousSessionID,
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
	// balance restored (docs/DESIGN.md §4.10's takeover path).
	if runErr != nil && deepseek.IsInsufficientBalance(runErr) {
		p.Halt("account balance exhausted (402 from DeepSeek)")
		log.Printf("worker: %s failed on an empty account; left unacked for retry after the pool restarts", req.RequestID)
		msg.NakWithDelay(p.retryLaterDelay())
		return
	}

	result := p.classify(req.RequestID, sessionID, started, runResult, runErr, runCtx.Err())
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
// expired is the authoritative signal.
func (p *Pool) classify(requestID, sessionID string, started time.Time, runResult *session.RunResult, runErr, ctxErr error) queue.Result {
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
func (p *Pool) finish(msg jetstream.Msg, requestID, sessionID string, result queue.Result, term bool) {
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
