package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

const (
	// guardDecideTimeout caps a shadow check on a context that outlives the
	// run. Each stage client has a shorter deadline of its own.
	guardDecideTimeout = 60 * time.Second
	// guardCommitTimeout bounds the append of a decision.
	guardCommitTimeout = 10 * time.Second
	// guardDrainTimeout bounds how long a run that is ending waits for shadow
	// checks still in flight.
	guardDrainTimeout = 45 * time.Second
)

// GuardDecider is the autonomy guard's pipeline. *guard.Pipeline satisfies it.
type GuardDecider interface {
	Decide(ctx context.Context, state guard.State) guard.Decision
}

// GuardFactory builds the decider for a session's guard setting. cmd/harness
// supplies it, since it holds the Anthropic credential and the stage clients.
type GuardFactory func(setting store.GuardSetting) (GuardDecider, error)

// runGuard is the tools.Guard a run's Executor carries. It lives for one run;
// a `task` subagent builds its own from the same setting.
type runGuard struct {
	runner  *Runner
	decider GuardDecider
	mode    string
	builder guard.StateBuilder
	pending sync.WaitGroup
}

// newRunGuard returns the guard for a run under setting and permission mode,
// or nil when the run is not screened: the setting is off, or the session
// cannot run `bash` at all.
func (r *Runner) newRunGuard(setting store.GuardSetting, mode tools.Mode) (*runGuard, error) {
	if !setting.Active() || mode != tools.ModeFull {
		return nil, nil
	}
	if r.Guards == nil {
		return nil, fmt.Errorf("session: the autonomy guard is %s but this runner has no guard configured", setting.Mode)
	}
	decider, err := r.Guards(setting)
	if err != nil {
		return nil, fmt.Errorf("session: build the autonomy guard: %w", err)
	}
	return &runGuard{runner: r, decider: decider, mode: setting.Mode}, nil
}

type guardCtxKey struct{}

// guardCallScope is what executeToolCall tells the guard about the call: the
// session whose log the decision joins, and the other calls of its round.
type guardCallScope struct {
	sess  store.Session
	round []store.ToolCallPayload
}

func withGuardScope(ctx context.Context, sess store.Session, round []store.ToolCallPayload) context.Context {
	return context.WithValue(ctx, guardCtxKey{}, guardCallScope{sess: sess, round: round})
}

// Screen implements tools.Guard.
func (g *runGuard) Screen(ctx context.Context, call tools.GuardCall) *tools.GuardRefusal {
	scope, ok := ctx.Value(guardCtxKey{}).(guardCallScope)
	if !ok {
		log.Printf("session: autonomy guard has no session scope for %s", call.ID)
		return g.unavailable()
	}
	state, err := g.state(ctx, scope, call)
	if err != nil {
		log.Printf("session: autonomy guard state for %s: %v", call.ID, err)
		return g.unavailable()
	}
	if g.mode == store.GuardModeShadow {
		g.pending.Add(1)
		go g.shadow(ctx, scope.sess, call.ID, state)
		return nil
	}
	d := g.decider.Decide(ctx, state)
	if ctx.Err() != nil {
		return refusalFor(guard.Decision{Verdict: guard.VerdictBlock, DecidedBy: guard.DecidedByStage2})
	}
	g.record(scope.sess, call.ID, d)
	if d.Verdict == guard.VerdictBlock {
		return refusalFor(d)
	}
	return nil
}

func (g *runGuard) state(ctx context.Context, scope guardCallScope, call tools.GuardCall) (guard.State, error) {
	hist, err := LoadGuardHistory(ctx, g.runner.Store, scope.sess)
	if err != nil {
		return guard.State{}, err
	}
	return BuildGuardState(ctx, g.builder, scope.sess.Workspace, hist, GuardCall{
		ID: call.ID, Command: call.Command, Workdir: call.Workdir, Round: scope.round,
	}), nil
}

// unavailable is what a mode that refuses does when it cannot build the state
// the check needs. Shadow lets the command run.
func (g *runGuard) unavailable() *tools.GuardRefusal {
	if g.mode != store.GuardModeEnforce {
		return nil
	}
	return &tools.GuardRefusal{
		Rule:    "autonomy guard: unavailable",
		Content: "Denied by the autonomy guard: it could not read the session to check this command. The command was not run.",
	}
}

// shadow runs the check beside the command. The decision is made and
// committed on contexts that a cancelled run does not cancel, because a
// cancelled stage call would be recorded as a stage failure.
func (g *runGuard) shadow(ctx context.Context, sess store.Session, callID string, state guard.State) {
	defer g.pending.Done()
	decideCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guardDecideTimeout)
	defer cancel()
	g.record(sess, callID, g.decider.Decide(decideCtx, state))
}

// record appends the decision, and stage 2's usage, to the session's log.
func (g *runGuard) record(sess store.Session, callID string, d guard.Decision) {
	r := g.runner
	ctx, cancel := context.WithTimeout(context.Background(), guardCommitTimeout)
	defer cancel()
	appended, err := r.Store.AppendEvents(ctx, sess.ID, GuardEvents(callID, g.mode, d, r.Prices, time.Now().UTC()))
	if err != nil {
		log.Printf("session: record autonomy guard decision for %s in %s: %v", callID, sess.ID, err)
		return
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
}

// Drain waits for shadow checks still in flight, so their records reach the
// parent before the run's last event closes the stream. It gives up when ctx
// ends or after guardDrainTimeout. The checks carry on and commit on their
// own.
func (g *runGuard) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		g.pending.Wait()
		close(done)
	}()
	timer := time.NewTimer(guardDrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// refusalFor is the refusal for a blocked decision. It names the guard, the
// stage and, when the rules decided, their names. It never carries stage 2's
// reason.
func refusalFor(d guard.Decision) *tools.GuardRefusal {
	who := "stage 2"
	switch d.DecidedBy {
	case guard.DecidedByRules:
		who = "rules"
		if len(d.Rules) > 0 {
			who += ": " + strings.Join(d.Rules, ", ")
		}
	case guard.DecidedByStage1:
		who = "stage 1"
	}
	return &tools.GuardRefusal{
		Rule:    "autonomy guard (" + who + ")",
		Content: fmt.Sprintf("Denied by the autonomy guard (%s). The command was not run.", who),
	}
}

// drainGuard lets a run that is about to end collect the shadow records still
// in flight. A cancelled run does not wait.
func drainGuard(ctx context.Context, executor *tools.Executor) {
	if ctx.Err() != nil {
		return
	}
	if g, ok := executor.Guard.(*runGuard); ok {
		g.Drain(ctx)
	}
}

func toolCallPayloads(calls []wire.AssembledToolCall) []store.ToolCallPayload {
	out := make([]store.ToolCallPayload, len(calls))
	for i, c := range calls {
		out[i] = store.ToolCallPayload{Index: i, ID: c.ID, Name: c.Name, Arguments: c.Arguments}
	}
	return out
}
