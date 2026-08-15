// Package worker is the harness's worker pool: it pulls work requests off
// the WORK stream, runs each as a session.Runner call, and records the
// result on the request's work_requests row (docs/DESIGN.md §4.10).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/kimi"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/workspace"
)

// Runner runs one session to a terminal result. *session.Runner implements
// it; the interface exists so the pool's tests can drive a run that ignores
// its context entirely — a genuinely wedged run — without calling an API.
// Create and FailSetup are the preparation window's two bookends: Create
// inserts the session row as "creating" before the workspace is built, so a
// run is visible and stoppable from the moment it is claimed, and FailSetup
// moves that row to "failed" when preparation fails instead of leaving it
// stuck.
type Runner interface {
	Create(ctx context.Context, opts session.RunOptions) error
	FailSetup(ctx context.Context, sessionID string, cause error) error
	Run(ctx context.Context, opts session.RunOptions) (*session.RunResult, error)
}

// Pool pulls from Consumer and dispatches each message to a session
// goroutine, bounded by Size. Nothing here holds per-request state outside
// the handler for that request — the property docs/DESIGN.md §4.5 asks
// every caller of session.Runner to preserve.
type Pool struct {
	Store    *store.Store
	Runner   Runner
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
	// without cloning over the network. attachments are the request's
	// materialised images (workspace.Attachment), fetched from the store by
	// id before this is called.
	PrepareWorkspace func(ctx context.Context, root, sessionID string, repos []queue.Repo, attachments []workspace.Attachment) (string, error)

	// SkillsFS is the skill tree every prepared workspace gets a copy of,
	// supplied by cmd/harness as assets.AgentSkills(). Nil ships none, which
	// is what every test that does not care about skills leaves it as.
	SkillsFS fs.FS

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

func (p *Pool) prepareWorkspace() func(context.Context, string, string, []queue.Repo, []workspace.Attachment) (string, error) {
	if p.PrepareWorkspace != nil {
		return p.PrepareWorkspace
	}
	return workspace.Prepare
}

// loadAttachments reads the request's attachments from the store by id,
// returning them as workspace.Attachment (the shape Prepare materialises)
// and their names (the shape the opening message names). A request carrying
// an id with no row — the row was cleaned up, say — is a setup failure
// naming the id: the run cannot do what its caller asked without the file.
func (p *Pool) loadAttachments(ctx context.Context, ids []string) ([]workspace.Attachment, []string, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	attachments := make([]workspace.Attachment, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		att, err := p.Store.GetAttachment(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil, fmt.Errorf("attachment %s named by the request is not in the store", id)
			}
			return nil, nil, fmt.Errorf("read attachment %s: %w", id, err)
		}
		attachments = append(attachments, workspace.Attachment{Name: att.Name, MIMEType: att.MIMEType, Data: att.Data})
		names = append(names, att.Name)
	}
	return attachments, names, nil
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
			p.handle(queue.WrapNATS(msg), release)
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
// keep going and still publish their results normally. An empty account —
// DeepSeek's 402, Kimi's 429 with error type exceeded_current_quota_error —
// means the balance is gone and every other queued request would hit the
// identical wall, so the pool stops instead of failing them one at a time
// (docs/DESIGN.md §4.5, §4.10). Calling Halt more than once, or before
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
func (p *Pool) retryLater(msg queue.Msg, requestID, code, message string) {
	if requestID == "" || msg.DeliveryCount() < p.maxDeliveryAttempts() {
		msg.Nak(p.retryLaterDelay())
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

// handle is one message's whole lifecycle: parse, claim the idempotency
// row, and either run a session, record a validation failure, republish a
// terminal row, or defer to a later delivery. releaseSlot is the run's
// idempotent pool-slot release, threaded down to the run path so the stop
// escalation can free the slot of a run that wedges (docs/RUN-CONTROL.md
// "Half two"); the other paths never register and never release.
func (p *Pool) handle(msg queue.Msg, releaseSlot func()) {
	defer p.recoverPanic(msg)

	numDelivered := msg.DeliveryCount()

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
// abandoned session — the row CloseSession was built for — publishes a
// failed result telling the caller what happened and how to retry, and
// terminates the message: no further delivery can help.
func (p *Pool) handleSpent(msg queue.Msg, req queue.Request, outcome store.ClaimOutcome) {
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
func (p *Pool) waitForResolution(msg queue.Msg, req queue.Request) {
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

func (p *Pool) recoverPanic(msg queue.Msg) {
	if r := recover(); r != nil {
		log.Printf("worker: recovered panic handling message: %v\n%s", r, debug.Stack())
		msg.Nak(p.retryLaterDelay())
	}
}

// recordValidationFailure is the request and result schema, validation and
// its Term path: a request that never
// authorizes itself is recorded as failed under no session (sessionID ""),
// published once, and Term'd so it is never redelivered.
func (p *Pool) recordValidationFailure(ctx context.Context, msg queue.Msg, requestID string, verr error) {
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
func (p *Pool) run(msg queue.Msg, req queue.Request, releaseSlot func()) {
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
	// The session id is on the row now, which is what deepseek_agent's
	// accepted wait reads back (docs/QUEUE-MIGRATION-PLAN.md §5.2); there is
	// no separate accepted message to publish.

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

	// The session row is created as "creating" before any preparation runs,
	// with the workspace path this attempt is about to clone into, so the
	// run is visible on the session list and stoppable from the moment it is
	// claimed — a stop during a clone has a row to mark. Runner.Run promotes
	// the row to "running" once preparation succeeds. A Create failure is a
	// setup failure on the existing path: there is no row to mark, because
	// it never existed.
	runOpts := session.RunOptions{
		SessionID:       sessionID,
		Model:           model,
		Effort:          effort,
		Thinking:        p.DefaultThinking,
		MaxTokens:       p.defaultMaxTokens(runCtx),
		Workspace:       filepath.Join(p.WorkspaceRoot, sessionID),
		PermissionMode:  mode,
		Deny:            req.Deny,
		Prompt:          req.Prompt,
		ResultSchema:    req.ResultSchema,
		MaxSubTurns:     req.MaxSubTurns,
		JobType:         req.JobType,
		Title:           req.Title,
		Description:     req.Description,
		Phase:           req.Phase,
		TotalPhases:     req.TotalPhases,
		ParentAgentType: req.ParentAgentType,
		ParentAgentID:   req.ParentAgentID,
		ParentIsUser:    req.ParentIsUser,
		PromptVariant:   req.PromptVariant,
		ReminderPolicy:  req.ReminderPolicy,
	}
	if err := p.Runner.Create(runCtx, runOpts); err != nil {
		if !rec.answer() {
			log.Printf("worker: %s: message for session %s already answered; dropping the setup failure", req.RequestID, sessionID)
			return
		}
		result := setupFailedResult(req.RequestID, sessionID, started, err)
		p.finish(msg, req.RequestID, sessionID, result, false)
		return
	}

	// Each attempt clones into a directory of its own, named for its session
	// id, so a redelivery never inherits the half-finished tree of an
	// attempt that died before its session existed. The request's attachments
	// are fetched from the store first — the request carries only ids, the
	// bytes live in the database (docs/DATA-API.md) — and materialised into
	// scratch/attachments/ by Prepare; the names ride to the opening message
	// so the model knows the files exist.
	attachments, attachmentNames, err := p.loadAttachments(runCtx, req.AttachmentIDs)
	if err != nil {
		if !rec.answer() {
			log.Printf("worker: %s: message for session %s already answered; dropping the setup failure", req.RequestID, sessionID)
			return
		}
		p.failSetup(req.RequestID, sessionID, err)
		result := setupFailedResult(req.RequestID, sessionID, started, err)
		p.finish(msg, req.RequestID, sessionID, result, false)
		return
	}
	ws, err := p.prepareWorkspace()(runCtx, p.WorkspaceRoot, sessionID, req.Repos, attachments)
	if err != nil {
		if !rec.answer() {
			// A stop force-finished this run while preparation was wedged;
			// the cancelled result is already on the stream.
			log.Printf("worker: %s: message for session %s already answered; dropping the setup failure", req.RequestID, sessionID)
			return
		}
		p.failSetup(req.RequestID, sessionID, err)
		result := setupFailedResult(req.RequestID, sessionID, started, err)
		p.finish(msg, req.RequestID, sessionID, result, false)
		return
	}

	// The skills this build ships, written into the workspace's own skills/
	// directory so discovery lists them beside whatever the cloned
	// repositories carry (internal/skills.Install). It runs after preparation
	// rather than inside it so that a test overriding PrepareWorkspace still
	// exercises this, and it never fails the run: a skill that did not land
	// is one catalogue entry missing, and the task the request asked for does
	// not depend on it.
	if n, err := skills.Install(p.SkillsFS, ws); err != nil {
		log.Printf("worker: %s: session %s: installing shipped skills: %v", req.RequestID, sessionID, err)
	} else if n > 0 {
		log.Printf("worker: %s: session %s: installed %d shipped skill(s)", req.RequestID, sessionID, n)
	}

	runOpts.Workspace = ws
	runOpts.AttachmentNames = attachmentNames
	runResult, runErr := p.Runner.Run(runCtx, runOpts)

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
	//
	// Which error means "empty account" is provider-specific: DeepSeek
	// answers 402, Kimi answers 429 with error type
	// exceeded_current_quota_error (third_party/kimi-docs/api/errors.md,
	// api/balance.md), and either halts the pool the same way.
	if runErr != nil && (deepseek.IsInsufficientBalance(runErr) || kimi.IsInsufficientBalance(runErr)) {
		if !rec.answer() {
			// A stop already answered this message; there is nothing to
			// defer to a later delivery.
			return
		}
		p.Halt(fmt.Sprintf("account balance exhausted: %v", runErr))
		log.Printf("worker: %s failed on an empty account; left unacked for retry after the pool restarts", req.RequestID)
		// One delivery is spent per pool restart that still finds the
		// account empty, so this waits for balance across restarts as
		// before — but within the ceiling, and the last attempt says the
		// account was empty instead of the request disappearing.
		p.retryLater(msg, req.RequestID, "insufficient_balance",
			fmt.Sprintf("the model account had no balance on each of %d delivery attempts", p.maxDeliveryAttempts()))
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

func (p *Pool) heartbeat(msg queue.Msg, done <-chan struct{}) {
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
	case runErr == nil && runResult != nil && runResult.Reason == session.ReasonCompleteRejected:
		// The run ended because Complete could not get its result past the
		// schema (session.maxCompleteRejections). The work may well have been
		// done, but the payload the requester asked for does not exist, and
		// reporting ok would hand back a null result as though the model had
		// simply chosen not to call Complete.
		res.Status = queue.StatusFailed
		res.Error = &queue.ResultError{
			Code:    "complete_rejected",
			Message: "run ended after repeated Complete calls failed result_schema validation",
		}
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

// failSetup marks a session whose workspace preparation failed, so no row is
// ever left stuck in "creating": the row moves to failed with an error event
// (Runner.FailSetup), and the session page shows why the run never started.
// A failure to record is logged rather than changing the setup result — the
// caller already won the message, and the result publish is what matters.
func (p *Pool) failSetup(requestID, sessionID string, cause error) {
	if err := p.Runner.FailSetup(context.Background(), sessionID, cause); err != nil {
		log.Printf("worker: %s: fail setup for session %s: %v", requestID, sessionID, err)
	}
}

// finish records requestID's outcome on the work_requests row and disposes
// of msg. The row is the result: the worker writes it with FinishWorkRequest
// and then acks (or Terms), so there is no separate publish to fail. A crash
// between the write and the ack redelivers the request, and the spent path
// then reads the terminal row back out instead of running the session again
// (docs/QUEUE-MIGRATION-PLAN.md §1.6).
//
// finish is also where a registered run's registry entry is torn down: the
// entry lives exactly as long as the message does, so a stop arriving while
// finish is still running still finds the run in the registry. The message
// itself is disposed of exactly once by whichever path won the record's
// disposed CompareAndSwap — the run finishing, or the stop escalation —
// which is why finish itself carries no guard of its own.
func (p *Pool) finish(msg queue.Msg, requestID, sessionID string, result queue.Result, term bool) {
	defer p.controller().remove(sessionID)

	data, err := json.Marshal(result)
	if err != nil {
		log.Printf("worker: encode result for %s: %v", requestID, err)
		msg.Nak(p.retryLaterDelay())
		return
	}

	matched, err := p.Store.FinishWorkRequest(context.Background(), requestID, sessionID, result.Status, data, result.FinishedAt)
	if err != nil {
		log.Printf("worker: record finish for %s: %v", requestID, err)
		msg.Nak(p.retryLaterDelay())
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

	if term {
		msg.Term()
	} else {
		msg.Ack()
	}
}

// republish acks a redelivery or a duplicate publish that arrived after the
// original attempt already finished. No store write, no publish, and no
// session run: the row already holds the terminal result, which is what the
// caller reads back over GET /api/requests/{request_id}, so there is
// nothing to resend (docs/QUEUE-MIGRATION-PLAN.md §1.6).
func (p *Pool) republish(msg queue.Msg, existing store.WorkRequest) {
	msg.Ack()
}
