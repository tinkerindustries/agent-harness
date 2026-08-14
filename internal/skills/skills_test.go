package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates dir/SKILL.md with the given content, making parents.
func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill in %s: %v", dir, err)
	}
}

const validSkill = `---
name: test-runner
description: Run this repository's test suite the way CI does.
---

# Test runner

Run scripts/test.sh.
`

// A queue-driven run clones each repository into its own subdirectory of the
// workspace, so skills sit two levels down.
func TestDiscoverClonedRepoLayout(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "myrepo", ".claude", "skills", "test-runner"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1: %+v", len(cat.Skills), cat.Skills)
	}
	got := cat.Skills[0]
	if got.Name != "test-runner" {
		t.Errorf("name = %q, want test-runner", got.Name)
	}
	if got.Description != "Run this repository's test suite the way CI does." {
		t.Errorf("description = %q", got.Description)
	}
	if got.Path != "myrepo/.claude/skills/test-runner/SKILL.md" {
		t.Errorf("path = %q", got.Path)
	}
}

// A CLI run points the workspace straight at a checkout, so skills sit one
// level higher than the cloned-repo case.
func TestDiscoverWorkspaceIsCheckout(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".claude", "skills", "test-runner"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	if cat.Skills[0].Path != ".claude/skills/test-runner/SKILL.md" {
		t.Errorf("path = %q", cat.Skills[0].Path)
	}
}

func TestDiscoverDeepcodeConvention(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "myrepo", ".deepcode", "skills", "test-runner"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	if cat.Skills[0].Path != "myrepo/.deepcode/skills/test-runner/SKILL.md" {
		t.Errorf("path = %q", cat.Skills[0].Path)
	}
}

// A skill the parser cannot use is skipped rather than failing the run: the
// repository is being worked on, not validated.
func TestDiscoverSkipsUnusable(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":    "# Just a heading\n\nSome prose.\n",
		"unterminated":      "---\nname: x\ndescription: y\n",
		"no description":    "---\nname: lonely\n---\n\nBody.\n",
		"empty description": "---\nname: lonely\ndescription: \"\"\n---\n\nBody.\n",
		"broken yaml":       "---\nname: [unclosed\n---\n\nBody.\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			writeSkill(t, filepath.Join(ws, "repo", ".claude", "skills", "candidate"), content)
			if cat := Discover(ws); len(cat.Skills) != 0 {
				t.Errorf("got %d skills, want 0: %+v", len(cat.Skills), cat.Skills)
			}
		})
	}
}

// A skill directory with no SKILL.md at all is not a skill.
func TestDiscoverIgnoresDirWithoutSkillFile(t *testing.T) {
	ws := t.TempDir()
	stray := filepath.Join(ws, "repo", ".claude", "skills", "empty")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if cat := Discover(ws); len(cat.Skills) != 0 {
		t.Errorf("got %d skills, want 0", len(cat.Skills))
	}
}

func TestDiscoverNameFallsBackToDirectory(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "repo", ".claude", "skills", "release-notes"),
		"---\ndescription: Draft the release notes.\n---\n\nBody.\n")

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	if cat.Skills[0].Name != "release-notes" {
		t.Errorf("name = %q, want release-notes", cat.Skills[0].Name)
	}
}

// Descriptions in the wild use folded blocks and contain colons; both have to
// survive into a single catalogue line.
func TestDiscoverCollapsesFoldedDescription(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "repo", ".claude", "skills", "deploy"), `---
name: deploy
description: >
  Deploy the service: build the image, push it,
  then roll the deployment.
---

Body.
`)

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	want := "Deploy the service: build the image, push it, then roll the deployment."
	if cat.Skills[0].Description != want {
		t.Errorf("description = %q, want %q", cat.Skills[0].Description, want)
	}
}

func TestDiscoverTruncatesLongDescription(t *testing.T) {
	ws := t.TempDir()
	long := strings.Repeat("verbose ", 200)
	writeSkill(t, filepath.Join(ws, "repo", ".claude", "skills", "wordy"),
		"---\nname: wordy\ndescription: "+long+"\n---\n\nBody.\n")

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	got := cat.Skills[0].Description
	if len(got) > MaxDescriptionLen+len("…") {
		t.Errorf("description is %d bytes, want at most %d", len(got), MaxDescriptionLen+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated description should be marked: %q", got)
	}
}

func TestDiscoverHandlesCRLF(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "repo", ".claude", "skills", "windows"),
		"---\r\nname: windows\r\ndescription: Works from a CRLF checkout.\r\n---\r\n\r\nBody.\r\n")

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1: %+v", len(cat.Skills), cat.Skills)
	}
	if cat.Skills[0].Description != "Works from a CRLF checkout." {
		t.Errorf("description = %q", cat.Skills[0].Description)
	}
}

