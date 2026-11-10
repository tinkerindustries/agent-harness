package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// GuardCall is the bash call a Guard screens.
type GuardCall struct {
	ID      string
	Command string
	// Workdir is the absolute directory the command runs in, resolved the way
	// `bash` resolves its own workdir argument.
	Workdir string
}

// GuardRefusal says why a Guard stopped a call. Rule is what the model and
// the parent read as the rule that refused it, and Content is the tool result
// text the model reads.
type GuardRefusal struct {
	Rule    string
	Content string
}

// Guard screens a `bash` call before it runs. Executor.Execute asks it only
// under full permission, after the policy has allowed the call. A nil result
// lets the call run.
type Guard interface {
	Screen(ctx context.Context, call GuardCall) *GuardRefusal
}

// screenBash asks the Guard about a bash call. It reports the refusal as a
// denial the way a deny pattern is reported, and nil when the call may run or
// there is nothing to screen.
func (e *Executor) screenBash(ctx context.Context, id, name string, argsRaw json.RawMessage) *Outcome {
	if e.Guard == nil || name != "bash" || e.Policy.Mode != ModeFull {
		return nil
	}
	var args bashArgs
	if json.Unmarshal(argsRaw, &args) != nil || args.Command == "" {
		return nil
	}
	dir, err := e.bashWorkdir(args.Workdir)
	if err != nil {
		// `bash` refuses this call itself, so there is nothing to screen.
		return nil
	}
	refusal := e.Guard.Screen(ctx, GuardCall{ID: id, Command: args.Command, Workdir: dir})
	if refusal == nil {
		return nil
	}
	content := refusal.Content
	if content == "" {
		content = fmt.Sprintf("Denied by the autonomy guard: %s", refusal.Rule)
	}
	return &Outcome{
		Denied: true,
		Rule:   refusal.Rule,
		Name:   name,
		Result: Result{Content: content, IsError: true},
	}
}
