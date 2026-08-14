// Package skills discovers Agent Skills shipped inside the repositories a
// run cloned and renders a catalogue of them for the opening user message.
//
// A skill is a directory holding a SKILL.md whose YAML frontmatter carries a
// name and a description. Only that one-line description reaches the model up
// front; the body is read on demand with the Read tool, which is why the
// catalogue names each skill's path. Nothing here touches the system prompt
// or the tool array, so adding skills to a repository does not disturb the
// cached head (docs/CACHE.md).
package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// skillDirs are the conventions scanned inside each repository.
// .claude/skills is what repositories in the wild carry; .deepcode/skills is
// where DeepSeek's own Deep Code agent looks
// (third_party/deepseek-docs/quick_start/agent_integrations/deepcode.md).
var skillDirs = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".deepcode", "skills"),
}

// WorkspaceSkillsDir is the harness's own skill directory, created by
// internal/workspace beside scratch/ and scanned like a repository's
// .claude/skills. It exists so a skill can be given to a session without
// being committed to any of the repositories the session is working in: a
// skill dropped in a clone shows up in that repository's diff and in its pull
// request, which is a change nobody asked for, and one the model then has to
// remember not to commit.
//
// Being under the workspace root rather than somewhere in the image is what
// makes the skill readable. The catalogue only carries each skill's
// description; the model opens the SKILL.md itself with Read, and Read is
// confined to the workspace (internal/tools/workspace.go), so a skill outside
// it would be advertised and then refuse to open.
const WorkspaceSkillsDir = "skills"

// Limits on what reaches the model. The catalogue rides in every request of
// the run, so a repository with a hundred verbose skills would otherwise
// spend real tokens per sub-turn on descriptions of work it will never do.
const (
	MaxSkills         = 50
	MaxDescriptionLen = 500
	// MaxSkillFileBytes bounds a single SKILL.md read during discovery. Only
	// the frontmatter is needed, and a file larger than this is not one.
	MaxSkillFileBytes = 256 * 1024
)

// Skill is one discovered SKILL.md.
type Skill struct {
	// Name identifies the skill. It comes from the frontmatter when present
	// and from the containing directory otherwise.
	Name string
	// Description is the frontmatter description, truncated to
	// MaxDescriptionLen. A skill without one is skipped: the description is
	// the only thing the model sees before deciding to open the skill.
	Description string
	// Path is the SKILL.md path relative to the workspace root, which is the
	// form the model passes back to Read.
	Path string
}

// Catalogue is the result of one discovery pass.
type Catalogue struct {
	Skills []Skill
	// Dropped counts skills found but omitted from Skills by MaxSkills.
	Dropped int
}

// frontmatter is the subset of SKILL.md's YAML header this package reads.
// Unknown keys are ignored rather than rejected; skills carry fields for
// other harnesses that mean nothing here.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Discover scans workspace for skills and returns them in a stable order.
//
// Three layouts are covered. A queue-driven run clones each repository into
// its own subdirectory of the workspace, so skills sit at
// <workspace>/<repo>/.claude/skills/<name>/SKILL.md. A CLI run points the
// workspace straight at a checkout, so they sit one level higher. And the
// harness's own skills sit at <workspace>/skills/<name>/SKILL.md, outside
// every repository (WorkspaceSkillsDir). All three are scanned; none recurses
// further.
//
// Discovery never fails a run. A workspace that cannot be read, a malformed
// SKILL.md, and a skill with no description all yield no entry and no error.
func Discover(workspace string) Catalogue {
	roots := []string{workspace}
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				roots = append(roots, filepath.Join(workspace, entry.Name()))
			}
		}
	}

	var found []Skill
	seen := make(map[string]bool)
	// The workspace's own skills/ first, so a repository shipping a skill of
	// the same name sorts after it and the harness's copy is the one a reader
	// meets first. Scanned directly rather than through skillDirs: it is the
	// skill directory itself, not a repository that contains one.
	for _, skill := range scanSkillDir(workspace, filepath.Join(workspace, WorkspaceSkillsDir)) {
		if seen[skill.Path] {
			continue
		}
		seen[skill.Path] = true
		found = append(found, skill)
	}
	for _, root := range roots {
		for _, dir := range skillDirs {
			for _, skill := range scanSkillDir(workspace, filepath.Join(root, dir)) {
				if seen[skill.Path] {
					continue
				}
				seen[skill.Path] = true
				found = append(found, skill)
			}
		}
	}

	// Sorting by path rather than by name keeps two repositories that ship a
	// skill of the same name in a fixed order, and keeps the catalogue
	// grouped by repository.
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	cat := Catalogue{Skills: found}
	if len(found) > MaxSkills {
		cat.Skills = found[:MaxSkills]
		cat.Dropped = len(found) - MaxSkills
	}
	return cat
}

// scanSkillDir reads the immediate children of dir, each of which should hold
// a SKILL.md.
func scanSkillDir(workspace, dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "SKILL.md")
		skill, ok := parseSkillFile(workspace, path, entry.Name())
		if !ok {
			continue
		}
		out = append(out, skill)
	}
	return out
}

// parseSkillFile reads one SKILL.md and reports whether it yielded a usable
// skill. dirName is the fallback identity for a file whose frontmatter names
// no skill.
func parseSkillFile(workspace, path, dirName string) (Skill, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > MaxSkillFileBytes {
		return Skill{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	block, ok := frontmatterBlock(data)
	if !ok {
		return Skill{}, false
	}
	var fm frontmatter
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return Skill{}, false
	}

	description := collapse(fm.Description)
	if description == "" {
		return Skill{}, false
	}
	if len(description) > MaxDescriptionLen {
		description = strings.TrimSpace(description[:MaxDescriptionLen]) + "…"
	}

	name := collapse(fm.Name)
	if name == "" {
		name = dirName
	}

	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return Skill{}, false
	}
	return Skill{Name: name, Description: description, Path: filepath.ToSlash(rel)}, true
}

// frontmatterBlock returns the YAML between the opening "---" line and the
// next "---" line. A file that does not open with a fence has no frontmatter.
func frontmatterBlock(data []byte) ([]byte, bool) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(text, "---\n") {
		return nil, false
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false
	}
	return []byte(rest[:end]), true
}

// collapse folds a frontmatter scalar onto one line. YAML folded and literal
// blocks are common in descriptions, and the catalogue gives each skill a
// single line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Render returns the catalogue block for the opening user message, or the
// empty string when nothing was found. An empty result leaves the opening
// message byte-identical to a run with no skills.
func (c Catalogue) Render() string {
	if len(c.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Skills available in this workspace. Each is a set of instructions for a\n")
	b.WriteString("particular kind of task, written by whoever maintains the repository it\n")
	b.WriteString("came from. When one matches what you have been asked to do, Read its file\n")
	b.WriteString("before starting and follow what it says.\n\n")
	for _, skill := range c.Skills {
		b.WriteString("- ")
		b.WriteString(skill.Name)
		b.WriteString(" (")
		b.WriteString(skill.Path)
		b.WriteString("): ")
		b.WriteString(skill.Description)
		b.WriteString("\n")
	}
	if c.Dropped > 0 {
		b.WriteString("\n")
		b.WriteString(dropNotice(c.Dropped))
		b.WriteString("\n")
	}
	return b.String()
}

func dropNotice(n int) string {
	plural := "s"
	if n == 1 {
		plural = ""
	}
	return "This workspace holds " + strconv.Itoa(n) + " further skill" + plural +
		" not listed here. Use Glob for SKILL.md if none of the above fits."
}
