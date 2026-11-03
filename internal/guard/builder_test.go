package guard

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeDisk struct {
	files map[string]string
	mtime map[string]time.Time
}

func (d fakeDisk) IsDir(string) bool    { return false }
func (d fakeDisk) IsFile(p string) bool { _, ok := d.files[p]; return ok }
func (d fakeDisk) ReadText(p string) (string, error) {
	if s, ok := d.files[p]; ok {
		return s, nil
	}
	return "", errors.New("missing")
}
func (d fakeDisk) ModTime(p string) (time.Time, bool) {
	t, ok := d.mtime[p]
	return t, ok
}

var sessionStart = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func noBranch(context.Context, string) string { return "feature/x" }

func buildState(disk fakeDisk, in Inputs) State {
	if in.Directory == "" {
		in.Directory = "/ws"
	}
	if in.SessionStart.IsZero() {
		in.SessionStart = sessionStart
	}
	return StateBuilder{FS: disk, Branch: noBranch}.Build(context.Background(), in)
}

func TestRecentCommandsKeepsLastThreeOldestFirst(t *testing.T) {
	got := RecentCommands([]string{"one", "two", "three", "four", "five"})
	want := []string{"three", "four", "five"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("RecentCommands = %q, want %q", got, want)
	}
	if got := RecentCommands(nil); got == nil || len(got) != 0 {
		t.Fatalf("RecentCommands(nil) = %#v, want an empty non-nil slice", got)
	}
}

func TestRecentCommandsCutAt200WithEllipsis(t *testing.T) {
	exact := strings.Repeat("a", 200)
	long := strings.Repeat("é", 201)
	got := RecentCommands([]string{exact, long})
	if got[0] != exact {
		t.Errorf("a 200 character command was cut: %q", got[0])
	}
	want := strings.Repeat("é", 200) + "…"
	if got[1] != want {
		t.Errorf("a 201 character command = %d runes, want %d runes ending in an ellipsis", len([]rune(got[1])), len([]rune(want)))
	}
}

func TestBuildFillsFixedFields(t *testing.T) {
	st := buildState(fakeDisk{}, Inputs{
		Command:  "echo hi",
		Commands: []string{"a", "b", "c", "d"},
	})
	if st.WorkingDirectory != "/ws" || st.Branch != "feature/x" || st.ProposedCommand != "echo hi" {
		t.Fatalf("fixed fields wrong: %+v", st)
	}
	if strings.Join(st.RecentCommands, "") != "bcd" {
		t.Fatalf("RecentCommands = %q", st.RecentCommands)
	}
	if len(st.ScriptsRun) != 0 {
		t.Fatalf("ScriptsRun = %+v, want none", st.ScriptsRun)
	}
}

func TestScriptOrigins(t *testing.T) {
	after := sessionStart.Add(time.Minute)
	before := sessionStart.Add(-time.Hour)
	disk := fakeDisk{
		files: map[string]string{
			"/ws/new.sh":      "echo new\n",
			"/ws/edited.sh":   "echo edited\n",
			"/ws/old.sh":      "echo old\n",
			"/ws/by_shell.sh": "echo shell\n",
		},
		mtime: map[string]time.Time{
			"/ws/new.sh": after, "/ws/edited.sh": after, "/ws/old.sh": before, "/ws/by_shell.sh": after,
		},
	}
	in := Inputs{
		Command: "bash new.sh && bash edited.sh && bash old.sh && bash by_shell.sh",
		Mutations: []Mutation{
			{Path: "/ws/new.sh", Created: true},
			{Path: "/ws/edited.sh", Created: false},
		},
	}
	got := map[string]ScriptEntry{}
	for _, e := range buildState(disk, in).ScriptsRun {
		got[e.Path] = e
	}
	cases := []struct {
		path, origin string
		content      *string
	}{
		{"new.sh", OriginWrittenThisSession, ptr("echo new\n")},
		{"edited.sh", OriginChangedThisSession, ptr("echo edited\n")},
		{"old.sh", OriginExisting, nil},
		{"by_shell.sh", OriginChangedThisSession, ptr("echo shell\n")},
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d scripts, want %d: %+v", len(got), len(cases), got)
	}
	for _, c := range cases {
		e := got[c.path]
		if e.Origin != c.origin {
			t.Errorf("%s origin = %q, want %q", c.path, e.Origin, c.origin)
		}
		if (e.Content == nil) != (c.content == nil) || (c.content != nil && *e.Content != *c.content) {
			t.Errorf("%s content = %v, want %v", c.path, e.Content, c.content)
		}
	}
}

