package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestValidPack(t *testing.T) {
	for _, name := range PackNames() {
		if !ValidPack(name) {
			t.Errorf("ValidPack(%q) = false, want true", name)
		}
	}
	// Case and whitespace are not forgiven: the name is an identifier a
	// producer sends, not something a person types into a search box, and a
	// silently-normalised name is a name two layers can disagree about.
	for _, name := range []string{"", "Unity", "unity ", "godot"} {
		if ValidPack(name) {
			t.Errorf("ValidPack(%q) = true, want false", name)
		}
	}
}

// The empty list is the case that matters most: it is what every producer
// sends unless somebody opted in, so it must not be an error.
func TestValidatePacksAcceptsNothing(t *testing.T) {
	if err := ValidatePacks(nil); err != nil {
		t.Errorf("ValidatePacks(nil) = %v, want nil", err)
	}
	if err := ValidatePacks([]string{}); err != nil {
		t.Errorf("ValidatePacks(empty) = %v, want nil", err)
	}
}

func TestValidatePacksRejectsUnknownAndRepeats(t *testing.T) {
	err := ValidatePacks([]string{"nope"})
	if err == nil {
		t.Fatal("expected an unknown pack to be rejected")
	}
	// The message names the known packs, because the caller that got this
	// wrong is often a model filling in a tool argument, and a list is what
	// lets it correct itself without another round trip.
	if !strings.Contains(err.Error(), PackUnity) {
		t.Errorf("error should list the known packs, got %v", err)
	}

	if err := ValidatePacks([]string{PackUnity, PackUnity}); err == nil {
		t.Error("expected a repeated pack to be rejected")
	}
}

// PackNames must not hand out the backing slice: a caller sorting or
// appending to it would rewrite the vocabulary for every later call.
func TestPackNamesReturnsACopy(t *testing.T) {
	got := PackNames()
	if len(got) == 0 {
		t.Fatal("PackNames returned nothing")
	}
	got[0] = "clobbered"
	if PackNames()[0] == "clobbered" {
		t.Error("PackNames handed out the backing array")
	}
}

// A pack installs through the same Install call the always-on tree uses, and
// the two land in one directory. This is what lets the catalogue list them
// together, and what makes a pack nothing more than a tree nobody installs by
// default.
func TestInstallPackMergesWithTheAlwaysOnTree(t *testing.T) {
	base := fstest.MapFS{
		"house-style/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: house-style\ndescription: how we write.\n---\n")},
	}
	pack := fstest.MapFS{
		"README.md":         &fstest.MapFile{Data: []byte("not a skill")},
		"LICENSE.md":        &fstest.MapFile{Data: []byte("not a skill either")},
		"widget/SKILL.md":   &fstest.MapFile{Data: []byte("---\nname: widget\ndescription: widgets.\n---\n")},
		"widget/refs/a.md":  &fstest.MapFile{Data: []byte("detail")},
		"gadget/SKILL.md":   &fstest.MapFile{Data: []byte("---\nname: gadget\ndescription: gadgets.\n---\n")},
		"loose/NOTSKILL.md": &fstest.MapFile{Data: []byte("no frontmatter, no skill")},
	}

	ws := t.TempDir()
	if n, err := Install(base, ws); err != nil || n != 1 {
		t.Fatalf("Install(base) = %d, %v; want 1, nil", n, err)
	}
	if n, err := Install(pack, ws); err != nil || n != 2 {
		t.Fatalf("Install(pack) = %d, %v; want 2, nil", n, err)
	}

	cat := Discover(ws)
	var names []string
	for _, s := range cat.Skills {
		names = append(names, s.Name)
	}
	want := map[string]bool{"house-style": true, "widget": true, "gadget": true}
	if len(names) != len(want) {
		t.Fatalf("catalogue names = %v, want exactly %v", names, want)
	}
	for _, name := range names {
		if !want[name] {
			t.Errorf("catalogue holds unexpected skill %q", name)
		}
	}

	// A skill's own files come across whole — a SKILL.md that references a
	// file which did not travel is worse than no skill at all — while the
	// pack's top-level README and LICENSE stay behind, the same rule that
	// keeps the always-on tree's README out of a workspace.
	if _, err := os.Stat(filepath.Join(ws, WorkspaceSkillsDir, "widget", "refs", "a.md")); err != nil {
		t.Errorf("a skill's reference file did not come across: %v", err)
	}
	for _, left := range []string{"README.md", "LICENSE.md"} {
		if _, err := os.Stat(filepath.Join(ws, WorkspaceSkillsDir, left)); err == nil {
			t.Errorf("the pack's %s was copied into the workspace", left)
		}
	}
}
