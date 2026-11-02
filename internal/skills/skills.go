// Package skills discovers Agent Skills shipped inside the repositories a
// run cloned, installed for the user's own Claude Code, or carried by a
// Claude Code plugin, and renders a catalogue of them for the opening user
// message.
//
// A skill is a directory holding a SKILL.md whose YAML frontmatter carries a
// name and a description. Only that one-line description reaches the model up
// front; the body is read on demand with the Read tool, which is why the
// catalogue names each skill's path. Nothing here touches the system prompt
// or the tool array, so adding skills to a repository does not disturb the
// cached head (docs/CACHE.md).
package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// skillDirs are the conventions scanned inside each project root.
// .claude/skills is what Claude Code reads; .deepcode/skills is where
// DeepSeek's own Deep Code agent looks for a project's skills, and
// .agents/skills is the vendor-neutral spelling two of this harness's three
// providers have converged on — Deep Code reads it as the user-level
// location (third_party/deepseek-docs/quick_start/agent_integrations/deepcode.md)
// and Gemini's Interactions API mounts skills there in its own hosted
// environment (third_party/gemini-docs/api/interactions-api.md, Source.target).
//
// Scanning all three costs one failed ReadDir per repository per convention
// and means a repository that adopted any of the spellings is not silently
// ignored. None of this reaches a provider's own skills mechanism, because
// none of the three has one on the API surface this harness speaks to: the
// catalogue is rendered into the opening user message either way (Render).
var skillDirs = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".deepcode", "skills"),
	filepath.Join(".agents", "skills"),
}

// WorkspaceSkillsDir is the harness's own skill directory, created by
// the workspace root beside scratch/ and scanned like a repository's
// .claude/skills. It exists so a skill can be given to a session without
// being committed to any of the repositories the session is working in: a
// skill dropped in a clone shows up in that repository's diff and in its pull
// request, which is a change nobody asked for, and one the model then has to
// remember not to commit.
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

// Ranks order the catalogue when more skills were found than MaxSkills
// allows. Everything inside the workspace comes before the user's own
// installation, so a run working in a repository sees that repository's
// skills rather than losing them to a long personal collection.
const (
	rankEnterprise = iota
	rankProject
	rankWorkspace
	rankUser
	rankPlugin
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
	// Path is the path the model passes back to Read. A skill inside the
	// workspace is named relative to the workspace root; one installed
	// outside it — the user's own, a plugin's, or an ancestor repository's —
	// is named absolutely, and ReadRoots carries the directory that makes it
	// readable.
	Path string
}

// Catalogue is the result of one discovery pass.
type Catalogue struct {
	Skills []Skill
	// Dropped counts skills found but omitted from Skills by MaxSkills.
	Dropped int
}

// ReadRoots returns the directories outside the workspace that hold a
// discovered skill, which is what the read-only tools need in order to open
// it. Empty when every skill sits inside the workspace.
func (c Catalogue) ReadRoots() []string {
	seen := make(map[string]bool)
	var roots []string
	for _, skill := range c.Skills {
		if !filepath.IsAbs(skill.Path) {
			continue
		}
		dir := filepath.Dir(skill.Path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		roots = append(roots, dir)
	}
	sort.Strings(roots)
	return roots
}

// frontmatter is the subset of SKILL.md's YAML header this package reads.
// Unknown keys are ignored rather than rejected; skills carry fields for
// other harnesses that mean nothing here.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// locations are the user-specific roots a discovery pass reads. Tests pass
// their own so a developer's installed skills never reach a test.
type locations struct {
	configDir  string
	enterprise string
}

// userLocations reads where Claude Code keeps its own skills: CLAUDE_CONFIG_DIR
// when set, ~/.claude otherwise, and the per-OS managed settings directory.
func userLocations() locations {
	home, _ := os.UserHomeDir()
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" && home != "" {
		config = filepath.Join(home, ".claude")
	}
	return locations{
		configDir:  config,
		enterprise: enterpriseSkillsDir(),
	}
}

// enterpriseSkillsDir is <managed settings directory>/.claude/skills, the
// enterprise location Claude Code documents. Only the Linux path is quoted
// in its documentation; the other two follow the same managed settings
// directories Claude Code uses for its policy files.
func enterpriseSkillsDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join("/Library/Application Support/ClaudeCode", ".claude", "skills")
	case "windows":
		return filepath.Join(`C:\ProgramData\ClaudeCode`, ".claude", "skills")
	default:
		return filepath.Join("/etc/claude-code", ".claude", "skills")
	}
}

