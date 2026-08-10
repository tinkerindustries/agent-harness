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

	var out bytes.Buffer
	live := &liveStdoutWriter{sink: stdoutSinkFromContext(ctx)}
	cmd.Stdout = io.MultiWriter(&out, live)
	cmd.Stderr = io.MultiWriter(&out, live)

	runErr := cmd.Run()
	live.flush()

	text, truncated := truncate(out.String(), e.outputCap(ctx))
	result := Result{Content: text, Truncated: truncated}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Content = fmt.Sprintf("command timed out\n\npartial output:\n%s", text)
		result.IsError = true
		return result
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
