//go:build !unix

package tools

import "os/exec"

// bashGroup is a no-op on platforms without process groups: the child runs in
// the default configuration and cmd.Cancel stays exec.CommandContext's default
// (kill the direct child). WaitDelay still bounds the wait on every platform —
// the call cannot hang — but the group cleanup is unavailable, so an orphan
// that survives the direct child also survives the call. The harness runs
// Linux in the container and macOS on the desk, and both are unix; this file
// exists so the package still compiles and vets where Setpgid does not.
func bashGroup(_ *exec.Cmd) *groupKiller { return &groupKiller{} }

// groupKiller is the no-op form of the unix killer.
type groupKiller struct{}

func (*groupKiller) signal() error    { return nil }
func (*groupKiller) forceKill() error { return nil }
