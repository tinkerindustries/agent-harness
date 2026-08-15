package worker

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// ErrRunNotFound is returned by Pool.Stop when no run in this process owns
// the named session (docs/RUN-CONTROL.md "Stopping is addressed at one
// goroutine, so the seam is a registry"). The HTTP handler turns it
// into a 404; here it is the whole contract.
var ErrRunNotFound = errors.New("worker: no run in this process owns that session")

// Controller is the pool's registry of in-flight runs, keyed by session id.
// One entry exists from the moment Pool.run generates a session id until
// that run's message is disposed of, whether by the run finishing or by a
// stop taking it over. Nothing about it touches the queue: control is
// real-time, addressed at one specific in-flight goroutine, and actively
// wrong to make redeliverable (docs/RUN-CONTROL.md "Stopping is addressed
// at one goroutine, so the seam is a registry").
type Controller struct {
	mu   sync.Mutex
	runs map[string]*inflight
}

func newController() *Controller {
	return &Controller{runs: make(map[string]*inflight)}
}

func (c *Controller) add(r *inflight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs[r.sessionID] = r
}

func (c *Controller) remove(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.runs, sessionID)
}

func (c *Controller) lookup(sessionID string) (*inflight, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[sessionID]
	return r, ok
}

// inflight is one run's control handle, shared by the run goroutine and the
// stop path. The run goroutine creates it when it registers and the two
// paths — the run finishing and the escalation force-finishing — race on
// disposed to decide who answers the message; releaseSlot and stopHeartbeat
// are the closures both paths must be able to call exactly once.
type inflight struct {
	requestID string
	sessionID string
	// msg is the claimed queue message this run answers, so the escalation can
	// run the same finish sequence — record the row, publish, ack — an
	// ordinary run would have.
	msg queue.Msg
	// cancel cancels runCtx: the soft stop. A healthy run ends at its next
	// check point; a wedged one does not, which is what the escalation is for.
	cancel context.CancelFunc
	// done is closed when Runner.Run returns.
	done chan struct{}
	// releaseSlot idempotently frees the pool semaphore token. The run
	// goroutine's own deferred release becomes a no-op once the escalation
	// has called this, so a wedged goroutine that wakes cannot hold the slot
	// hostage a second time.
	releaseSlot func()
	// stopHeartbeat idempotently stops the message heartbeat. Both paths go
	// through this one closer, so the run goroutine's own deferred close
	// cannot panic on a channel the escalation already closed.
	stopHeartbeat func()
	// started is when the run began, carried on the force-finish result so
	// the cancelled result is as honest about timing as any other.
	started time.Time
	// stopping records that a stop has been accepted for this run. It, not
	// the context error, is what classifies a stopped run as cancelled
	// rather than timed out: a stop and a deadline both leave runCtx
	// cancelled.
	stopping atomic.Bool
	// disposed records that the queue message has been answered. Whoever wins
	// the CompareAndSwap owns the message; everyone else logs and drops its
	// result.
	disposed atomic.Bool
	// reason is the stop's reason string, the cancelled result's message.
	reason atomic.Pointer[string]
}

func (r *inflight) stopReason() string {
	if p := r.reason.Load(); p != nil {
		return *p
	}
	return ""
}

// answer claims the right to dispose of the run's message. The first caller
// wins; a wedged goroutine that wakes after the fact loses and must drop its
// own result instead of publishing a second, contradictory one over the top
// of the first and acking an already-acked message (docs/RUN-CONTROL.md
// "Half two"). The record is the guard's home rather than the registry, so
// the guard holds even after finish has deregistered the entry.
func (r *inflight) answer() bool {
	return r.disposed.CompareAndSwap(false, true)
}

// controller returns the pool's run registry, creating it on first use so
// Stop and Running work whether or not Run has started pulling.
func (p *Pool) controller() *Controller {
	p.ctrlOnce.Do(func() { p.ctrl = newController() })
	return p.ctrl
}

