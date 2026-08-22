package skills

import (
	"fmt"
	"sort"
	"strings"
)

// A pack is a bundle of shipped skills a request opts into by name. It is the
// same tree the always-on directory holds — one subdirectory per skill, each
// with a SKILL.md — installed by the same Install call, into the same
// workspace skills/ directory, and discovered by the same scan. The only
// thing that makes a pack a pack is that nothing installs it unless the
// request said so (assets/skill-packs, internal/worker).
//
// The default is off, and that is the point rather than caution. Every
// skill's description rides in the opening message of every request of the
// run, so an always-on pack of seventeen Unity skills would tax a run about a
// Go service for the whole of its life, and would crowd the repository's own
// skills out of a catalogue capped at MaxSkills
// (assets/agent-skills/README.md makes this argument at length; opting in is
// the answer to it).
//
// Packs are named here rather than derived from the embedded tree so that
// queue.Request.Validate can reject an unknown name without the queue package
// having to reach the assets. The two sides are held together by a test that
// walks the embedded tree and asserts the names agree, so a pack added to
// assets/skill-packs without a line here fails the build's tests rather than
// silently never being installable.
const (
	// PackUnity is Unity Technologies' own skills, vendored
	// (assets/skill-packs/unity/README.md), plus one of ours recording what
	// the Unity CLI cannot do from a session here (docs/UNITY.md).
	PackUnity = "unity"
	// PackBlender is ours rather than vendored, and deliberately small: the
	// Blender MCP server already sends the conceptual material in its own
	// initialize instructions, so this carries only what that server cannot
	// know — which of the two Blenders a tool reaches from in here
	// (assets/skill-packs/blender/README.md, docs/MCP.md).
	PackBlender = "blender"
)

// packNames is every pack this build knows, in the order PackNames returns.
var packNames = []string{PackUnity, PackBlender}

// PackNames returns every known pack name, sorted. The browser's start form
// and the MCP launch tool's schema both render this, so a pack added above
// appears in each without either of them being edited.
func PackNames() []string {
	out := append([]string(nil), packNames...)
	sort.Strings(out)
	return out
}

// ValidPack reports whether name is a pack this build carries.
func ValidPack(name string) bool {
	for _, known := range packNames {
		if name == known {
			return true
		}
	}
	return false
}

// ValidatePacks checks the pack list a producer put on a request: every name
// known, no repeats. An unknown name is refused rather than ignored, because
// a typo that silently installs nothing is a run that quietly lacks the
// skills someone asked for — and the failure would show up as the model not
// knowing something, which is the hardest kind of bug to trace back to a
// misspelt argument.
func ValidatePacks(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !ValidPack(name) {
			return fmt.Errorf("unknown skill pack %q: known packs are %s", name, strings.Join(PackNames(), ", "))
		}
		if seen[name] {
			return fmt.Errorf("skill pack %q named more than once", name)
		}
		seen[name] = true
	}
	return nil
}