func TestScriptResolvesAgainstWorkdir(t *testing.T) {
	disk := fakeDisk{files: map[string]string{"/ws/sub/run.sh": "x"}, mtime: map[string]time.Time{"/ws/sub/run.sh": sessionStart.Add(time.Second)}}
	st := buildState(disk, Inputs{Command: "bash run.sh", Workdir: "sub"})
	if len(st.ScriptsRun) != 1 || st.ScriptsRun[0].Origin != OriginChangedThisSession || st.ScriptsRun[0].Content == nil {
		t.Fatalf("script not found through the workdir: %+v", st.ScriptsRun)
	}
	if st.ScriptsRun[0].Path != "run.sh" || st.WorkingDirectory != "/ws" {
		t.Fatalf("path and directory should stay as written: %+v", st)
	}
}

func TestPendingWriteIsUnreadableAndEscalates(t *testing.T) {
	disk := fakeDisk{}
	st := buildState(disk, Inputs{Command: "bash setup.sh", Pending: []string{"/ws/setup.sh"}})
	if len(st.ScriptsRun) != 1 {
		t.Fatalf("scripts = %+v", st.ScriptsRun)
	}
	e := st.ScriptsRun[0]
	if e.Origin != OriginWrittenThisSession || e.Content != nil {
		t.Fatalf("a file the round is still writing = %+v, want written with no content", e)
	}
	if got := NewScriptDetector().EscalationReasons("bash setup.sh", st.ScriptsRun); len(got) == 0 {
		t.Fatal("an unreadable session script did not escalate")
	}
}

func TestPendingWriteToExistingFileIsChangedAndUnreadable(t *testing.T) {
	disk := fakeDisk{files: map[string]string{"/ws/setup.sh": "old"}, mtime: map[string]time.Time{"/ws/setup.sh": sessionStart.Add(-time.Hour)}}
	st := buildState(disk, Inputs{Command: "bash setup.sh", Pending: []string{"/ws/setup.sh"}})
	e := st.ScriptsRun[0]
	if e.Origin != OriginChangedThisSession || e.Content != nil {
		t.Fatalf("got %+v, want changed with no content", e)
	}
}

func TestUnreadableSessionScriptHasNilContent(t *testing.T) {
	disk := fakeDisk{mtime: map[string]time.Time{"/ws/gone.sh": sessionStart.Add(time.Second)}}
	st := buildState(disk, Inputs{Command: "bash gone.sh"})
	if e := st.ScriptsRun[0]; e.Origin != OriginChangedThisSession || e.Content != nil {
		t.Fatalf("got %+v, want a session script with no content", e)
	}
}

func TestRepositories(t *testing.T) {
	if got := Repositories("/r/proj/.claude/worktrees/w1"); len(got) != 1 || got[0] != "/r/proj" {
		t.Errorf("worktree directory = %q", got)
	}
	if got := Repositories("/r/proj"); len(got) != 1 || got[0] != "/r/proj" {
		t.Errorf("plain directory = %q", got)
	}
}

func ptr(s string) *string { return &s }

func TestGitBranch(t *testing.T) {
	dir := t.TempDir()
	if got := GitBranch(context.Background(), dir); got != "" {
		t.Fatalf("a directory outside a repository gave %q", got)
	}
	if err := exec.Command("git", "-C", dir, "init", "-q", "-b", "topic").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if got := GitBranch(context.Background(), dir); got != "topic" {
		t.Fatalf("GitBranch = %q, want topic", got)
	}
}
