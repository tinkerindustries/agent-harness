import { memo, useEffect, useMemo, useRef, useState } from "react";
import type { SubTurnGroup, TranscriptFilter, TranscriptItem } from "../api/groups";
import type { RailPhaseRef } from "../api/groups";
import type { ToolCallPayload } from "../api/types";
import { toolGlyph, type ToolGlyph } from "./blocks/toolArgs";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "./ui/accordion";

// TimelineRail is the phase 6 browsing affordance (docs/WEB-REDESIGN.md,
// design/transcript.html's left column): every sub-turn as one entry, grouped
// under the plan item that was in_progress when it ran. Each entry shows the
// sub-turn number and one glyph per tool call, coloured by family, a failed
// result overriding to red (design/components.html "Tool glyphs"). Clicking
// an entry scrolls to its card; an IntersectionObserver marks the current
// entry.
//
// Two constraints from the plan matter here:
//
// 1. The observer is ONE instance over the group containers, not one per
//    block — 142 observers on a screen that is already frame-budget
//    sensitive is the failure this phase can introduce. The rail creates a
//    single IntersectionObserver on mount, observes every .subturn card
//    element, and maps an intersecting card back to its entry by the card's
//    data-seq (the group's stable id, added by SubTurnCard). The rail is
//    memoised on items/getToolCall/filter, so a live-only delta — the
//    token-rate hot path — bails out entirely, and only a frozen block or a
//    marker move re-renders it (the entries themselves are memoised, so a
//    marker move re-renders two of them).
// 2. The rail is plain sticky CSS, not ScrollArea: the column sticks below
//    the toolbar and scrolls its own content with overflow — a custom scroll
//    container would cost a wrapper and a listener per column (phase 1's
//    deliberate omission).

export interface RailEntry {
  // The group's stable id — the card's data-seq, what the observer matches
  // entries to.
  seq: number;
  subTurn: number;
  glyphs: ToolGlyph[];
}

export interface RailPhase extends RailPhaseRef {
  firstSubTurn: number;
  lastSubTurn: number;
  entries: RailEntry[];
}

// buildRail is the rail's view over the grouped transcript: consecutive
// groups under the same phase ref become one phase whose entries carry the
// sub-turn numbers and glyphs. Pure and O(n) over items, called from the
// rail's memo — the same cost profile as rendering the entries themselves.
export function buildRail(
  items: TranscriptItem[],
  getToolCall: (id: string) => ToolCallPayload | undefined,
): RailPhase[] {
  const phases: RailPhase[] = [];
  let current: RailPhase | null = null;
  for (const item of items) {
    if (item.kind !== "group") continue;
    const group = item.group;
    if (!current || current.id !== group.phase.id) {
      current = { ...group.phase, firstSubTurn: group.subTurn, lastSubTurn: group.subTurn, entries: [] };
      phases.push(current);
    } else {
      current.lastSubTurn = group.subTurn;
    }
    current.entries.push({ seq: group.seq, subTurn: group.subTurn, glyphs: glyphsFor(group, getToolCall) });
  }
  return phases;
}

// glyphsFor is one sub-turn's glyph row: one glyph per tool call in the
// order the model made them, read from the call the fold keeps (getToolCall
// — never re-parsed here), with a failed result (is_error) or a denial
// overriding that call's glyph to the red "!" (design/components.html).
function glyphsFor(group: SubTurnGroup, getToolCall: (id: string) => ToolCallPayload | undefined): ToolGlyph[] {
  const assistant = group.blocks[0];
  if (assistant.type !== "assistant") return [];
  const failed = new Set<string>();
  for (const block of group.blocks.slice(1)) {
    if (block.type === "tool_result" && block.is_error) failed.add(block.tool_call_id);
    else if (block.type === "tool_denied") failed.add(block.tool_call_id);
  }
  return assistant.toolCalls.map((call) => {
    if (failed.has(call.id)) return { letter: "!", family: "err" };
    return toolGlyph((getToolCall(call.id) ?? call).name);
  });
}

interface Props {
  items: TranscriptItem[];
  getToolCall: (id: string) => ToolCallPayload | undefined;
  // The observer must re-scan the group containers whenever the mounted set
  // of cards changes: a frozen block appends a card (items), and a filter
  // chip hides or restores cards without touching items. filter is that
  // second dependency; the rail's own rendering ignores it — the rail lists
  // every sub-turn, filters only choose which cards exist to watch and
  // scroll to.
  filter: TranscriptFilter;
  // The transcript column whose .subturn cards the observer watches.
  containerRef: React.RefObject<HTMLDivElement | null>;
}

