//go:build !unix && !windows

package tools

import "os/exec"

// bashGroup is a no-op on platforms without process groups: the child runs in
// the default configuration and cmd.Cancel stays exec.CommandContext's default
// (kill the direct child). WaitDelay still bounds the wait on every platform —
// the call cannot hang — but the group cleanup is unavailable, so an orphan
// that survives the direct child also survives the call. Unix signals the
// process group (bash_unix.go) and Windows terminates the process tree
// (bash_windows.go); this file exists so the package still compiles and vets
// anywhere else.
func bashGroup(_ *exec.Cmd) *groupKiller { return &groupKiller{} }

// groupKiller is the no-op form of the unix killer.
type groupKiller struct{}

func (*groupKiller) signal() error    { return nil }
func (*groupKiller) forceKill() error { return nil }
func (*groupKiller) started()         {}
func (*groupKiller) release()         {}
