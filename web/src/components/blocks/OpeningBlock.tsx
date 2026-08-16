import { memo, useMemo, useState } from "react";
import type { Block } from "../../api/fold";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "../ui/collapsible";
import { ScreenshotGallery } from "./ScreenshotGallery";
import { shortenWorkspacePaths } from "../ui/workspacePath";
import { BLOCK_TEXT_CLS } from "./blockStyles";

// OpeningBlock renders the run's opening message collapsed to one summary
// line — the workspace line, the word count, and the files it names
// (.fold card). A task that runs about 3,000 words before its first sub-turn
// pushes the transcript's start off the first screen, so the full text stays
// one disclosure away.
//
// .block-opening replaced every property the shared .block/.block-label
// rules set (blockStyles.ts's own comment), so this builds its own frame
// from scratch rather than starting from BLOCK_CLS/BLOCK_LABEL_CLS.
// "block"/"block-opening" survive as literal residual tokens only for
// .turn-list > .block-opening's margin override below (styles.css) — the
// same ancestor-scoped spacing MiscBlocks.tsx's residuals exist for.
export const OpeningBlock = memo(function OpeningBlock({ block }: { block: Extract<Block, { type: "opening" }> }) {
  const [open, setOpen] = useState(false);
  const summary = useMemo(() => openingSummary(block.text), [block.text]);
  return (
    <section className="block block-opening flex items-baseline gap-2 rounded-[calc(var(--radius)-4px)] pt-1 pb-3 px-1.5 mb-2 border-b border-border hover:bg-muted">
      <div className="flex-none w-12 text-[0.625rem] text-muted-foreground uppercase tracking-[var(--label-tracking)]">task</div>
      <Collapsible open={open} onOpenChange={setOpen} className="min-w-0 flex-1">
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="group flex w-full cursor-pointer items-center gap-1.5 border-0 bg-transparent py-0.5 text-left font-[inherit] text-inherit hover:text-foreground max-phone:min-h-11 max-phone:py-1.5"
            title={open ? "collapse the task" : "expand the task"}
          >
            <span
              className="text-muted-foreground transition-transform [transition-duration:var(--dur-caret)] motion-reduce:transition-none group-data-[state=open]:rotate-90"
              aria-hidden
            >
              ▸
            </span>
            <span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap text-sm text-muted-foreground">{summary}</span>
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <p className={BLOCK_TEXT_CLS}>{block.text}</p>
          {/* The task's image attachments, rendered through the same gallery
              the Screenshot and vision-tool results use — the paths the
              payload carried, addressed on GET /api/sessions/{id}/screenshot. */}
          <ScreenshotGallery paths={block.attachments} />
        </CollapsibleContent>
      </Collapsible>
    </section>
  );
});

// openingSummary is the collapsed task line: the first non-empty line (the
// workspace, truncated by CSS), the word count, and up to three of the files
// the message names. The file list is fuzzy on purpose — the opening message
// is prose, and the summary's job is to say what the task touches, not to be
// exhaustive.
export function openingSummary(text: string): string {
  const first = text.split("\n").find((l) => l.trim().length > 0) ?? "";
  const words = text.trim().split(/\s+/).filter(Boolean).length;
  const files = fileNames(text);
  // The first line is the workspace line, whose root is the same on every run
  // and long enough to push the word count off the end. The disclosure below
  // still carries the opening message verbatim.
  const parts = [shortenWorkspacePaths(first.trim()), `${words.toLocaleString("en-US")} words`];
  if (files.length > 0) parts.push(files.slice(0, 3).join(", "));
  return parts.join(" · ");
}

// fileNames collects the file references an opening message names: code spans
// that look like paths or filenames, plus bare path-like tokens ending in a
// common source extension. Deduplicated, first-seen order.
function fileNames(text: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const push = (token: string) => {
    const t = token.trim();
    if (t === "" || t.length > 120 || seen.has(t)) return;
    seen.add(t);
    out.push(t);
  };

  for (const m of text.matchAll(/`([^`]+)`/g)) {
    const t = m[1].trim();
    if (t.includes("/") || /\.[a-zA-Z0-9]{1,8}$/.test(t)) push(t);
  }
  const extension = /(?:^|[\s([,])([A-Za-z0-9_.\-/]+\.(?:md|go|ts|tsx|js|jsx|json|css|html|yml|yaml|toml|sh|py|mod|sum|txt))\b/g;
  for (const m of text.matchAll(extension)) push(m[1]);
  return out;
}
