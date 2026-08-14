package skills

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func shippedFS() fstest.MapFS {
	return fstest.MapFS{
		"vision-tools/SKILL.md":            {Data: []byte(validSkill)},
		"vision-tools/references/gui.md":   {Data: []byte("# GUI\n")},
		"vision-tools/scripts/shot.py":     {Data: []byte("print('hi')\n")},
		"README.md":                        {Data: []byte("# Skills we ship\n")},
		"not-a-skill/notes.md":             {Data: []byte("# Notes\n")},
		"workspace-conventions/SKILL.md":   {Data: []byte(validSkill)},
		"workspace-conventions/extra.json": {Data: []byte("{}\n")},
	}
}

// Install writes every directory holding a SKILL.md, with its own files, and
// nothing else: the source tree's README explains the tree to a human and has
// no business in a session's workspace.
func TestInstallWritesSkillsAndSkipsTheRest(t *testing.T) {
	ws := t.TempDir()

	n, err := Install(shippedFS(), ws)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if n != 2 {
		t.Fatalf("installed %d skills, want 2", n)
	}

	for _, want := range []string{
		"vision-tools/SKILL.md",
		"vision-tools/references/gui.md",
		"vision-tools/scripts/shot.py",
		"workspace-conventions/SKILL.md",
		"workspace-conventions/extra.json",
	} {
		if _, err := os.Stat(filepath.Join(ws, WorkspaceSkillsDir, filepath.FromSlash(want))); err != nil {
			t.Errorf("%s not installed: %v", want, err)
		}
	}
	for _, gone := range []string{"README.md", "not-a-skill"} {
		if _, err := os.Stat(filepath.Join(ws, WorkspaceSkillsDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should not have been installed", gone)
		}
	}
}

// The whole point of installing into the workspace is that Discover then
// finds them, so the two halves are tested together rather than trusted to
// agree.
func TestInstalledSkillsAreDiscovered(t *testing.T) {
	ws := t.TempDir()
	if _, err := Install(shippedFS(), ws); err != nil {
		t.Fatalf("Install: %v", err)
	}

	cat := Discover(ws)
	if len(cat.Skills) != 2 {
		t.Fatalf("discovered %d skills, want 2", len(cat.Skills))
	}
	if cat.Skills[0].Path != "skills/vision-tools/SKILL.md" {
		t.Errorf("path = %q", cat.Skills[0].Path)
	}
}

// A nil tree is how a deployment says it ships no skills. It is not an error
// and it does not create the directory — internal/workspace does that.
func TestInstallNilShipsNothing(t *testing.T) {
	ws := t.TempDir()

	n, err := Install(nil, ws)
	if err != nil {
		t.Fatalf("Install(nil): %v", err)
	}
	if n != 0 {
		t.Fatalf("installed %d skills from nil, want 0", n)
	}
}

// Installing twice is what a re-prepared workspace does. The second write
// replaces the first rather than failing or appending to it.
func TestInstallIsRepeatable(t *testing.T) {
	ws := t.TempDir()
	if _, err := Install(shippedFS(), ws); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	target := filepath.Join(ws, WorkspaceSkillsDir, "vision-tools", "SKILL.md")
	if err := os.WriteFile(target, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	if _, err := Install(shippedFS(), ws); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) == "stale\n" {
		t.Fatal("second Install did not replace the file")
	}
}

// A skill directory that cannot be written is one catalogue entry missing,
// not a failed run: Install reports the failure and still installs the rest.
func TestInstallReportsFailureAndContinues(t *testing.T) {
	ws := t.TempDir()
	// A file where a skill's directory needs to be makes that one skill
	// unwritable while leaving the other alone.
	blocked := filepath.Join(ws, WorkspaceSkillsDir)
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "vision-tools"), []byte("in the way\n"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	n, err := Install(shippedFS(), ws)
	if err == nil {
		t.Fatal("Install should report the skill it could not write")
	}
	if n != 1 {
		t.Fatalf("installed %d skills, want the 1 that was not blocked", n)
	}
}
