import { memo, useMemo } from "react";
import type { Block } from "../../api/fold";
import { BLOCK_PRE_CLS } from "./blockStyles";

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
//
// .block-skills replaced every property the shared .block/.block-label rules
// set, same as .block-opening (OpeningBlock.tsx's own comment) — its own
// frame from scratch, not BLOCK_CLS/BLOCK_LABEL_CLS. "block"/"block-skills"
// survive as literal residual tokens only for .turn-list > .block-skills's
// margin override (styles.css). "skills-summary" survives on the <summary>
// only to scope the cross-browser <details> marker hider
// (.skills-summary::-webkit-details-marker), the same pattern as
// .think/.tool/.phase/.result-json's own disclosures — the caret's own
// rotation is the group-open: variant on the <details>, which needs a real
// literal arrow span rather than the old rule's ::before (no className
// equivalent), matching OpeningBlock's own disclosure.
export const SkillsBlock = memo(function SkillsBlock({ block }: { block: Extract<Block, { type: "skills" }> }) {
  const lines = useMemo(() => skillLines(block.text), [block.text]);
  const count = lines.length;
  return (
    <section className="block block-skills flex items-baseline gap-2 rounded-[calc(var(--radius)-4px)] py-1 px-1.5 hover:bg-muted">
      <div className="flex-none w-12 text-[0.625rem] text-muted-foreground uppercase tracking-[var(--label-tracking)]">skills</div>
      <details className="group min-w-0 flex-1">
        <summary className="skills-summary flex cursor-pointer list-none items-baseline gap-1.5 text-sm text-muted-foreground hover:text-foreground max-phone:min-h-11 max-phone:items-center">
          <span
            className="text-xs text-muted-foreground transition-transform [transition-duration:var(--dur-caret)] motion-reduce:transition-none group-open:rotate-90"
            aria-hidden
          >
            ▸
          </span>
          {count} skill{count === 1 ? "" : "s"} offered to the model
        </summary>
        <pre className={BLOCK_PRE_CLS}>{block.text}</pre>
      </details>
    </section>
  );
});
