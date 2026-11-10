package session

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/httplog"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// tooldispatch.go: executing one sub-turn's tool calls in tool_calls array
// order (the append-only invariant tool results depend on), and the stdout
// sink a long-running tool streams through.

// executeToolCalls runs every call concurrently and returns outcomes in the
// same order as calls, regardless of completion order — the appender is
// what must preserve tool_calls order, not the execution itself
// (docs/TOOLS.md). The four `task` tools are the exception and run
// synchronously, in calls order, interleaved with the goroutine spawning:
// task_create mints ids from a counter in execution order, and the store's
// plan replay (internal/store/status.go) and the frontend both reconstruct
// those ids by walking the event log in the same calls order, so letting
// two task_create calls race for the executor's lock would hand the model
// ids nobody else can reconstruct.
//
// Calls that rewrite the same file are the other exception: they run in one
// goroutine, in calls order, so the file is never the prize in a race.
// `edit`, `write` and `apply_patch` all read a whole file, apply a change to their
// own copy, and write it back, so two of them racing on one path silently
// drop whichever finishes first — the model is told both succeeded and one of
// its changes is simply gone. Ordering them makes disjoint edits both land,
// and turns the genuinely conflicting case into `edit`'s own "old_string not
// found" with a note saying why (tools.MutationTargets picks the groups out).
// Files are grouped separately, so different files still run concurrently,
// and a patch that spans files holds one group per file it names.
func (r *Runner) executeToolCalls(ctx context.Context, sess store.Session, executor *tools.Executor, calls []wire.AssembledToolCall) []tools.Outcome {
	outcomes := make([]tools.Outcome, len(calls))
	var wg sync.WaitGroup
	// Calls that rewrite a file another call in this batch also rewrites, in
	// the order the model asked for them. Grouping is by resolved path, so
	// the group is the set that would otherwise race. A call that names
	// several files, which apply_patch can, joins every group its files
	// belong to, and two groups it bridges become one, so it can never run
	// beside a call that touches one of its files.
	var runs []*mutationRun
	runOf := make(map[string]*mutationRun)
	for i, c := range calls {
		if isTaskFamily(c.Name) {
			outcomes[i] = r.executeToolCall(ctx, sess, executor, c)
			continue
		}
		targets := tools.MutationTargets(executor.Workspace, c.Name, c.Arguments)
		if len(targets) == 0 {
			wg.Add(1)
			go func(i int, c wire.AssembledToolCall) {
				defer wg.Done()
				outcomes[i] = r.executeToolCall(ctx, sess, executor, c)
			}(i, c)
			continue
		}
		var run *mutationRun
		for _, target := range targets {
			other, ok := runOf[target]
			if !ok {
				continue
			}
			switch {
			case run == nil:
				run = other
			case run != other:
				run.absorb(other, runOf)
			}
		}
		if run == nil {
			run = &mutationRun{}
			runs = append(runs, run)
		}
		for _, target := range targets {
			runOf[target] = run
		}
		run.calls = append(run.calls, i)
	}
	// One goroutine per group, so different files still run concurrently and
	// only a real collision pays for the ordering.
	for _, run := range runs {
		wg.Add(1)
		go func(idx []int) {
			defer wg.Done()
			applied := 0
			for _, i := range idx {
				outcomes[i] = r.executeToolCall(ctx, sess, executor, calls[i])
				if !outcomes[i].Result.IsError {
					applied++
					continue
				}
				// The edits disagreed: an earlier one in this batch
				// rewrote the region this one was written against, so
				// its old_string is no longer there. Say so. Left
				// alone the model reads "old_string not found" and
				// goes hunting for a typo in text that was correct
				// when it wrote it.
				if applied > 0 {
					outcomes[i].Result.Content +=
						"\n\nNote: an earlier call in this same message already rewrote this file, so it no " +
							"longer matches what this call was written against. Read it again and redo this " +
							"change against what is there now. Two writes to one file in one message are " +
							"applied in order, not merged — send the second one after seeing the first land."
				}
			}
		}(run.calls)
	}
	wg.Wait()
	return outcomes
}

// mutationRun is the set of calls that have to run in order because they
// touch a common file. calls holds the indices into the batch and is sorted
// before the group runs, so merging two groups cannot reorder the model's
// calls.
type mutationRun struct {
	calls []int
}

// absorb folds other into g and repoints every path that led to other, so a
// later call that names one of those paths joins the merged group.
func (g *mutationRun) absorb(other *mutationRun, runOf map[string]*mutationRun) {
	g.calls = append(g.calls, other.calls...)
	sort.Ints(g.calls)
	other.calls = nil
	for target, run := range runOf {
		if run == other {
			runOf[target] = g
		}
	}
}

// executeToolCall runs one assembled call. A bash call gets a live stdout sink
// wired through its context so the browser can show output as it happens
// instead of only on completion (docs/DESIGN.md §5.2).
func (r *Runner) executeToolCall(ctx context.Context, sess store.Session, executor *tools.Executor, c wire.AssembledToolCall) tools.Outcome {
	call := wire.ToolCall{
		ID:   c.ID,
		Type: "function",
		Function: wire.ToolCallFunc{
			Name:      c.Name,
			Arguments: c.Arguments,
		},
	}
	callCtx := httplog.WithSessionID(ctx, sess.ID)
	// `bash` streams its command's output; an MCP tool streams whatever
	// progress its server reports while the call is still running, which
	// is the only thing standing between a three-minute render and a
	// transcript that looks like a hang (docs/MCP.md, "What a server sends
	// back unasked").
	if r.Hub != nil && (c.Name == "bash" || strings.HasPrefix(c.Name, tools.MCPToolPrefix)) {
		callCtx = tools.WithStdoutSink(callCtx, r.stdoutSink(ctx, sess, c.ID))
	}
	if executor.Guard != nil && c.Name == "bash" {
		callCtx = withGuardScope(callCtx, sess.ID)
	}
	return executor.Execute(callCtx, call)
}

// isTaskFamily reports whether name is one of the four task tools that
// execute synchronously in calls order rather than in a goroutine, so the
// ids task_create mints never diverge from what the event-log replay
// reconstructs (see executeToolCalls).
func isTaskFamily(name string) bool {
	switch name {
	case "task_create", "task_get", "task_list", "task_update":
		return true
	}
	return false
}

// stdoutSink returns the callback a running `bash` call uses to publish
// incremental output. Each chunk commits as its own tool_stdout event,
// mirrored and fanned out to SSE subscribers exactly like any other event
// batch (docs/DESIGN.md §4.1); liveStdoutWriter is what keeps the volume of
// chunks down, not this function. It is never added to the fold's local
// event slice — internal/fold already treats tool_stdout as carrying no
// messages-array content, so there is nothing for the next request to gain
// from holding onto it after it is published.
func (r *Runner) stdoutSink(ctx context.Context, sess store.Session, toolCallID string) func(string) {
	return func(chunk string) {
		appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
			{Kind: store.KindToolStdout, Payload: store.ToolStdoutPayload{ToolCallID: toolCallID, Text: chunk}},
		})
		if err != nil {
			log.Printf("session: append tool_stdout for %s: %v", sess.ID, err)
			return
		}
		r.mirrorAppend(sess, appended)
		r.publishEvents(sess, appended)
	}
}
