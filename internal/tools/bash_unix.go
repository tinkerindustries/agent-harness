//go:build unix

package tools

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// bashGroup runs the child in its own process group and returns a killer that
// signals the whole group rather than the direct /bin/sh child. Killing only
// sh is what lets a grandchild that inherited the output pipe survive the
// call today: the pipe stays open after sh exits, cmd.Run() blocks on the copy
// goroutines forever, and only ending the pipe-holder unblocks it
// (docs/RUN-CONTROL.md, "Half one"). Put behind //go:build unix so the package
// still builds and vets on platforms without Setpgid (bash_other.go).
func bashGroup(cmd *exec.Cmd) *groupKiller {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	k := &groupKiller{cmd: cmd}
	cmd.Cancel = k.signal
	return k
}

// groupKiller signals the child's process group: SIGTERM on the first call,
// SIGKILL on any later one. The group id is the child's own pid, because
// Setpgid made the child the leader of a fresh group that every descendant
// inherits.
type groupKiller struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	term bool // a SIGTERM has been sent; escalate to SIGKILL
}

// signal is the cmd.Cancel implementation: os/exec calls it when the command's
// context is done. The first call asks the group to stop; any later call (from
// forceKill, or a second cancellation) takes it down.
func (k *groupKiller) signal() error {
	k.mu.Lock()
	sig := syscall.SIGTERM
	if k.term {
		sig = syscall.SIGKILL
	}
	k.term = true
	pid := k.pgid()
	k.mu.Unlock()

	if pid == 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		// The group is already gone. Report it the way os/exec's own default
		// Cancel reports a dead process, so watchCtx does not dress a
		// successful shutdown up as a cancel error.
		return os.ErrProcessDone
	}
	return err
}

// forceKill guarantees the group is dead. The command's own process has
// exited; what is left in the group is a grandchild holding the output pipe,
// and no context cancellation is going to reach it. SIGTERM first (a trap can
// still clean up state), then SIGKILL as the backstop.
func (k *groupKiller) forceKill() error {
	k.mu.Lock()
	pid := k.pgid()
	needTerm := !k.term
	k.term = true
	k.mu.Unlock()

	if pid == 0 {
		return os.ErrProcessDone
	}
	if needTerm {
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil // the goal — the group is gone — is already met
	}
	return err
}

// pgid returns the child's process group id. Setpgid makes the child the
// leader of its own group, so its pid is the pgid. Zero when the command has
// not started (Cancel is never called before Start; the guard is for safety).
func (k *groupKiller) pgid() int {
	if k.cmd == nil || k.cmd.Process == nil {
		return 0
	}
	return k.cmd.Process.Pid
}