export const TimelineRail = memo(function TimelineRail({ items, getToolCall, filter, containerRef }: Props) {
  const [currentSeq, setCurrentSeq] = useState<number | null>(null);
  const currentSeqRef = useRef<number | null>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);

  const phases = useMemo(() => buildRail(items, getToolCall), [items, getToolCall]);

  // ONE IntersectionObserver for the whole rail, created once per mount and
  // disconnected on unmount — the "not one per block" constraint
  // (docs/WEB-REDESIGN.md phase 6). The current sub-turn is the card
  // straddling the top strip of the viewport: a negative bottom rootMargin
  // shrinks the observation root to the top 20% of the viewport, so a card
  // is intersecting exactly while it is near the top, and the topmost such
  // card is the one the rail marks. An element taller than the strip stays
  // intersecting while any part of it is in the strip, which is the right
  // scrollspy feel for a card you are reading. Sticky at the bottom is free:
  // the last card is the only one left in the strip.
  useEffect(() => {
    const observer = new IntersectionObserver(
      (entries) => {
        let best: Element | null = null;
        let bestTop = Infinity;
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          if (entry.boundingClientRect.top < bestTop) {
            bestTop = entry.boundingClientRect.top;
            best = entry.target;
          }
        }
        if (!best) return;
        const seq = Number((best as HTMLElement).dataset.seq);
        if (Number.isFinite(seq) && seq !== currentSeqRef.current) {
          currentSeqRef.current = seq;
          setCurrentSeq(seq);
        }
      },
      { root: null, rootMargin: "0px 0px -80% 0px", threshold: 0 },
    );
    observerRef.current = observer;
    return () => {
      observer.disconnect();
      observerRef.current = null;
    };
  }, []);

  // Re-scan the mounted turns whenever the set can have changed. observe()
  // on an already-observed target is a no-op, so an append costs a
  // querySelectorAll plus no-op calls — nothing per frame. A turn that left
  // the DOM (filtered out) can no longer be current; clear the marker so
  // the next intersecting turn owns it.
  useEffect(() => {
    const container = containerRef.current;
    const observer = observerRef.current;
    if (!container || !observer) return;
    for (const el of container.querySelectorAll<HTMLElement>("[data-seq]")) observer.observe(el);
    if (currentSeqRef.current !== null && !container.querySelector(`[data-seq="${currentSeqRef.current}"]`)) {
      currentSeqRef.current = null;
      setCurrentSeq(null);
    }
  }, [items, filter, containerRef]);

  const totalTurns = phases.reduce((n, p) => n + p.entries.length, 0);

  return (
    <nav className="timeline-rail" aria-label="Timeline" data-current-seq={currentSeq ?? ""}>
      <div className="rail-head">
        <span>Timeline</span>
        <span className="rail-count">{totalTurns} turns</span>
      </div>
      <Accordion
        type="multiple"
        // The first phase starts open so a fresh page shows what an entry
        // looks like; the rest open on demand. Several phases can be open at
        // once — browsing a session means cross-phase jumps.
        defaultValue={phases.length > 0 ? [String(phases[0].id)] : []}
      >
        {phases.map((phase) => (
          <AccordionItem key={phase.id} value={String(phase.id)} className="rail-phase">
            <AccordionTrigger className="rail-phase-trigger">
              <span className="caret rail-caret" aria-hidden>
                ▸
              </span>
              <span className="phase-name">{phase.label ? `${phase.index} · ${phase.label}` : phase.label}</span>
              <span className="phase-range">
                {phase.firstSubTurn}–{phase.lastSubTurn}
              </span>
            </AccordionTrigger>
            <AccordionContent className="rail-phase-content">
              {phase.entries.map((entry) => (
                <RailTurn key={entry.seq} entry={entry} current={currentSeq === entry.seq} />
              ))}
            </AccordionContent>
          </AccordionItem>
        ))}
      </Accordion>
    </nav>
  );
});

// RailTurn is one sub-turn's rail entry: the number and the glyph row, as a
// plain anchor to the card (scroll-margin-top on .subturn keeps the target
// clear of the sticky toolbar). Memoised so a marker move — the scroll-hot
// path — re-renders at most two entries, not the whole rail.
const RailTurn = memo(function RailTurn({ entry, current }: { entry: RailEntry; current: boolean }) {
  return (
    <a className="rail-turn" href={`#sub-turn-${entry.subTurn}`} aria-current={current ? "true" : undefined}>
      <span className="n">{entry.subTurn}</span>
      <span className="glyphs">
        {entry.glyphs.map((glyph, i) => (
          <span key={i} className={glyphClass(glyph)}>
            {glyph.letter}
          </span>
        ))}
      </span>
    </a>
  );
});

function glyphClass(glyph: ToolGlyph): string {
  switch (glyph.family) {
    case "write":
      return "g g-write";
    case "shell":
      return "g g-bash";
    case "err":
      return "g g-err";
    default:
      return "g";
  }
}
