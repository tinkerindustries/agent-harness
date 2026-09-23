package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// backgroundShell tracks one Bash call started with run_in_background: true,
// so a later BashOutput can read what it has produced since the last poll
// and KillBash can end it. It is Executor state, not the tool call's — the
// call that started it has already returned by the time anything reads
// this.
type backgroundShell struct {
	command string
	started time.Time
	killer  *groupKiller
	buf     *capturedBuffer

	mu         sync.Mutex
	readOffset int
	done       bool
	killed     bool
	exitCode   int
	wedged     bool // exec.ErrWaitDelay: exited, but left a process holding its output open
}

// execBashBackground implements Bash's run_in_background: true path. It
// starts command exactly as the foreground path does — same shell, same
// working directory, same environment resolution, same process group and
// WaitDelay for the orphaned-pipe case docs/RUN-CONTROL.md names — but does
// not wait for it: it mints an id, records the shell on the Executor, and
// returns immediately. The command is deliberately not tied to ctx, which
// Execute cancels the moment this function returns (registry.go, "Execute");
// a background shell's whole purpose is to outlive the call that started it,
// so cmd.Wait runs in its own goroutine against the process alone, and only
// KillBash or Executor.Close ever end it early.
func execBashBackground(ctx context.Context, e *Executor, command string) Result {
	// context.Background(), not ctx: os/exec refuses a non-nil cmd.Cancel
	// (bashGroup below sets one) on a Cmd not created through
	// CommandContext, and Background() is never Done, so cmd.Cancel is
	// simply never invoked by it — the command's own lifetime comes only
	// from KillBash or Executor.Close calling the group killer directly.
	cmd := exec.CommandContext(context.Background(), shellPath(), "-c", command)
	cmd.Dir = e.Workspace

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

	cmd.WaitDelay = e.bashWaitDelay(ctx)
	killer := bashGroup(cmd)

	buf := &capturedBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Start(); err != nil {
		return errorResult("start background command: %v", err)
	}
	killer.started()

	bg := &backgroundShell{command: command, started: time.Now(), killer: killer, buf: buf}

	e.shellsMu.Lock()
	e.nextShellID++
	id := fmt.Sprintf("bash_%d", e.nextShellID)
	if e.shells == nil {
		e.shells = make(map[string]*backgroundShell)
	}
	e.shells[id] = bg
	e.shellsMu.Unlock()

	go func() {
		runErr := cmd.Wait()
		if errors.Is(runErr, exec.ErrWaitDelay) {
			killer.forceKill()
		}
		killer.release()
		bg.mu.Lock()
		defer bg.mu.Unlock()
		bg.done = true
		bg.wedged = errors.Is(runErr, exec.ErrWaitDelay)
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			bg.exitCode = exitErr.ExitCode()
		}
	}()

	return Result{Content: fmt.Sprintf(
		"Running in the background with id %s. Use BashOutput with this id to read its output, and KillBash to stop it.", id)}
}

type bashOutputArgs struct {
	BashID string `json:"bash_id"`
	Filter string `json:"filter"`
}

// execBashOutput implements BashOutput: the output a background shell has
// produced since the last time it was read, plus whether it is still
// running. Consuming the output on read — rather than replaying it whole
// every call — is what keeps a long-running dev server's log from being
// re-sent, and re-billed, on every poll.
func execBashOutput(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args bashOutputArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.BashID == "" {
		return errorResult("bash_id is required")
	}

	e.shellsMu.Lock()
	bg, ok := e.shells[args.BashID]
	e.shellsMu.Unlock()
	if !ok {
		return errorResult("no background shell with id %q", args.BashID)
	}

	var filterRe *regexp.Regexp
	if args.Filter != "" {
		re, err := regexp.Compile(args.Filter)
		if err != nil {
			return errorResult("invalid filter: %v", err)
		}
		filterRe = re
	}

	// buf.String() must be taken under bg.mu, alongside readOffset:
	// otherwise two concurrent BashOutput calls on the same shell could each
	// snapshot the buffer at a different length while sharing one
	// readOffset, and whichever locks second could slice its own, shorter
	// snapshot from an offset the first call already advanced past it.
	bg.mu.Lock()
	full := bg.buf.String()
	newOutput := full[bg.readOffset:]
	bg.readOffset = len(full)
	done, killed, wedged, exitCode := bg.done, bg.killed, bg.wedged, bg.exitCode
	bg.mu.Unlock()

	if filterRe != nil {
		lines := strings.Split(newOutput, "\n")
		kept := lines[:0]
		for _, line := range lines {
			if filterRe.MatchString(line) {
				kept = append(kept, line)
			}
		}
		newOutput = strings.Join(kept, "\n")
	}

	status := "running"
	switch {
	case killed:
		status = "killed"
	case wedged:
		status = "exited, but left a process holding its output open and was killed"
	case done:
		status = fmt.Sprintf("completed (exit code %d)", exitCode)
	}

	text := newOutput
	if text == "" {
		text = "(no new output)"
	}
	out, truncated := truncate(text, e.outputCap(ctx))
	return Result{Content: fmt.Sprintf("Status: %s\n\n%s", status, out), Truncated: truncated}
}

type killBashArgs struct {
	ShellID string `json:"shell_id"`
}

// execKillBash implements KillBash: end a background shell before it exits
// on its own. Killing an already-finished shell is not an error — reported
// as a no-op — because the model cannot always tell from BashOutput's status
// alone whether a kill would still land before the process exits by itself.
func execKillBash(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args killBashArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.ShellID == "" {
		return errorResult("shell_id is required")
	}

	e.shellsMu.Lock()
	bg, ok := e.shells[args.ShellID]
	e.shellsMu.Unlock()
	if !ok {
		return errorResult("no background shell with id %q", args.ShellID)
	}

	bg.mu.Lock()
	if bg.done {
		bg.mu.Unlock()
		return Result{Content: fmt.Sprintf("%s had already finished; nothing to kill.", args.ShellID)}
	}
	bg.mu.Unlock()

	if err := bg.killer.forceKill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errorResult("kill %s: %v", args.ShellID, err)
	}

	bg.mu.Lock()
	bg.killed = true
	bg.mu.Unlock()

	return Result{Content: fmt.Sprintf("%s killed.", args.ShellID)}
}
