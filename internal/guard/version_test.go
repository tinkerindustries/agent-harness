package guard

import "testing"

func TestVersionChangesWithEachInput(t *testing.T) {
	base := VersionInputs{Rules: "r1", Cutoff: DefaultCutoff, Stage1Model: "clef-flash", Stage2Model: "claude-haiku-5-5"}
	p, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	ref := versionOf(base, p)
	if again := versionOf(base, p); again != ref {
		t.Fatalf("not stable: %s vs %s", again, ref)
	}
	variants := map[string]string{}
	for name, in := range map[string]VersionInputs{
		"rules":         {Rules: "r2", Cutoff: base.Cutoff, Stage1Model: base.Stage1Model, Stage2Model: base.Stage2Model},
		"cutoff":        {Rules: base.Rules, Cutoff: 0.2, Stage1Model: base.Stage1Model, Stage2Model: base.Stage2Model},
		"stage 1 model": {Rules: base.Rules, Cutoff: base.Cutoff, Stage1Model: "clef-other", Stage2Model: base.Stage2Model},
		"stage 2 model": {Rules: base.Rules, Cutoff: base.Cutoff, Stage1Model: base.Stage1Model, Stage2Model: "claude-sonnet-5-5"},
	} {
		variants[name] = versionOf(in, p)
	}
	p1 := p
	p1.Stage1Questions = append([]byte(nil), p.Stage1Questions...)
	p1.Stage1Questions[len(p1.Stage1Questions)-3] ^= 1
	variants["stage 1 prompt"] = versionOf(base, p1)
	p2 := p
	p2.Stage2System += " "
	variants["stage 2 prompt"] = versionOf(base, p2)
	p3 := p
	p3.Stage2Schema = append([]byte(nil), p.Stage2Schema...)
	p3.Stage2Schema[3] ^= 1
	variants["stage 2 schema"] = versionOf(base, p3)

	seen := map[string]string{ref: "base"}
	for name, v := range variants {
		if prev, dup := seen[v]; dup {
			t.Errorf("changing %s gives the same version as %s: %s", name, prev, v)
		}
		seen[v] = name
	}
}

func TestVersionDefaultsModelsAndIsPublicAPI(t *testing.T) {
	a, err := Version(VersionInputs{Rules: "r", Cutoff: DefaultCutoff})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Version(VersionInputs{Rules: "r", Cutoff: DefaultCutoff, Stage1Model: DefaultStage1Model, Stage2Model: DefaultStage2Model})
	if a != b || a == "" {
		t.Fatalf("a=%s b=%s", a, b)
	}
}

func TestVersionFieldBoundariesDoNotCollide(t *testing.T) {
	p, _ := loadPrompts()
	a := versionOf(VersionInputs{Rules: "ab", Stage1Model: "c"}, p)
	b := versionOf(VersionInputs{Rules: "a", Stage1Model: "bc"}, p)
	if a == b {
		t.Fatal("shifting a byte between fields keeps the version")
	}
}
