// Package assets carries the files this repository ships rather than runs.
//
// Two of the three directories here are embedded. assets/skills holds Claude
// Code skills for driving the harness from outside; they are copied into a
// user's own skills directory by hand and the binary has no use for them
// (assets/skills/README.md). assets/agent-skills holds skills for the
// harness's OWN sessions, which the binary does need to carry: they are
// written into every prepared workspace's skills/ directory
// (internal/skills.Install), where discovery finds them beside each cloned
// repository's .claude/skills. assets/skill-packs holds the same kind of
// thing, one directory deeper and on the opposite default: a pack reaches a
// session only when that session's request named it, so a run that wants
// nothing to do with Unity pays nothing for the Unity pack
// (internal/skills, "Packs").
//
// The package sits at the repository root rather than under internal/
// because go:embed cannot reach outside its own package directory: a package
// at internal/agentskills could not embed ../../assets/agent-skills, and the
// alternatives are a build step that copies the tree (the webassets/dist
// arrangement, which fails silently when skipped) or moving the authored
// files somewhere less obvious than assets/.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed all:agent-skills
var agentSkills embed.FS

//go:embed all:skill-packs
var skillPacks embed.FS

// AgentSkills is the shipped skill tree, rooted so that each entry is one
// skill directory — the shape internal/skills.Install and Discover both
// expect. A caller that wants no shipped skills passes nil rather than this.
func AgentSkills() fs.FS {
	return sub(agentSkills, "agent-skills")
}

// SkillPacks is every optional pack this build carries, keyed by the name a
// request names it by (internal/skills.PackNames). Each value has the same
// shape AgentSkills does — one entry per skill directory — so the worker
// installs a pack with the same internal/skills.Install call it uses for the
// always-on tree, and a pack's own README and LICENSE are skipped for the
// same reason that tree's README is: they are not directories holding a
// SKILL.md.
//
// Composition hands this to the worker pool whole. Nothing here decides which
// packs a run gets; the request does.
func SkillPacks() map[string]fs.FS {
	root := sub(skillPacks, "skill-packs")
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		// Unreachable for the same reason sub's panic is: the directory is
		// embedded above.
		panic("assets: skill-packs unreadable in the embedded tree: " + err.Error())
	}
	packs := make(map[string]fs.FS, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		packs[entry.Name()] = sub(root, entry.Name())
	}
	return packs
}

// sub roots an embedded tree at one of its directories.
func sub(fsys fs.FS, dir string) fs.FS {
	out, err := fs.Sub(fsys, dir)
	if err != nil {
		// Unreachable: the directory is embedded above, so the sub-path
		// exists in every binary that compiled. Panicking here rather than
		// returning an error keeps the caller's composition free of a branch
		// that cannot be taken.
		panic("assets: " + dir + " missing from the embedded tree: " + err.Error())
	}
	return out
}