// stopGracePeriod resolves run.stop_grace_period the way the other
// per-request settings resolve — read through the store on every call, so a
// key changed with `harness config set` applies to the next stop — with
// StopGracePeriod as the test override and 30s as the fallback.
func (p *Pool) stopGracePeriod(ctx context.Context) time.Duration {
	if p.StopGracePeriod > 0 {
		return p.StopGracePeriod
	}
	if p.Settings != nil {
		if v, err := p.Settings.Duration(ctx, settings.KeyRunStopGracePeriod); err == nil {
			return v
		}
	}
	return 30 * time.Second
}

// Stop begins ending sessionID and returns immediately. ErrRunNotFound when
// no run in this process owns that session; nil both for a stop it accepted
// and for one already in progress.
//
// The soft stop is rec.cancel(): a healthy run ends at its next check point
// the way a deadline already does. Everything after the return happens in a
// goroutine (escalateStop), which is why this path is non-blocking and why
// the endpoint answers 202.
func (p *Pool) Stop(sessionID, reason string) error {
	rec, ok := p.controller().lookup(sessionID)
	if !ok {
		return ErrRunNotFound
	}
	if rec.stopping.Load() {
		// A stop is already in progress for this run; a second stop is not
		// an error (docs/RUN-CONTROL.md "Half two").
		return nil
	}
	rec.reason.Store(&reason)
	if !rec.stopping.CompareAndSwap(false, true) {
		// Two stops raced; the other one accepted the stop.
		return nil
	}
	rec.cancel()
	go p.escalateStop(rec)
	return nil
}

// Running reports whether this process is running sessionID.
func (p *Pool) Running(sessionID string) bool {
	_, ok := p.controller().lookup(sessionID)
	return ok
}

// escalateStop is the second half of a stop, the escalation for the runs
// that a cancelled context does not reach: a wedged goroutine blocked in a
// syscall no Go-level cancellation can unblock (docs/RUN-CONTROL.md "Half
// two: the escalation, for the ones that still get through"). It waits out
// the grace period for the run to end on its own and, when it does not,
// force-finishes it: the session row is marked cancelled, a cancelled result
// is published and the message acked, the heartbeat is stopped, and the pool
// slot is released.
func (p *Pool) escalateStop(rec *inflight) {
	grace := p.stopGracePeriod(context.Background())
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-rec.done:
		// The run ended on its own inside the grace period; its ordinary
		// finish path already published the result and acked the message.
		return
	case <-timer.C:
	}

	now := time.Now().UTC()
	if err := p.Store.CancelRunningSession(context.Background(), rec.sessionID, now); err != nil {
		var sfe *store.SessionFinishedError
		if errors.As(err, &sfe) {
			// The run finished on its own in the gap between the stop being
			// asked for and the force-finish landing. Relabelling a completed
			// run as cancelled would destroy the one distinction its terminal
			// status carries, so nothing is published here — the run's own
			// result is already on its way. This error exists for exactly
			// this race.
			log.Printf("worker: stop of session %s: run finished as %q in the gap; leaving its result alone",
				rec.sessionID, sfe.Status)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("worker: stop of session %s: cancel running session: %v", rec.sessionID, err)
			return
		}
		// The session row never existed — the stop landed in the instant
		// between the run registering and its session row being created.
		// There is nothing to mark, but the caller still gets their answer
		// below. (A stop during workspace preparation itself is the reason
		// the row is created before the clone starts: CancelRunningSession
		// accepts "creating", so that case marks the row instead of reaching
		// here.)
		log.Printf("worker: stop of session %s: no session row to mark (run still starting); force-finishing anyway",
			rec.sessionID)
	}

	if !rec.answer() {
		// The run goroutine answered the message in the gap between the
		// cancel and here; its result is already published and acked.
		return
	}

	result := queue.Result{
		RequestID:  rec.requestID,
		SessionID:  rec.sessionID,
		Status:     queue.StatusCancelled,
		Error:      &queue.ResultError{Code: "cancelled", Message: rec.stopReason()},
		StartedAt:  rec.started,
		FinishedAt: now,
	}
	p.finish(rec.msg, rec.requestID, rec.sessionID, result, false)

	rec.stopHeartbeat()
	rec.releaseSlot()
	log.Printf("worker: %s (session %s) did not stop within %s; force-finished as cancelled, heartbeat stopped, slot released (leaked goroutine)",
		rec.requestID, rec.sessionID, grace)
}
