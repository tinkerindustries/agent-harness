package assets

import (
	"io/fs"
	"path"
	"sort"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
)

// TestSkillPacksMatchKnownNames is the one thing holding the two halves of a
// pack together. internal/skills names the packs a request may ask for so
// that queue validation does not have to reach the embedded tree, and this
// directory holds their bytes; nothing at compile time makes the two agree.
// Without this test, a pack added here but not named there validates as
// unknown and can never be installed, and a name added there but not here
// passes validation and then quietly installs nothing.
func TestSkillPacksMatchKnownNames(t *testing.T) {
	embedded := make([]string, 0)
	for name := range SkillPacks() {
		embedded = append(embedded, name)
	}
	sort.Strings(embedded)

	known := skills.PackNames()
	if len(embedded) != len(known) {
		t.Fatalf("embedded packs %v, known pack names %v", embedded, known)
	}
	for i := range known {
		if embedded[i] != known[i] {
			t.Fatalf("embedded packs %v, known pack names %v", embedded, known)
		}
	}
}

// TestSkillPacksHoldInstallableSkills checks each pack is the shape
// internal/skills.Install expects — top-level directories holding a SKILL.md
// — rather than a tree one level out. A pack nested one directory too deep
// installs nothing at all and reports no error, because Install's rule for
// "not a skill" and the rule for "not this pack's fault" are the same rule.
func TestSkillPacksHoldInstallableSkills(t *testing.T) {
	for name, pack := range SkillPacks() {
		entries, err := fs.ReadDir(pack, ".")
		if err != nil {
			t.Fatalf("pack %s: read: %v", name, err)
		}
		var found int
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if _, err := fs.Stat(pack, path.Join(entry.Name(), "SKILL.md")); err == nil {
				found++
			}
		}
		if found == 0 {
			t.Errorf("pack %s holds no directory with a SKILL.md; check the vendoring depth", name)
		}
	}
}

// TestUnityPackCarriesItsLicence guards the one condition attached to
// vendoring Unity's skills: they are under the Unity Companion License, not a
// permissive one, and the licence file travels with them
// (assets/skill-packs/unity/README.md). A refresh that copies the skills over
// the top and drops LICENSE.md is the way this would be lost.
func TestUnityPackCarriesItsLicence(t *testing.T) {
	pack, ok := SkillPacks()[skills.PackUnity]
	if !ok {
		t.Fatalf("no %s pack embedded", skills.PackUnity)
	}
	if _, err := fs.Stat(pack, "LICENSE.md"); err != nil {
		t.Errorf("the %s pack has no LICENSE.md: %v", skills.PackUnity, err)
	}
}

// TestEveryPackedSkillReachesTheCatalogue is the guard against the failure
// mode a vendored tree makes easy: a SKILL.md that installs perfectly well
// and then yields no catalogue entry, because its frontmatter did not parse
// or carried no description. Nothing reports that at runtime — discovery is
// deliberately forgiving, so the skill simply is not mentioned to the model —
// and the files are still sitting in the workspace looking installed.
//
// It has caught one already: upstream's physics-3d-collision carried an
// unquoted description containing ": ", which is not valid YAML, so the skill
// vanished from the catalogue while its 33 KB still landed in every opted-in
// workspace. It is not vendored (assets/skill-packs/unity/README.md). A
// refresh that brings back a malformed skill should fail here rather than
// quietly shipping a directory no session can be told about.
func TestEveryPackedSkillReachesTheCatalogue(t *testing.T) {
	for name, pack := range SkillPacks() {
		ws := t.TempDir()
		installed, err := skills.Install(pack, ws)
		if err != nil {
			t.Fatalf("pack %s: install: %v", name, err)
		}
		cat := skills.Discover(ws)
		if cat.Dropped > 0 {
			t.Fatalf("pack %s: %d skills over the catalogue cap on its own", name, cat.Dropped)
		}
		if len(cat.Skills) != installed {
			listed := make(map[string]bool, len(cat.Skills))
			for _, s := range cat.Skills {
				listed[s.Name] = true
			}
			entries, _ := fs.ReadDir(pack, ".")
			var missing []string
			for _, entry := range entries {
				if !entry.IsDir() || listed[entry.Name()] {
					continue
				}
				if _, err := fs.Stat(pack, path.Join(entry.Name(), "SKILL.md")); err == nil {
					missing = append(missing, entry.Name())
				}
			}
			t.Errorf("pack %s: installed %d skills but only %d reached the catalogue; check the frontmatter of %v",
				name, installed, len(cat.Skills), missing)
		}
	}
}
