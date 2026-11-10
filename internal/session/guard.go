package session

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// ApprovalRequest is what the loop asks its parent when the classifier did not
// allow a `bash` call: the call, and why the classifier did not.
type ApprovalRequest struct {
	SessionID string
	CallID    string
	Command   string
	// Workdir is the directory the command runs in.
	Workdir string
	// Verdict is guard.Block or guard.Unreachable.
	Verdict string
	// Reason is the classifier's explanation for a block, or why it was
	// unreachable. It is for the parent and a person: the model never reads it.
	Reason string
}

// ApprovalAnswer is the parent's decision.
type ApprovalAnswer struct {
	Approve bool
	// Message is the person's note for a refusal. The model reads it.
	Message string
}

// Approver asks the parent about one call and waits. An error, including a
// cancelled ctx, is a refusal: nothing the loop does leaves a call running
// because nobody answered.
type Approver func(ctx context.Context, req ApprovalRequest) (ApprovalAnswer, error)

// runGuard is the tools.Guard a run's Executor carries. It lives for one run;
// a `task` subagent builds its own from the same setting.
//
// The effect of the classifier is this and nothing more: the permission mode
// and `deny` patterns have already refused what they refuse before Screen is
// asked (tools.Executor.Execute), so an allow never widens them. Allow runs
// the call without asking. Block, or an endpoint that does not answer, asks
// the parent, and the call runs only on an approval. A verdict on its own
// never refuses a call.
type runGuard struct {
	classify *guard.Client
	approve  Approver
	setting  store.GuardSetting
}

// newRunGuard returns the guard for a run under setting and permission mode,
// or nil when the run is not screened: the setting is off, or the session
// cannot run `bash` at all.
func (r *Runner) newRunGuard(setting store.GuardSetting, mode tools.Mode) *runGuard {
	if !setting.Active() || mode != tools.ModeFull {
		return nil
	}
	return &runGuard{
		classify: &guard.Client{URL: setting.ClassifyURL, HTTP: r.GuardHTTP},
		approve:  r.Approver,
		setting:  setting,
	}
}

type guardCtxKey struct{}

func withGuardScope(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, guardCtxKey{}, sessionID)
}

// Screen implements tools.Guard.
func (g *runGuard) Screen(ctx context.Context, call tools.GuardCall) *tools.GuardRefusal {
	sessionID, _ := ctx.Value(guardCtxKey{}).(string)
	dir := call.Workdir
	engine := g.setting.Engine
	if engine == "" {
		engine = "agent-harness"
	}
	res := g.classify.Classify(ctx, guard.Request{
		Command: call.Command, Shell: "bash", CWD: dir, Branch: guard.Branch(ctx, dir),
		Scenario: g.setting.Scenario, Engine: engine, SessionID: sessionID, CallID: call.ID,
	})
	if ctx.Err() != nil {
		return refusal("the run was stopped before the command was checked")
	}
	if res.Verdict == guard.Allow {
		return nil
	}
	reason := res.Reason
	if res.Verdict == guard.Unreachable {
		reason = "the classifier did not answer: " + res.Err
	}
	if g.approve == nil {
		log.Printf("session: autonomy guard has no approver for %s; refusing", call.ID)
		return refusal("nobody was available to approve it")
	}
	ans, err := g.approve(ctx, ApprovalRequest{
		SessionID: sessionID, CallID: call.ID, Command: call.Command, Workdir: dir,
		Verdict: res.Verdict, Reason: reason,
	})
	if err != nil || ctx.Err() != nil {
		return refusal("it was not approved")
	}
	if !ans.Approve {
		if msg := strings.TrimSpace(ans.Message); msg != "" {
			return refusal("it was not approved: " + msg)
		}
		return refusal("it was not approved")
	}
	return nil
}

// refusal is the tool result for a refused call. It never carries the
// classifier's reason, so a model cannot learn from it what to rephrase.
func refusal(why string) *tools.GuardRefusal {
	return &tools.GuardRefusal{
		Rule:    "autonomy guard",
		Content: fmt.Sprintf("Denied by the autonomy guard: %s. The command was not run.", why),
	}
}
