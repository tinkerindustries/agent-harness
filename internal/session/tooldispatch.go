package session

import (
	"context"
	"log"
	"sync"

	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// tooldispatch.go: executing one sub-turn's tool calls in tool_calls array
// order (the append-only invariant tool results depend on), and the stdout
// sink a long-running tool streams through.

// executeToolCalls runs every call concurrently and returns outcomes in the
// same order as calls, regardless of completion order — the appender is
// what must preserve tool_calls order, not the execution itself
// (docs/TOOLS.md). The four Task tools are the exception and run
// synchronously, in calls order, interleaved with the goroutine spawning:
// TaskCreate mints ids from a counter in execution order, and the store's
// plan replay (internal/store/status.go) and the frontend both reconstruct
// those ids by walking the event log in the same calls order, so letting
// two TaskCreate calls race for the executor's lock would hand the model
// ids nobody else can reconstruct.
//
// Calls that rewrite the same file are the other exception: they run in one
// goroutine, in calls order, so the file is never the prize in a race.
// Edit and Write both read a whole file, apply a change to their own copy,
// and write it back, so two of them racing on one path silently drop
// whichever finishes first — the model is told both succeeded and one of its
// changes is simply gone. Ordering them makes disjoint edits both land, and
// turns the genuinely conflicting case into Edit's own "old_string not found"
// with a note saying why (tools.MutationTarget picks the group out). Files
// are grouped separately, so different files still run concurrently.
func (r *Runner) executeToolCalls(ctx context.Context, sess store.Session, executor *tools.Executor, calls []wire.AssembledToolCall) []tools.Outcome {
	outcomes := make([]tools.Outcome, len(calls))
	var wg sync.WaitGroup
	// Calls that rewrite the same file, in the order the model asked for
	// them. Grouping is by resolved path, so the group is the set that
	// would otherwise race.
	sameFile := make(map[string][]int)
	for i, c := range calls {
		if isTaskFamily(c.Name) {
			outcomes[i] = r.executeToolCall(ctx, sess, executor, c)
			continue
		}
		if path, ok := tools.MutationTarget(executor.Workspace, c.Name, c.Arguments); ok {
			sameFile[path] = append(sameFile[path], i)
			continue
		}
		wg.Add(1)
		go func(i int, c wire.AssembledToolCall) {
			defer wg.Done()
			outcomes[i] = r.executeToolCall(ctx, sess, executor, c)
		}(i, c)
	}
	// One goroutine per file, so different files still run concurrently and
	// only a real collision pays for the ordering.
	for _, idx := range sameFile {
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
		}(idx)
	}
	wg.Wait()
	return outcomes
}

// executeToolCall runs one assembled call. A Bash call gets a live stdout sink
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
	if r.Hub != nil && c.Name == "Bash" {
		callCtx = tools.WithStdoutSink(callCtx, r.stdoutSink(ctx, sess, c.ID))
	}
	return executor.Execute(callCtx, call)
}

// isTaskFamily reports whether name is one of the four Task tools that
// execute synchronously in calls order rather than in a goroutine, so the
// ids TaskCreate mints never diverge from what the event-log replay
// reconstructs (see executeToolCalls).
func isTaskFamily(name string) bool {
	switch name {
	case "TaskCreate", "TaskGet", "TaskList", "TaskUpdate":
		return true
	}
	return false
}

// stdoutSink returns the callback a running Bash call uses to publish
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

func toolCallNames(calls []wire.AssembledToolCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Name
	}
	return out
}