// Discover scans workspace and the Claude Code skill locations for skills and
// returns them in a stable order.
//
// Four layouts are covered. A hosted run clones each repository into its own
// subdirectory of the workspace, so project skills sit at
// <workspace>/<repo>/.claude/skills/<name>/SKILL.md. A CLI run points the
// workspace straight at a checkout, so they sit one level higher, and a run
// started in a subdirectory of a repository also sees the repository root's
// .claude/skills. The harness's own skills sit at
// <workspace>/skills/<name>/SKILL.md, outside every repository
// (WorkspaceSkillsDir). And Claude Code's own locations are read the way
// Claude Code reads them: the user's ~/.claude/skills (or
// $CLAUDE_CONFIG_DIR/skills), the skills synced there from a claude.ai
// account, every installed plugin's skills/, and the enterprise managed
// settings directory.
//
// Discovery never fails a run. A workspace that cannot be read, a malformed
// SKILL.md, and a skill with no description all yield no entry and no error.
func Discover(workspace string) Catalogue {
	return discover(workspace, userLocations())
}

func discover(workspace string, loc locations) Catalogue {
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return Catalogue{}
	}
	if resolved, err := filepath.EvalSymlinks(ws); err == nil {
		ws = resolved
	}

	type ranked struct {
		Skill
		rank int
	}
	var found []ranked
	seen := make(map[string]bool)
	add := func(dir string, rank int) {
		if dir == "" {
			return
		}
		for _, candidate := range scanSkillDir(ws, dir) {
			if seen[candidate.resolved] {
				continue
			}
			seen[candidate.resolved] = true
			found = append(found, ranked{Skill: candidate.skill, rank: rank})
		}
	}

	// Enterprise first, then the workspace's own project roots, then the
	// user's installation, then plugins. The order matters only when more
	// skills were found than the catalogue can carry.
	add(loc.enterprise, rankEnterprise)
	for _, root := range projectRoots(ws) {
		for _, dir := range skillDirs {
			add(filepath.Join(root, dir), rankProject)
		}
	}
	add(filepath.Join(ws, WorkspaceSkillsDir), rankWorkspace)
	add(filepath.Join(loc.configDir, "skills"), rankUser)
	for _, dir := range syncedSkillDirs(loc.configDir) {
		add(dir, rankUser)
	}
	for _, dir := range pluginSkillDirs(loc.configDir) {
		add(dir, rankPlugin)
	}

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].rank != found[j].rank {
			return found[i].rank < found[j].rank
		}
		return found[i].Path < found[j].Path
	})

	cat := Catalogue{}
	for _, skill := range found {
		cat.Skills = append(cat.Skills, skill.Skill)
	}
	if len(cat.Skills) > MaxSkills {
		cat.Skills = cat.Skills[:MaxSkills]
		cat.Dropped = len(found) - MaxSkills
	}
	return cat
}

// projectRoots returns the directories whose .claude/skills belong to this
// project: the workspace itself, each repository cloned immediately beneath
// it, and every directory between the workspace and its git repository root.
// Claude Code walks up to the repository root, so a run started in a
// subdirectory sees the skills checked in beside it.
func projectRoots(ws string) []string {
	roots := []string{ws}
	if entries, err := os.ReadDir(ws); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(ws, name)
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				roots = append(roots, path)
			}
		}
	}
	if root := gitRoot(ws); root != "" && root != ws {
		for dir := filepath.Dir(ws); ; dir = filepath.Dir(dir) {
			roots = append(roots, dir)
			if dir == root {
				break
			}
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
	}
	return roots
}

