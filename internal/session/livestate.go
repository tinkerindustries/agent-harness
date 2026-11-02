package session

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// livestate.go: persisting the working plan and the rolling recent-tool-calls
// list a running session's row carries, written alongside each other in one
// store write.

// persistLiveState writes this sub-turn's recent-tool-call roll to the
// session row. The runner calls it where it
// already appends the tool_call events — the one component that sees every
// tool call and already holds the store handle — rather than inside
// tools.Executor, which deliberately does not import internal/store. The
// plan column is written separately by persistTaskState, after the tool
// calls have run, because a task_create/task_update's effect (minted ids,
// patched fields) only exists once the handler actually executes.
// task_create and task_update themselves are not rolled into the recent
// calls: the plan panel already says what they said. task_get and task_list
// are non-mutating reads, so they roll like any other tool.
func (r *Runner) persistLiveState(ctx context.Context, sess store.Session, calls []wire.AssembledToolCall) {
	var recent []store.RecentToolCall
	for _, c := range calls {
		if c.Name == "task_create" || c.Name == "task_update" {
			continue
		}
		recent = append(recent, store.RecentToolCall{
			Name:      c.Name,
			Arguments: c.Arguments,
			CreatedAt: time.Now().UTC(),
		})
	}
	if len(recent) == 0 {
		return
	}
	if err := r.Store.UpdateSessionLiveState(ctx, sess.ID, "", recent); err != nil {
		log.Printf("session: persist live state for %s: %v", sess.ID, err)
	}
}

// persistTaskState writes this sub-turn's plan to the session row, once the
// tool calls have actually run. It is a no-op unless the sub-turn carried a
// task_create or task_update — the only two tools that mutate the plan — so a
// task_get/task_list-only sub-turn never clobbers the row, and the
// recent-tool-call roll for this sub-turn was already handled by
// persistLiveState. The snapshot comes from executor.Todos() after
// execution, the only source of truth for minted ids and applied patches;
// UpdateSessionLiveState is called with a nil calls argument so this write
// touches the plan column only.
func (r *Runner) persistTaskState(ctx context.Context, sess store.Session, executor *tools.Executor, calls []wire.AssembledToolCall) {
	mutates := false
	for _, c := range calls {
		if c.Name == "task_create" || c.Name == "task_update" {
			mutates = true
			break
		}
	}
	if !mutates {
		return
	}
	plan := planSnapshot(executor.Todos())
	if err := r.Store.UpdateSessionLiveState(ctx, sess.ID, plan, nil); err != nil {
		log.Printf("session: persist task state for %s: %v", sess.ID, err)
		return
	}
	// sess is a copy passed in by the caller; its Plan field predates the
	// write above (it was reloaded, if at all, before this sub-turn's
	// task_create/task_update ran). Set it directly from the value just
	// written rather than reloading from the store again — publishState
	// only reads this copy, so mutating it here is enough to avoid the same
	// stale-publish bug turn.go's reload fixes for the roll.
	sess.Plan = plan
	r.publishState(ctx, sess)
}

// planSnapshot renders the executor's current plan as the JSON todos array
// the session row's plan column stores — store.StatusTodo, the same shape
// the status endpoint replays. The conversion is explicit field by field
// because there is no single raw-arguments source of truth anymore: the
// plan lives in the executor, mutated by task_create and task_update.
func planSnapshot(todos []tools.Todo) string {
	out := make([]store.StatusTodo, len(todos))
	for i, t := range todos {
		out[i] = store.StatusTodo{
			TaskID:      t.TaskID,
			Subject:     t.Subject,
			Description: t.Description,
			Status:      t.Status,
			ActiveForm:  t.ActiveForm,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
