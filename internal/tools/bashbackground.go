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

// backgroundShell tracks one bash call started with run_in_background: true,
// so a later job_output can read what it has produced since the last poll
// and job_kill can end it. It is session state, not the tool call's: the
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
	// quiet is set by a registry Close: the shell is being ended because the
	// run or the process is going away, so its exit is not a completion the
	// model should be told about.
	quiet    bool
	exitCode int
	wedged   bool // exec.ErrWaitDelay: exited, but left a process holding its output open

	// doneCh closes once, after cmd.Wait has set done and the exit fields
	// above, so a job_output with wait parks on it rather than polling.
	doneCh chan struct{}
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
// the id bash mints. It belongs to the session rather than to an Executor,
// because a shell a bash call started outlives the run that started it: the
// next run's job_output has to find the same id. The process closes it when
// the session ends, and a stopped run closes it explicitly; Close on an
// Executor that was handed one of these leaves it alone.
//
// It also carries the session id the completion is recorded against. That is
// not constant: compaction retires the row a run started under and continues
// it under a new one, and a shell started after that belongs to the new row.
// SetSessionID is how the loop moves the registry with it.
type Shells struct {
	mu     sync.Mutex
	byID   map[string]*backgroundShell
	order  []string
	nextID int
	// session and onExit are guarded by mu like the rest of the registry.
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
	s.order = append(s.order, id)
	return id
}

func (s *Shells) get(id string) (*backgroundShell, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bg, ok := s.byID[id]
	return bg, ok
}

// jobState is one job's row in job_list: the id bash minted, the command it
// runs, when it started and how it stands.
type jobState struct {
	ID       string
	Command  string
	Started  time.Time
	Done     bool
	Killed   bool
	Wedged   bool
	ExitCode int
}

// list returns every job the registry holds, in the order the jobs were
// started. A finished job stays in the registry, so a job that exited while
// the model was busy elsewhere is still there to be listed and read.
func (s *Shells) list() []jobState {
	type entry struct {
		id string
		bg *backgroundShell
	}
	s.mu.Lock()
	entries := make([]entry, 0, len(s.order))
	for _, id := range s.order {
		if bg, ok := s.byID[id]; ok {
			entries = append(entries, entry{id: id, bg: bg})
		}
	}
	s.mu.Unlock()

	out := make([]jobState, 0, len(entries))
	for _, e := range entries {
		e.bg.mu.Lock()
		out = append(out, jobState{
			ID:       e.id,
			Command:  e.bg.command,
			Started:  e.bg.started,
			Done:     e.bg.done,
			Killed:   e.bg.killed,
			Wedged:   e.bg.wedged,
			ExitCode: e.bg.exitCode,
		})
		e.bg.mu.Unlock()
	}
	return out
}

// wait blocks until bg exits or ctx is done, and reports whether it exited.
// A timed-out wait leaves the job running; the caller reports the output so
// far either way.
func (s *Shells) wait(ctx context.Context, bg *backgroundShell) bool {
	select {
	case <-bg.doneCh:
		return true
	case <-ctx.Done():
		return false
	}
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
			// Marked before the kill for the same reason job_kill marks it:
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

// execBashBackground implements bash's run_in_background: true path. It
// starts command exactly as the foreground path does — same shell, same
// working directory, same environment resolution, same process group and
// WaitDelay for the orphaned-pipe case docs/RUN-CONTROL.md names — but does
// not wait for it: it mints an id, records the shell in the session's
// registry, and returns immediately. The command is deliberately not tied to
// ctx, which Execute cancels the moment this function returns (registry.go,
// "Execute"); a background job's whole purpose is to outlive the call that
// started it, so cmd.Wait runs in its own goroutine against the process
// alone, and only job_kill, a stopped run, or the process exiting ever end it
// early.
func execBashBackground(ctx context.Context, e *Executor, command, dir string) Result {
	// context.Background(), not ctx: os/exec refuses a non-nil cmd.Cancel
	// (bashGroup below sets one) on a Cmd not created through
	// CommandContext, and Background() is never Done, so cmd.Cancel is
	// simply never invoked by it — the command's own lifetime comes only
	// from job_kill or the registry's Close calling the group killer directly.
	cmd := exec.CommandContext(context.Background(), shellPath(), "-c", command)
	cmd.Dir = dir

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

	bg := &backgroundShell{
		command: command,
		started: time.Now(),
		killer:  killer,
		buf:     buf,
		doneCh:  make(chan struct{}),
	}
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
		close(bg.doneCh)
		if quiet {
			return
		}
		registry.finished(ShellExit{ID: id, Command: command, ExitCode: exitCode, Killed: killed, Wedged: wedged})
	}()

	return Result{Content: fmt.Sprintf(
		"Running in the background with id %s. Use job_output with this id to read its output, job_list to see every job, and job_kill to stop it.", id)}
}

