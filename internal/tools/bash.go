package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type bashArgs struct {
	Command         string `json:"command"`
	TimeoutMS       int    `json:"timeout"`
	Description     string `json:"description"`
	RunInBackground bool   `json:"run_in_background"`
}

// shellPath is the shell every Bash call runs through: bash where it exists,
// the system sh otherwise. The tool is named Bash and models write bash —
// arrays, [[ ]], ${PIPESTATUS[0]} — which busybox ash rejects as a syntax
// error. Resolved once, since PATH does not change under a running process.
var shellPath = sync.OnceValue(func() string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return systemShell(runtime.GOOS)
})

// systemShell is where goos keeps its POSIX sh. Android has no /bin.
func systemShell(goos string) string {
	if goos == "android" {
		return "/system/bin/sh"
	}
	return "/bin/sh"
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
	if args.RunInBackground {
		return execBashBackground(ctx, e, args.Command)
	}

	cmd := exec.CommandContext(ctx, shellPath(), "-c", args.Command)
	cmd.Dir = e.Workspace

	// A nil cmd.Env inherits this process's environment, which is how the
	// ambient credentials the parent process set reach git and gh. Env is
	// only built explicitly when something has extra variables to add — a
	// GitHub App installation token for gh, minted for this session's own
	// repositories — or when EnvFilter must run before anything is
	// inherited at all; both branches start from the same os.Environ() the
	// nil case would have inherited, so nothing is lost by taking either.
	switch {
	case e.EnvFilter != nil:
		base := e.EnvFilter(os.Environ())
		if e.ExtraEnv != nil {
			base = append(base, e.ExtraEnv(ctx, e.Workspace)...)
		}
		cmd.Env = base
	case e.ExtraEnv != nil:
		if extra := e.ExtraEnv(ctx, e.Workspace); len(extra) > 0 {
			cmd.Env = append(os.Environ(), extra...)
		}
	}

	// A command that backgrounds a process without redirecting its output
	// (node server.js &, inheriting the captured pipe) leaves that pipe open
	// after the shell exits, and cmd.Run() would block on the copy goroutines
	// forever — past the tool timeout, past a cancelled context. WaitDelay
	// bounds that wait, and the process group (bashGroup) is what lets the
	// kill reach the pipe-holder instead of only the direct shell child
	// (docs/TOOLS.md, "Bash").
	waitDelay := e.bashWaitDelay(ctx)
	cmd.WaitDelay = waitDelay
	group := bashGroup(cmd)
	defer group.release()

	var out capturedBuffer
	live := &liveStdoutWriter{sink: stdoutSinkFromContext(ctx)}
	cmd.Stdout = io.MultiWriter(&out, live)
	cmd.Stderr = io.MultiWriter(&out, live)

	started := time.Now()
	runErr := runGrouped(cmd, group)
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
		result.Content = fmt.Sprintf("command timed out after %s\n\npartial output:\n%s",
			bashLimitNote(ctx, started, args.TimeoutMS), text)
		result.IsError = true
		return result
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// Everything the command wrote before it wedged is kept, the way the
		// timeout path above keeps it. A command that builds, prints its log,
		// and only then backgrounds something would otherwise come back as the
		// explanation alone, and the model would have lost the output it
		// actually asked for.
		result.Content = fmt.Sprintf(
			"the command exited but left a process holding its output open; the harness stopped waiting after %s and killed the process group. Redirect output and detach (cmd >/tmp/x.log 2>&1 &) if you meant to leave something running\n\npartial output:\n%s",
			waitDelay, text)
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

// runGrouped is cmd.Run with the group killer told the child has started, in
// the gap between Start and Wait that Run does not expose. On Windows that is
// where the child joins the job object every process it starts inherits
// (bash_windows.go); on unix Setpgid already did the equivalent at Start.
func runGrouped(cmd *exec.Cmd, group *groupKiller) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	group.started()
	return cmd.Wait()
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

// bashLimitNote names the wall-clock bound that actually expired, and says why
// it is not the one the caller asked for when those differ. docs/CACHE.md bars
// tools.bash_timeout and tools.bash_timeout_max from the tool description,
// because the description is part of the frozen request head and an operator
// can change either number; this message is where the rule says the model
// learns the real limit instead. A model told only "command timed out" reads a
// clamped 30-minute request as a hung command and retries it the same way.
func bashLimitNote(ctx context.Context, started time.Time, requestedMS int) string {
	limit, ok := toolLimitFrom(ctx)
	if !ok {
		// Reached other than through Executor.Execute, so nothing attached
		// the limit. The deadline still holds it, minus the setup between
		// WithTimeout and the command actually starting.
		deadline, hasDeadline := ctx.Deadline()
		if !hasDeadline {
			return "its timeout"
		}
		limit = deadline.Sub(started).Round(time.Millisecond)
	}
	requested := time.Duration(requestedMS) * time.Millisecond
	switch {
	case requestedMS <= 0:
		return fmt.Sprintf("%s, the default for a call that names no timeout", limit)
	case requested > limit:
		return fmt.Sprintf("%s; %s was requested and clamped down to the harness ceiling, so asking for longer will not help", limit, requested)
	default:
		return limit.String()
	}
}
