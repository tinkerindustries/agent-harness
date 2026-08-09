import { memo, useMemo } from "react";
import type { Block } from "../../api/fold";

// The catalogue's leading paragraph tells the model what skills are and how
// to open one. A reader of the transcript does not need it, so the panel
// shows the list and the summary line counts it.
function skillLines(text: string): string[] {
  return text.split("\n").filter((line) => line.startsWith("- "));
}

// SkillsBlock renders the skills catalogue the run put in front of the model
// (internal/skills). It is collapsed by default: on most runs the model never
// opens a skill, and the catalogue would otherwise push the task off the top
// of the transcript.
export const SkillsBlock = memo(function SkillsBlock({ block }: { block: Extract<Block, { type: "skills" }> }) {
  const lines = useMemo(() => skillLines(block.text), [block.text]);
  const count = lines.length;
  return (
    <section className="block block-skills">
      <div className="block-label">skills</div>
      <details>
        <summary className="skills-summary">
          {count} skill{count === 1 ? "" : "s"} offered to the model
        </summary>
        <pre className="block-pre">{block.text}</pre>
      </details>
    </section>
  );
});