type jobOutputArgs struct {
	JobID  string `json:"job_id"`
	Filter string `json:"filter"`
	Wait   bool   `json:"wait"`
}

// execJobOutput implements job_output: the output a background job has
// produced since the last time it was read, plus whether it is still
// running. Consuming the output on read — rather than replaying it whole
// every call — is what keeps a long-running dev server's log from being
// re-sent, and re-billed, on every poll.
//
// wait blocks until the job finishes or this call's own timeout expires. A
// wait that runs out returns what the job has written so far and leaves the
// job running, so a model that asked for a build to finish can read where it
// got to without losing the job.
func execJobOutput(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args jobOutputArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.JobID == "" {
		return errorResult("job_id is required")
	}

	registry := e.shellsRegistry()
	bg, ok := registry.get(args.JobID)
	if !ok {
		return errorResult("no background job with id %q", args.JobID)
	}

	var filterRe *regexp.Regexp
	if args.Filter != "" {
		re, err := regexp.Compile(args.Filter)
		if err != nil {
			return errorResult("invalid filter: %v", err)
		}
		filterRe = re
	}

	waitedOut := false
	if args.Wait {
		waitedOut = !registry.wait(ctx, bg)
	}

	// buf.String() must be taken under bg.mu, alongside readOffset:
	// otherwise two concurrent job_output calls on the same job could each
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
	if waitedOut {
		status += " (the wait ended on this call's own timeout; the job is still running)"
	}

	text := newOutput
	if text == "" {
		text = "(no new output)"
	}
	out, truncated := truncate(text, e.outputCap(ctx))
	return Result{Content: fmt.Sprintf("Status: %s\n\n%s", status, out), Truncated: truncated}
}

type jobKillArgs struct {
	JobID string `json:"job_id"`
}

// execJobKill implements job_kill: end a background job before it exits
// on its own. Killing an already-finished job is not an error — reported
// as a no-op — because the model cannot always tell from job_output's status
// alone whether a kill would still land before the process exits by itself.
func execJobKill(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args jobKillArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.JobID == "" {
		return errorResult("job_id is required")
	}

	bg, ok := e.shellsRegistry().get(args.JobID)
	if !ok {
		return errorResult("no background job with id %q", args.JobID)
	}

	bg.mu.Lock()
	if bg.done {
		bg.mu.Unlock()
		return Result{Content: fmt.Sprintf("%s had already finished; nothing to kill.", args.JobID)}
	}
	// Set before the kill, so the wait goroutine's ShellExit reports killed
	// rather than a bare signal exit whichever way the race goes.
	bg.killed = true
	bg.mu.Unlock()

	if err := bg.killer.forceKill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errorResult("kill %s: %v", args.JobID, err)
	}

	return Result{Content: fmt.Sprintf("%s killed.", args.JobID)}
}

// execJobList implements job_list: every background job the session still
// holds, with the command it runs, when it started and whether it is running
// or has exited. Finished jobs stay in the registry, so a job that ended
// while the model was busy elsewhere shows up here rather than vanishing.
func execJobList(ctx context.Context, e *Executor, _ json.RawMessage) Result {
	jobs := e.shellsRegistry().list()
	if len(jobs) == 0 {
		return Result{Content: "No background jobs."}
	}
	var b strings.Builder
	for _, j := range jobs {
		state := "running"
		switch {
		case j.Killed:
			state = "killed"
		case j.Wedged:
			state = "exited, but left a process holding its output open and was killed"
		case j.Done:
			state = fmt.Sprintf("exited (exit code %d)", j.ExitCode)
		}
		fmt.Fprintf(&b, "- %s: %s, started %s, command: %s\n",
			j.ID, state, j.Started.UTC().Format(time.RFC3339), j.Command)
	}
	out, truncated := truncate(b.String(), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}
