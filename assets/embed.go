// Package assets carries the files this repository ships rather than runs.
//
// Only one of the two directories here is embedded. assets/skills holds
// Claude Code skills for driving the harness from outside; they are copied
// into a user's own skills directory by hand and the binary has no use for
// them (assets/skills/README.md). assets/agent-skills holds skills for the
// harness's OWN sessions, which the binary does need to carry: they are
// written into every prepared workspace's skills/ directory
// (internal/skills.Install), where discovery finds them beside each cloned
// repository's .claude/skills.
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

// AgentSkills is the shipped skill tree, rooted so that each entry is one
// skill directory — the shape internal/skills.Install and Discover both
// expect. A caller that wants no shipped skills passes nil rather than this.
func AgentSkills() fs.FS {
	sub, err := fs.Sub(agentSkills, "agent-skills")
	if err != nil {
		// Unreachable: the directory is embedded above, so the sub-path
		// exists in every binary that compiled. Panicking here rather than
		// returning an error keeps the caller's composition free of a branch
		// that cannot be taken.
		panic("assets: agent-skills missing from the embedded tree: " + err.Error())
	}
	return sub
}
