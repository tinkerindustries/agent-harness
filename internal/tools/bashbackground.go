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
// and KillBash can end it. It is session state, not the tool call's: the
// call that started it has already returned by the time anything reads
// this.
type backgroundShell struct {
	started time.Time
	killer  *groupKiller
	buf     *capturedBuffer

	mu         sync.Mutex
	readOffset int
	done       bool
	killed     bool
	// quiet is set by a registry Close: the shell is being ended because the
	// run or the process is going away, so its exit is not a completion the
	// model should be told about.
	quiet    bool
	exitCode int
	wedged   bool // exec.ErrWaitDelay: exited, but left a process holding its output open
}

// ShellExit is one background shell's ending. The registry reports it once,
// after cmd.Wait returns, so the session can record the completion and tell
// the model.
type ShellExit struct {
	ID       string
	Command  string
	ExitCode int
	Killed   bool
	Wedged   bool
}

// Shells is the set of background shells one session has running, keyed by
// the id Bash mints. It belongs to the session rather than to an Executor,
// because a shell a Bash call started outlives the run that started it: the
// next run's BashOutput has to find the same id. The process closes it when
// the session ends, and a stopped run closes it explicitly; Close on an
// Executor that was handed one of these leaves it alone.
//
// It also carries the session id the completion is recorded against. That is
// not constant: compaction retires the row a run started under and continues
// it under a new one, and a shell started after that belongs to the new row.
// SetSessionID is how the loop moves the registry with it.
type Shells struct {
	mu      sync.Mutex
	byID    map[string]*backgroundShell
	nextID  int
	session string
	onExit  func(sessionID string, exit ShellExit)
}

// NewShells returns an empty registry for sessionID. onExit, when non-nil,
// is called once per shell after it exits, outside the registry's lock, with
// the session id current at that moment.
func NewShells(sessionID string, onExit func(string, ShellExit)) *Shells {
	return &Shells{
		byID:    make(map[string]*backgroundShell),
		session: sessionID,
		onExit:  onExit,
	}
}

// SessionID is the session a completion would be recorded against now.
func (s *Shells) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// SetSessionID retargets the registry at another row. Compaction is the only
// caller: the successor session is the one still running, so a shell that
// exits later must be recorded there, and a shell started later must be
// addressable by its runs.
func (s *Shells) SetSessionID(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sessionID
}

func (s *Shells) add(bg *backgroundShell) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("bash_%d", s.nextID)
	s.byID[id] = bg
	return id
}

func (s *Shells) get(id string) (*backgroundShell, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bg, ok := s.byID[id]
	return bg, ok
}

func (s *Shells) finished(exit ShellExit) {
	s.mu.Lock()
	cb, session := s.onExit, s.session
	s.mu.Unlock()
	if cb != nil {
		cb(session, exit)
	}
}

// Close ends every shell in the registry that is still running. It is the
// stop and process-exit path: an ordinary run ending leaves its shells here
// for the next run to read.
func (s *Shells) Close() {
	s.mu.Lock()
	shells := make([]*backgroundShell, 0, len(s.byID))
	for _, bg := range s.byID {
		shells = append(shells, bg)
	}
	s.mu.Unlock()

	for _, bg := range shells {
		bg.mu.Lock()
		done := bg.done
		if !done {
			// Marked before the kill for the same reason KillBash marks it:
			// the wait goroutine reads this when it builds the ShellExit, and
			// a stop's notice should say killed, not "exit code -1". quiet
			// keeps that same exit out of the completion callback: the run is
			// ending, so there is nobody to tell.
			bg.killed = true
			bg.quiet = true
		}
		bg.mu.Unlock()
		if !done {
			bg.killer.forceKill()
		}
	}
}

// execBashBackground implements Bash's run_in_background: true path. It
// starts command exactly as the foreground path does — same shell, same
// working directory, same environment resolution, same process group and
// WaitDelay for the orphaned-pipe case docs/RUN-CONTROL.md names — but does
// not wait for it: it mints an id, records the shell in the session's
// registry, and returns immediately. The command is deliberately not tied to
// ctx, which Execute cancels the moment this function returns (registry.go,
// "Execute"); a background shell's whole purpose is to outlive the call that
// started it, so cmd.Wait runs in its own goroutine against the process
// alone, and only KillBash, a stopped run, or the process exiting ever end it
// early.
func execBashBackground(ctx context.Context, e *Executor, command string) Result {
	// context.Background(), not ctx: os/exec refuses a non-nil cmd.Cancel
	// (bashGroup below sets one) on a Cmd not created through
	// CommandContext, and Background() is never Done, so cmd.Cancel is
	// simply never invoked by it — the command's own lifetime comes only
	// from KillBash or the registry's Close calling the group killer directly.
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

	bg := &backgroundShell{started: time.Now(), killer: killer, buf: buf}
	registry := e.shellsRegistry()
	id := registry.add(bg)

	go func() {
		runErr := cmd.Wait()
		wedged := errors.Is(runErr, exec.ErrWaitDelay)
		if wedged {
			killer.forceKill()
		}
		killer.release()
		var exitCode int
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		bg.mu.Lock()
		bg.done = true
		bg.wedged = wedged
		bg.exitCode = exitCode
		killed, quiet := bg.killed, bg.quiet
		bg.mu.Unlock()
		if quiet {
			return
		}
		registry.finished(ShellExit{ID: id, Command: command, ExitCode: exitCode, Killed: killed, Wedged: wedged})
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

	bg, ok := e.shellsRegistry().get(args.BashID)
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

	bg, ok := e.shellsRegistry().get(args.ShellID)
	if !ok {
		return errorResult("no background shell with id %q", args.ShellID)
	}

	bg.mu.Lock()
	if bg.done {
		bg.mu.Unlock()
		return Result{Content: fmt.Sprintf("%s had already finished; nothing to kill.", args.ShellID)}
	}
	// Set before the kill, so the wait goroutine's ShellExit reports killed
	// rather than a bare signal exit whichever way the race goes.
	bg.killed = true
	bg.mu.Unlock()

	if err := bg.killer.forceKill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errorResult("kill %s: %v", args.ShellID, err)
	}

	return Result{Content: fmt.Sprintf("%s killed.", args.ShellID)}
}