// gitRoot walks up from ws to the first directory holding a .git entry, which
// is a directory in an ordinary checkout and a file in a worktree.
func gitRoot(ws string) string {
	for dir := ws; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

// syncedSkillDirs returns the per-account directories Claude Code syncs a
// claude.ai account's skills into: <config>/skills/synced/<account>/.
func syncedSkillDirs(configDir string) []string {
	if configDir == "" {
		return nil
	}
	dirs, err := filepath.Glob(filepath.Join(configDir, "skills", "synced", "*"))
	if err != nil {
		return nil
	}
	return dirs
}

// pluginSkillDirs returns each installed plugin's skills directory, plus the
// plugin directories synced into the config dir rather than installed from a
// marketplace.
func pluginSkillDirs(configDir string) []string {
	if configDir == "" {
		return nil
	}
	var dirs []string
	if data, err := os.ReadFile(filepath.Join(configDir, "plugins", "installed_plugins.json")); err == nil {
		var installed struct {
			Plugins map[string][]struct {
				InstallPath string `json:"installPath"`
			} `json:"plugins"`
		}
		if json.Unmarshal(data, &installed) == nil {
			for _, versions := range installed.Plugins {
				for _, version := range versions {
					if version.InstallPath == "" {
						continue
					}
					dirs = append(dirs, filepath.Join(version.InstallPath, "skills"))
				}
			}
		}
	}
	if synced, err := filepath.Glob(filepath.Join(configDir, "plugins", "synced", "*", "*", "skills")); err == nil {
		dirs = append(dirs, synced...)
	}
	sort.Strings(dirs)
	return dirs
}

type scannedSkill struct {
	skill    Skill
	resolved string
}

// scanSkillDir reads the immediate children of dir, each of which should hold
// a SKILL.md. A child may be a symlink to a skill directory; Claude Code
// follows one, so this does too.
func scanSkillDir(workspace, dir string) []scannedSkill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []scannedSkill
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name(), "SKILL.md")
		skill, resolved, ok := parseSkillFile(workspace, path, entry.Name())
		if !ok {
			continue
		}
		out = append(out, scannedSkill{skill: skill, resolved: resolved})
	}
	return out
}

// parseSkillFile reads one SKILL.md and reports whether it yielded a usable
// skill. dirName is the fallback identity for a file whose frontmatter names
// no skill. The resolved path is the dedupe key: two locations pointing at
// the same skill load it once, the way Claude Code does.
func parseSkillFile(workspace, path, dirName string) (Skill, string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > MaxSkillFileBytes {
		return Skill{}, "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, "", false
	}
	block, ok := frontmatterBlock(data)
	if !ok {
		return Skill{}, "", false
	}
	var fm frontmatter
	if err := yaml.Unmarshal(block, &fm); err != nil {
		// A description written as a plain scalar often carries ": " in
		// prose ("Adopt a repository into worktree-manager: audit it"),
		// which strict YAML reads as a second mapping and rejects. Claude
		// Code loads those skills, so this reads the two fields it needs
		// from the header directly rather than losing the skill to a
		// punctuation mark.
		fm = looseFrontmatter(block)
	}

	description := collapse(fm.Description)
	if description == "" {
		return Skill{}, "", false
	}
	if len(description) > MaxDescriptionLen {
		description = strings.TrimSpace(description[:MaxDescriptionLen]) + "…"
	}

	name := collapse(fm.Name)
	if name == "" {
		name = dirName
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	rel, err := filepath.Rel(workspace, resolved)
	shown := resolved
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		shown = filepath.ToSlash(rel)
	}
	return Skill{Name: name, Description: description, Path: shown}, resolved, true
}

// looseFrontmatter reads name and description from a header strict YAML
// refused, one top-level "key: value" line at a time. It understands the two
// spellings skills use for a long description — a quoted or plain scalar, and
// the ">" or "|" block whose following indented lines belong to it — because
// a description is the only field discovery needs.
func looseFrontmatter(block []byte) frontmatter {
	var fm frontmatter
	lines := strings.Split(string(block), "\n")
	for i := 0; i < len(lines); i++ {
		key, value, ok := frontmatterLine(lines[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(value, ">") || strings.HasPrefix(value, "|") {
			var collected []string
			for i+1 < len(lines) {
				next := lines[i+1]
				if strings.TrimSpace(next) == "" {
					i++
					continue
				}
				if !strings.HasPrefix(next, " ") && !strings.HasPrefix(next, "\t") {
					break
				}
				collected = append(collected, strings.TrimSpace(next))
				i++
			}
			value = strings.Join(collected, " ")
		} else {
			value = unquoteFrontmatter(value)
		}
		switch key {
		case "name":
			fm.Name = value
		case "description":
			fm.Description = value
		}
	}
	return fm
}

// frontmatterLine splits one top-level "key: value" line. Indented lines are
// block content, and comments and blank lines carry nothing.
func frontmatterLine(line string) (string, string, bool) {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return "", "", false
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	key, value, ok := strings.Cut(trimmed, ":")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}

// unquoteFrontmatter removes one matching pair of quotes from a scalar.
// Only the escape sequences that matter inside a description are undone.
func unquoteFrontmatter(value string) string {
	if len(value) < 2 {
		return value
	}
	switch {
	case value[0] == '"' && value[len(value)-1] == '"':
		inner := value[1 : len(value)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	case value[0] == '\'' && value[len(value)-1] == '\'':
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
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
	b.WriteString("particular kind of task, written by whoever maintains it. When one matches\n")
	b.WriteString("what you have been asked to do, Read its file before starting and follow\n")
	b.WriteString("what it says.\n\n")
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
