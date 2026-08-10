package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type bashArgs struct {
	Command     string `json:"command"`
	TimeoutMS   int    `json:"timeout"`
	Description string `json:"description"`
}

// execBash implements Bash: foreground only, wall-clock timeout from ctx
// (set in Executor.timeoutFor), output capped and labelled on truncation
// (docs/TOOLS.md).
func execBash(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args bashArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Command == "" {
		return errorResult("command is required")
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", args.Command)
	cmd.Dir = e.Workspace

	// A command that backgrounds a process without redirecting its output
	// (node server.js &, inheriting the captured pipe) leaves that pipe open
	// after /bin/sh exits, and cmd.Run() would block on the copy goroutines
	// forever — past the tool timeout, past a cancelled context. WaitDelay
	// bounds that wait, and the process group (bashGroup) is what lets the
	// kill reach the pipe-holder instead of only the direct sh child
	// (docs/TOOLS.md, "Bash").
	waitDelay := e.bashWaitDelay(ctx)
	cmd.WaitDelay = waitDelay
	group := bashGroup(cmd)

	var out capturedBuffer
	live := &liveStdoutWriter{sink: stdoutSinkFromContext(ctx)}
	cmd.Stdout = io.MultiWriter(&out, live)
	cmd.Stderr = io.MultiWriter(&out, live)

	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The command exited but a grandchild kept its output pipe open; Wait
		// returned after WaitDelay and nothing has been cancelled — this path
		// has no cancellation behind it. Finish the group off, or the orphan
		// outlives the call, keeps whatever port it bound, and breaks the next
		// run.
		group.forceKill()
	}
	live.flush()

	text, truncated := truncate(out.String(), e.outputCap(ctx))
	result := Result{Content: text, Truncated: truncated}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Content = fmt.Sprintf("command timed out\n\npartial output:\n%s", text)
		result.IsError = true
		return result
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return errorResult(
			"the command exited but left a process holding its output open; the harness stopped waiting after %s and killed the process group. Redirect output and detach (cmd >/tmp/x.log 2>&1 &) if you meant to leave something running",
			waitDelay)
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.Content = fmt.Sprintf("%s\n\n[exit code %d]", text, exitErr.ExitCode())
		result.IsError = true
		return result
	}
	if runErr != nil {
		return errorResult("run command: %v", runErr)
	}
	if result.Content == "" {
		result.Content = "(no output)"
	}
	return result
}

// capturedBuffer is the buffer a Bash call captures its command's stdout and
// stderr into. cmd.Stdout and cmd.Stderr are distinct writer values, so the
// two copy goroutines can call Write concurrently; and once WaitDelay can
// make Wait return while a copy goroutine is still writing, the post-Wait read
// of the buffer would race it. The mutex makes both safe
// (docs/RUN-CONTROL.md, "Half one").
type capturedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *capturedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *capturedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// stdoutStreamInterval bounds how often liveStdoutWriter forwards buffered
// output to its sink, so a command writing many small chunks turns into a
// live update a couple of times a second rather than one store commit per OS
// pipe read.
const stdoutStreamInterval = 200 * time.Millisecond

// liveStdoutWriter mirrors everything written through it to sink, coalesced
// by time rather than forwarded write-for-write. A nil sink (no live
// subscriber, or a caller that never attached one) makes every write free.
type liveStdoutWriter struct {
	sink func(string)

	mu       sync.Mutex
	pending  strings.Builder
	lastSent time.Time
}

func (w *liveStdoutWriter) Write(p []byte) (int, error) {
	if w.sink == nil {
		return len(p), nil
	}
	w.mu.Lock()
	w.pending.Write(p)
	var chunk string
	if time.Since(w.lastSent) >= stdoutStreamInterval {
		chunk = w.pending.String()
		w.pending.Reset()
		w.lastSent = time.Now()
	}
	w.mu.Unlock()
	if chunk != "" {
		w.sink(chunk)
	}
	return len(p), nil
}

// flush forwards whatever is left buffered. Call it once after the command
// exits, so the tail of the output — anything written inside the last
// interval — still reaches a live subscriber instead of only appearing once
// the final tool_result lands.
func (w *liveStdoutWriter) flush() {
	if w.sink == nil {
		return
	}
	w.mu.Lock()
	chunk := w.pending.String()
	w.pending.Reset()
	w.mu.Unlock()
	if chunk != "" {
		w.sink(chunk)
	}
}
