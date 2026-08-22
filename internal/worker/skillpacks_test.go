package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// workspaceRunner records the workspace each run was given, which is the only
// thing these tests need out of the loop: what matters is what is on disk by
// the time the run starts, not what the run then does.
type workspaceRunner struct {
	mu         sync.Mutex
	workspaces []string
}

func (r *workspaceRunner) Create(context.Context, session.RunOptions) error { return nil }

func (r *workspaceRunner) FailSetup(context.Context, string, error) error { return nil }

func (r *workspaceRunner) Run(_ context.Context, opts session.RunOptions) (*session.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaces = append(r.workspaces, opts.Workspace)
	return &session.RunResult{Status: store.StatusOK}, nil
}

func (r *workspaceRunner) Resume(_ context.Context, opts session.ResumeOptions) (*session.RunResult, error) {
	return &session.RunResult{SessionID: opts.SessionID, Status: store.StatusOK}, nil
}

func (r *workspaceRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.workspaces...)
}

// testPacks stands in for assets.SkillPacks(). The pack name is a real one so
// the request passes queue validation, which is part of what is being tested:
// a pack has to survive the queue as well as the pool.
func testPacks() map[string]fs.FS {
	return map[string]fs.FS{
		skills.PackUnity: fstest.MapFS{
			"README.md":       &fstest.MapFile{Data: []byte("not a skill")},
			"packed/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: packed\ndescription: from the pack.\n---\n")},
		},
	}
}

func testAlwaysOn() fs.FS {
	return fstest.MapFS{
		"always/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: always\ndescription: every run gets this.\n---\n")},
	}
}

func skillPath(workspace, skill string) string {
	return filepath.Join(workspace, skills.WorkspaceSkillsDir, skill, "SKILL.md")
}

func waitForWorkspace(t *testing.T, runner *workspaceRunner) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if seen := runner.seen(); len(seen) > 0 {
			return seen[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run never started")
	return ""
}

// The default is nothing. A request that names no packs gets the always-on
// tree and not one file more — which is the property the whole opt-in design
// exists to provide, since a pack's descriptions would otherwise ride in
// every request of every run that has nothing to do with it.
func TestPoolInstallsNoPackByDefault(t *testing.T) {
	runner := &workspaceRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(*store.Store) Runner { return runner })
	h.pool.SkillsFS = testAlwaysOn()
	h.pool.SkillPacks = testPacks()
	stop := h.startPool(t)
	defer stop()

	h.publish(t, queue.Request{
		RequestID:      uniqueID("req"),
		Prompt:         "go",
		Repos:          testRepos(),
		PermissionMode: "full",
	})

	ws := waitForWorkspace(t, runner)
	if _, err := os.Stat(skillPath(ws, "always")); err != nil {
		t.Errorf("the always-on tree did not land: %v", err)
	}
	if _, err := os.Stat(skillPath(ws, "packed")); err == nil {
		t.Error("a pack was installed for a request that did not ask for one")
	}
}

// A request that names a pack gets it, in the same skills/ directory as the
// always-on tree, so discovery lists the two together and the session cannot
// tell which came from where.
func TestPoolInstallsTheRequestedPack(t *testing.T) {
	runner := &workspaceRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(*store.Store) Runner { return runner })
	h.pool.SkillsFS = testAlwaysOn()
	h.pool.SkillPacks = testPacks()
	stop := h.startPool(t)
	defer stop()

	h.publish(t, queue.Request{
		RequestID:      uniqueID("req"),
		Prompt:         "go",
		Repos:          testRepos(),
		PermissionMode: "full",
		SkillPacks:     []string{skills.PackUnity},
	})

	ws := waitForWorkspace(t, runner)
	for _, skill := range []string{"always", "packed"} {
		if _, err := os.Stat(skillPath(ws, skill)); err != nil {
			t.Errorf("skill %q did not land: %v", skill, err)
		}
	}
	// The pack's own README explains the pack to whoever maintains it and
	// means nothing to a session; Install leaves it behind for the same
	// reason it leaves the always-on tree's README behind.
	if _, err := os.Stat(filepath.Join(ws, skills.WorkspaceSkillsDir, "README.md")); err == nil {
		t.Error("the pack's README was copied into the workspace")
	}
}

// A build that validates a pack name it does not carry must still run the
// task. The run is worse — it lacks skills somebody asked for — but the work
// the request described does not depend on them, which is the same posture
// Install itself takes towards a skill that fails to copy.
func TestPoolRunsWhenAPackIsMissingFromTheBuild(t *testing.T) {
	runner := &workspaceRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(*store.Store) Runner { return runner })
	h.pool.SkillsFS = testAlwaysOn()
	h.pool.SkillPacks = nil // this build carries no packs at all
	stop := h.startPool(t)
	defer stop()

	h.publish(t, queue.Request{
		RequestID:      uniqueID("req"),
		Prompt:         "go",
		Repos:          testRepos(),
		PermissionMode: "full",
		SkillPacks:     []string{skills.PackUnity},
	})

	ws := waitForWorkspace(t, runner)
	if _, err := os.Stat(skillPath(ws, "always")); err != nil {
		t.Errorf("the always-on tree did not land: %v", err)
	}
}
