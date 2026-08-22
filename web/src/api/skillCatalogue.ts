// The skills catalogue read back out of the block the fold lifted from the
// opening message. internal/skills.Catalogue.Render writes one entry a line:
//
//   - <name> (<path to SKILL.md, relative to the workspace root>): <description>
//
// The browser parses it because taking a skill is not an event. This harness
// has no Skill tool, and none of its three providers exposes a skills
// mechanism on the API surface it speaks to (internal/skills/skills.go's
// package comment), so a model that decides to follow a skill does it by
// calling Read on the path the catalogue named. Matching a Read's target
// against these paths is the only thing that separates "the model opened the
// test-runner skill" from "the model read a file" — see toolArgs.ts's
// skillOpened, which is the one consumer.
//
// Matching the catalogue rather than the SKILL.md file name is what keeps
// that honest in a repository whose subject matter is skills. This one ships
// two skill packs; a run editing assets/skill-packs/unity/... reads SKILL.md
// files all day without ever taking one as instructions, and a badge keyed on
// the name alone would call every one of those a skill load.

// A name may itself contain brackets, so the path is the last parenthesised
// group before the colon — hence the greedy name and the bracket-free path.
const ENTRY = /^- (.+) \(([^()]*)\): /;

// SkillCatalogue maps a skill's SKILL.md path to its name. Readonly because
// it is a context value shared by every tool row on the page.
export type SkillCatalogue = ReadonlyMap<string, string>;

// EMPTY_SKILL_CATALOGUE is the value for a transcript with no catalogue: a
// run whose workspace held no skills, and a context with no provider above
// it. Module-level so every such transcript shares one reference and no
// consumer re-renders on it.
export const EMPTY_SKILL_CATALOGUE: SkillCatalogue = new Map();

// parseSkillCatalogue reads the rendered catalogue. A line that is not an
// entry — the leading paragraph, the blank lines, the "further skills not
// listed here" notice — contributes nothing, so a catalogue this parser has
// fallen behind degrades to no badges rather than to wrong ones.
export function parseSkillCatalogue(text: string): SkillCatalogue {
  if (text === "") return EMPTY_SKILL_CATALOGUE;
  const skills = new Map<string, string>();
  for (const line of text.split("\n")) {
    const m = ENTRY.exec(line);
    if (m) skills.set(m[2], m[1]);
  }
  return skills.size === 0 ? EMPTY_SKILL_CATALOGUE : skills;
}
