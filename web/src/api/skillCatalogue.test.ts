import { describe, expect, it } from "vitest";
import { EMPTY_SKILL_CATALOGUE, parseSkillCatalogue } from "./skillCatalogue";

// Verbatim internal/skills.Catalogue.Render output — the leading paragraph,
// the entries, and the notice a truncated catalogue ends with. The parser's
// whole job is to survive that shape, so the fixture is the shape rather than
// the entry lines alone.
const RENDERED = `Skills available in this workspace. Each is a set of instructions for a
particular kind of task, written by whoever maintains the repository it
came from. When one matches what you have been asked to do, Read its file
before starting and follow what it says.

- test-runner (repo/.claude/skills/test-runner/SKILL.md): Run the suite.
- workspace-conventions (skills/workspace-conventions/SKILL.md): How this workspace is laid out.

This workspace holds 3 further skills not listed here. Use Glob for SKILL.md if none of the above fits.
`;

describe("parseSkillCatalogue", () => {
  it("maps each entry's path to its name", () => {
    const skills = parseSkillCatalogue(RENDERED);
    expect(skills.get("repo/.claude/skills/test-runner/SKILL.md")).toBe("test-runner");
    expect(skills.get("skills/workspace-conventions/SKILL.md")).toBe("workspace-conventions");
  });

  it("takes only the entry lines", () => {
    // The prose, the blank lines and the drop notice are not skills, and a
    // parser that counted them would badge Reads that open nothing.
    expect(parseSkillCatalogue(RENDERED).size).toBe(2);
  });

  it("keeps a name that contains brackets", () => {
    const skills = parseSkillCatalogue("- deploy (staging) (repo/.claude/skills/deploy/SKILL.md): Ship it.\n");
    expect(skills.get("repo/.claude/skills/deploy/SKILL.md")).toBe("deploy (staging)");
  });

  it("returns the shared empty map for text with no entries", () => {
    // Reference identity, not just emptiness: the value is a context value,
    // and a fresh map per parse would re-render every tool row on the page.
    expect(parseSkillCatalogue("")).toBe(EMPTY_SKILL_CATALOGUE);
    expect(parseSkillCatalogue("no entries here\n")).toBe(EMPTY_SKILL_CATALOGUE);
  });
});