// Two repositories in one request can ship a skill of the same name. Both are
// listed, distinguished by path, in a stable order.
func TestDiscoverListsSameNameFromTwoRepos(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "beta", ".claude", "skills", "test-runner"), validSkill)
	writeSkill(t, filepath.Join(ws, "alpha", ".claude", "skills", "test-runner"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 2 {
		t.Fatalf("got %d skills, want 2: %+v", len(cat.Skills), cat.Skills)
	}
	if cat.Skills[0].Path != "alpha/.claude/skills/test-runner/SKILL.md" ||
		cat.Skills[1].Path != "beta/.claude/skills/test-runner/SKILL.md" {
		t.Errorf("order is not path-sorted: %q then %q", cat.Skills[0].Path, cat.Skills[1].Path)
	}
}

func TestDiscoverCapsSkillCount(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < MaxSkills+3; i++ {
		dir := filepath.Join(ws, "repo", ".claude", "skills", "skill-"+string(rune('a'+i/26))+string(rune('a'+i%26)))
		writeSkill(t, dir, validSkill)
	}

	cat := Discover(ws)
	if len(cat.Skills) != MaxSkills {
		t.Errorf("got %d skills, want %d", len(cat.Skills), MaxSkills)
	}
	if cat.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", cat.Dropped)
	}
	if !strings.Contains(cat.Render(), "3 further skills") {
		t.Errorf("render should say what was dropped:\n%s", cat.Render())
	}
}

func TestDiscoverMissingWorkspace(t *testing.T) {
	cat := Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(cat.Skills) != 0 || cat.Dropped != 0 {
		t.Errorf("want an empty catalogue, got %+v", cat)
	}
}

// An empty catalogue renders to nothing so the opening message stays
// byte-identical to a run with no skills (docs/CACHE.md).
func TestRenderEmpty(t *testing.T) {
	if got := (Catalogue{}).Render(); got != "" {
		t.Errorf("render of an empty catalogue = %q, want empty", got)
	}
}

func TestRenderNamesPathAndDescription(t *testing.T) {
	cat := Catalogue{Skills: []Skill{
		{Name: "test-runner", Description: "Run the suite.", Path: "repo/.claude/skills/test-runner/SKILL.md"},
	}}
	got := cat.Render()
	for _, want := range []string{"test-runner", "repo/.claude/skills/test-runner/SKILL.md", "Run the suite.", "Read"} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "further skill") {
		t.Errorf("render should not mention dropped skills when none were:\n%s", got)
	}
}

// The harness's own skills sit directly under <workspace>/skills, outside
// every repository, so giving a session a skill never shows up in a clone's
// diff.
func TestDiscoverWorkspaceSkillsDir(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, WorkspaceSkillsDir, "vision-tools"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cat.Skills))
	}
	if cat.Skills[0].Path != "skills/vision-tools/SKILL.md" {
		t.Errorf("path = %q", cat.Skills[0].Path)
	}
}

// A workspace skill and a repository skill coexist: the workspace's is not a
// replacement for what a repository ships, and neither hides the other.
func TestDiscoverWorkspaceAndRepoSkills(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, WorkspaceSkillsDir, "vision-tools"), validSkill)
	writeSkill(t, filepath.Join(ws, "myrepo", ".claude", "skills", "test-runner"), validSkill)

	cat := Discover(ws)
	if len(cat.Skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(cat.Skills))
	}
	var paths []string
	for _, s := range cat.Skills {
		paths = append(paths, s.Path)
	}
	want := []string{"myrepo/.claude/skills/test-runner/SKILL.md", "skills/vision-tools/SKILL.md"}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("path[%d] = %q, want %q", i, paths[i], w)
		}
	}
}

// An empty skills directory is the normal state — internal/workspace creates
// it on every run whether or not anything ever lands in it.
func TestDiscoverEmptyWorkspaceSkillsDir(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, WorkspaceSkillsDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if cat := Discover(ws); len(cat.Skills) != 0 {
		t.Fatalf("got %d skills, want 0", len(cat.Skills))
	}
}
