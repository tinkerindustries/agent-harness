package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/skills"
)

// The directory name is spelled in two packages — here, which creates it, and
// internal/skills, which scans it — so that neither depends on the other at
// build time. A test dependency is free of that constraint, so the agreement
// is pinned here rather than left to a comment. Drift would be silent: the
// directory would be created and never scanned.
func TestSkillsDirMatchesSkillsPackage(t *testing.T) {
	if skillsDir != skills.WorkspaceSkillsDir {
		t.Fatalf("workspace creates %q but internal/skills scans %q", skillsDir, skills.WorkspaceSkillsDir)
	}
}

// Prepare creates the skills directory empty, beside scratch/. Empty is the
// normal state: nothing populates it yet, and discovery of an empty directory
// yields no entries and no error.
func TestPrepareCreatesSkillsDir(t *testing.T) {
	root := t.TempDir()
	dir, err := Prepare(context.Background(), root, "sess-skills", nil, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, skillsDir))
	if err != nil {
		t.Fatalf("stat skills dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", skillsDir)
	}
	entries, err := os.ReadDir(filepath.Join(dir, skillsDir))
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("skills dir should start empty, holds %d entries", len(entries))
	}
}
