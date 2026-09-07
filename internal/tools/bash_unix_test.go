//go:build unix

// The process-group tests, split out for the same reason bash_unix.go is:
// group cleanup is what bash_unix.go provides and bash_other.go explicitly
// does not, so the test that proves an orphan dies with the call can only run
// where Setpgid and Kill exist. Keeping it here is what lets the package
// build and vet on Windows.

package tools

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBashKillsProcessGroup proves the orphan dies with the call. The child
// writes its pid to a pidfile in the workspace, and after the call returns
// syscall.Kill(pid, 0) must report the process gone. A pidfile rather than a
// port, so the test needs nothing bindable.
func TestBashKillsProcessGroup(t *testing.T) {
	e, root := newTestExecutor(t)
	e.Timeouts.BashWaitDelay = 200 * time.Millisecond

	pidFile := filepath.Join(root, "child.pid")
	res := execBash(t.Context(), e, mustJSON(t, bashArgs{
		Command: fmt.Sprintf("sleep 300 & echo $! > %s; echo started", pidFile),
	}))
	if !res.IsError {
		t.Fatalf("expected the wedged call to be reported as an error, got: %+v", res)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the child never wrote its pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("unreadable pidfile %q: %v", pidBytes, err)
	}

	// The group kill is SIGKILL, so death is immediate; poll because the
	// orphan is reparented when sh exits and a zombie lingers until its new
	// parent reaps it.
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("backgrounded process %d is still alive after the call", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// processAlive reports whether pid is still a running process. syscall.Kill
// with signal 0 is the probe: ESRCH means no such process. A zombie — a
// killed process whose parent has not yet reaped it — is dead for our
// purposes, but on Linux it still answers kill(pid, 0), and this container's
// PID 1 (the harness itself) never reaps, so the /proc state field is the
// tie-breaker. On platforms without /proc the kill probe alone is the whole
// answer: their init reaps orphans promptly.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no /proc entry, or non-Linux: the probe above stands
	}
	// /proc/<pid>/stat is "pid (comm) state ..."; comm may contain spaces and
	// parens, so the state follows the last ')'.
	commEnd := bytes.LastIndexByte(stat, ')')
	if commEnd >= 0 && commEnd+2 < len(stat) {
		return stat[commEnd+2] != 'Z'
	}
	return true
}
