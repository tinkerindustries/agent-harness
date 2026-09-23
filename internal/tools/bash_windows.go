//go:build windows

package tools

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bashGroup is the Windows form of the unix process-group killer. Windows has
// no process groups to signal; the nearest thing is a job object, which the
// child joins the moment it has started (started) and every process it
// creates joins with it, whatever happens to the processes in between. That
// matters here more than on unix: Git Bash runs `sleep 300 &` in a forked
// bash that exits and leaves sleep.exe parented to a pid that no longer
// exists, so no walk down from the shell can reach it. The killer terminates
// the job, and also walks the child's tree by parent pid, which reaches
// anything the child started in the moment before it joined.
//
// Killing only the direct child is what the unix side exists to avoid, and on
// Windows it is worse: Git Bash runs even a lone `sleep 30` as a second
// process under bash.exe, so the kill that reaches the shell alone leaves the
// command running and its output pipe open.
func bashGroup(cmd *exec.Cmd) *groupKiller {
	// notBefore is taken before Start, so the child and everything it spawns
	// was created after it. A process in the tree created earlier is one
	// that inherited a reused pid, not one of ours.
	k := &groupKiller{cmd: cmd, notBefore: time.Now().Add(-time.Second)}
	cmd.Cancel = k.signal
	return k
}

// groupKiller terminates the child's job and process tree. There is no
// SIGTERM to send first, so both methods end them outright.
type groupKiller struct {
	cmd       *exec.Cmd
	notBefore time.Time

	mu  sync.Mutex
	job windows.Handle // zero before started, after release, or if the child could not join one
}

// started puts the child in a fresh job object. It is called once, right
// after cmd.Start, before the shell has had time to start anything of its
// own. A child that cannot join one is still reached by the tree walk.
//
// The job is not created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: a command
// that detached a process on purpose, output redirected, is allowed to leave
// it running once the call returns, as it is on unix.
func (k *groupKiller) started() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(k.cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return
	}
	k.mu.Lock()
	k.job = job
	k.mu.Unlock()
}

// release closes the job handle once nothing will kill through it again.
// Closing it ends nothing: the job has no kill-on-close limit.
func (k *groupKiller) release() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.job != 0 {
		windows.CloseHandle(k.job)
		k.job = 0
	}
}

// signal is the cmd.Cancel implementation. It reports os.ErrProcessDone when
// nothing was left to kill, the way os/exec's own default Cancel reports a
// dead process. The child's liveness is read first, because terminating the
// job leaves the tree walk nothing to count.
func (k *groupKiller) signal() error {
	live := false
	if k.cmd != nil && k.cmd.Process != nil {
		if h, _, ok := openForKill(uint32(k.cmd.Process.Pid)); ok {
			windows.CloseHandle(h)
			live = true
		}
	}
	killed, err := k.killTree()
	if err == nil && killed == 0 && !live {
		return os.ErrProcessDone
	}
	return err
}

// forceKill guarantees the tree is dead. It is called after the child has
// exited as well as before, so the tree is found by parent pid from a fresh
// snapshot rather than from a handle the child has to still be alive for.
func (k *groupKiller) forceKill() error {
	_, err := k.killTree()
	return err
}

// killTree terminates the child's job, then every live process in the
// child's tree, parents first so a shell cannot start another command in the
// gap. A process started between the snapshot and the kill is missed by that
// round, so it repeats until a round finds nothing left, a bounded number of
// times. killed counts what the tree walk found alive.
func (k *groupKiller) killTree() (killed int, err error) {
	if k.cmd == nil || k.cmd.Process == nil {
		return 0, nil
	}
	k.mu.Lock()
	if k.job != 0 {
		windows.TerminateJobObject(k.job, 1)
	}
	k.mu.Unlock()

	root := uint32(k.cmd.Process.Pid)
	for range 5 {
		procs, err := k.tree(root)
		if err != nil {
			return killed, err
		}
		if len(procs) == 0 {
			return killed, nil
		}
		for _, h := range procs {
			if windows.TerminateProcess(h, 1) == nil {
				killed++
			}
			windows.CloseHandle(h)
		}
	}
	return killed, nil
}

// tree opens every live process descended from root, root included, in
// breadth-first order. A process that has exited is not opened but is still
// followed, since the shell exiting is exactly the case where a grandchild
// holding its output pipe is left. A live process is opened only if it was
// created after its nearest live ancestor and after notBefore: a pid Windows
// has since handed to an unrelated process fails that test, and nothing under
// it is touched. The caller closes the handles.
func (k *groupKiller) tree(root uint32) ([]windows.Handle, error) {
	children, err := childrenByParent()
	if err != nil {
		return nil, err
	}

	type node struct {
		pid     uint32
		created time.Time // the nearest live ancestor's creation time
	}
	var out []windows.Handle
	queue := []node{{pid: root, created: k.notBefore}}
	seen := map[uint32]bool{root: true}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]

		h, created, ok := openForKill(n.pid)
		if ok {
			if created.Before(n.created) {
				windows.CloseHandle(h)
				continue
			}
			out = append(out, h)
			n.created = created
		}
		for _, pid := range children[n.pid] {
			if !seen[pid] {
				seen[pid] = true
				queue = append(queue, node{pid: pid, created: n.created})
			}
		}
	}
	return out, nil
}

// childrenByParent is one snapshot of the process table, as each pid's
// children. A parent that has exited still names its children here, which is
// what lets a grandchild holding the output pipe be found after the shell is
// gone.
func childrenByParent() (map[uint32][]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	children := map[uint32][]uint32{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID != pe.ParentProcessID {
			children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return children, nil
}

// openForKill opens pid for termination and reports when it was created. A
// process that has already exited, or that this one may not touch, is not
// ok, and is skipped rather than failing the kill.
func openForKill(pid uint32) (windows.Handle, time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return 0, time.Time{}, false
	}
	// A handle to a process that has exited still opens; it is not live.
	if ev, err := windows.WaitForSingleObject(h, 0); err != nil || ev == windows.WAIT_OBJECT_0 {
		windows.CloseHandle(h)
		return 0, time.Time{}, false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		windows.CloseHandle(h)
		return 0, time.Time{}, false
	}
	return h, time.Unix(0, created.Nanoseconds()), true
}
