import { memo, type ReactNode } from "react";
import type { Block, LiveView } from "../../api/fold";
import type { TranscriptFilter, TranscriptItem } from "../../api/groups";
import type { ToolCallPayload } from "../../api/types";
import { FrozenBlock } from "../blocks/FrozenBlock";
import { groupMatchesFilter } from "../../api/groups";
import { groupMatchesQuery } from "./turnHelpers";
import { Turn } from "./Turn";
import { LiveTurnSection } from "./LiveTurn";
import type { SteerBlock } from "./SteerMessage";

// TurnTranscript is the session pages' conversation (design/session-chat.html
// and design/session-watch.html share it): one .turn per sub-turn group, the
// top-level blocks unchanged (opening, skills, run_finished, error, steer),
// and the still-streaming tail. It replaces BlockList on the session screens
// and the perf harnesses, so the harness keeps measuring what actually ships;
// TaskChildBody keeps BlockList — a child transcript nested inside a tool
// result is a different context, with its own compact card vocabulary, and
// the design does not cover it.
export function TurnTranscript({
  items,
  live,
  filter,
  getToolCall,
  renderSteer,
  renderInstruction,
  renderRunFinished,
  textQuery,
}: {
  items: TranscriptItem[];
  live: LiveView;
  filter: TranscriptFilter;
  getToolCall: (id: string) => ToolCallPayload | undefined;
  // renderSteer replaces the steer block card with the chat page's .msg-user
  // rendering (phase 3): a sent message is a message, not a block. Absent —
  // the watch page and the perf harnesses — steer blocks keep the FrozenBlock
  // rendering. The callback must be reference-stable across live-only deltas
  // (the chat screen memoises it), or the memoised TurnList below would
  // re-render the whole conversation on every token.
  renderSteer?: (block: SteerBlock) => ReactNode;
  // renderInstruction replaces the instruction block card with the watch
  // page's .msg-user rendering (phase 5, design/session-watch.html: the
  // launching agent's instruction attributed to the launcher, "from
  // claude-code · delivered · sub-turn 1"). Absent — the chat page and the
  // perf harnesses — the block keeps its FrozenBlock rendering. Same
  // reference-stability rule as renderSteer.
  renderInstruction?: (block: Extract<Block, { type: "instruction" }>) => ReactNode;
  // renderRunFinished replaces the run_finished block card with the watch
  // page's result panel (phase 4): for a run another agent launched, the
  // result payload is what the parent gets back, rendered at the end of the
  // stream where the run ended. Absent — the chat page and the perf
  // harnesses — the block keeps its FrozenBlock rendering. Reference-stable
  // across live-only deltas, like renderSteer.
  renderRunFinished?: (block: Extract<Block, { type: "run_finished" }>) => ReactNode;
  // textQuery is the watch page's find box (design/session-watch.html): a
  // non-blank query hides every sub-turn whose visible text does not contain
  // it. Blank — the chat page and the perf harnesses — matches everything.
  textQuery?: string;
}) {
  const empty = items.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="turn-list">
      <TurnList
        items={items}
        filter={filter}
        getToolCall={getToolCall}
        renderSteer={renderSteer}
        renderInstruction={renderInstruction}
        renderRunFinished={renderRunFinished}
        textQuery={textQuery}
      />
      <LiveTurnSection turn={live.turn} pendingTools={live.pendingTools} />
      {empty && <p className="empty-row">Waiting for the run to start…</p>}
    </div>
  );
}

// TurnList is the frozen half of the conversation, memoised on the items
// array itself — the same second layer of memoisation SubTurnList had on
// top of the per-group Turn memo: a live-only delta (a reasoning/content
// delta, a tool_stdout chunk) leaves the items array reference-identical and
// the whole list bails out — the token-rate hot path (docs/DESIGN.md §5.1).
// When a block freezes, only the tail group can be new, so an append costs a
// walk over items plus a render of the tail turn, not a re-render of the
// transcript (§5.2's freeze, at group granularity). textQuery is a second
// memo dimension: typing in the find box is a deliberate re-render, exactly
// like toggling a chip.
export const TurnList = memo(function TurnList({
  items,
  filter,
  getToolCall,
  renderSteer,
  renderInstruction,
  renderRunFinished,
  textQuery,
}: {
  items: TranscriptItem[];
  filter: TranscriptFilter;
  getToolCall: (id: string) => ToolCallPayload | undefined;
  renderSteer?: (block: SteerBlock) => ReactNode;
  renderInstruction?: (block: Extract<Block, { type: "instruction" }>) => ReactNode;
  renderRunFinished?: (block: Extract<Block, { type: "run_finished" }>) => ReactNode;
  textQuery?: string;
}) {
  return (
    <>
      {items.map((item) =>
        item.kind === "group" ? (
          groupMatchesFilter(item.group, filter) && groupMatchesQuery(item.group, textQuery ?? "", getToolCall) ? (
            <Turn key={item.group.seq} group={item.group} getToolCall={getToolCall} />
          ) : null
        ) : item.block.type === "steer" && renderSteer ? (
          // The chat page's sent-message rendering; the watch page and the
          // perf harnesses keep the block below.
          renderSteer(item.block)
        ) : item.block.type === "instruction" && renderInstruction ? (
          // The watch page's launcher-instruction rendering (phase 5); the
          // chat page and the perf harnesses keep the block below.
          renderInstruction(item.block)
        ) : item.block.type === "run_finished" && renderRunFinished ? (
          // The watch page's result panel; the chat page and the perf
          // harnesses keep the block below.
          renderRunFinished(item.block)
        ) : (
          // Top-level blocks are outside the chips' subject matter: the
          // filters choose which turns to show, and opening / skills /
          // run_finished / error / steer / instruction stay no matter what.
          <FrozenBlock key={`${item.block.seq}-${item.block.type}`} block={item.block} />
        ),
      )}
    </>
  );
});
