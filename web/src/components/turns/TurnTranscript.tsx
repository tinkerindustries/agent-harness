import { memo } from "react";
import type { LiveView } from "../../api/fold";
import type { TranscriptFilter, TranscriptItem } from "../../api/groups";
import type { ToolCallPayload } from "../../api/types";
import { FrozenBlock } from "../blocks/FrozenBlock";
import { groupMatchesFilter } from "../../api/groups";
import { Turn } from "./Turn";
import { LiveTurnSection } from "./LiveTurn";

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
}: {
  items: TranscriptItem[];
  live: LiveView;
  filter: TranscriptFilter;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  const empty = items.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="turn-list">
      <TurnList items={items} filter={filter} getToolCall={getToolCall} />
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
// transcript (§5.2's freeze, at group granularity).
export const TurnList = memo(function TurnList({
  items,
  filter,
  getToolCall,
}: {
  items: TranscriptItem[];
  filter: TranscriptFilter;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  return (
    <>
      {items.map((item) =>
        item.kind === "group" ? (
          groupMatchesFilter(item.group, filter) ? (
            <Turn key={item.group.seq} group={item.group} getToolCall={getToolCall} />
          ) : null
        ) : (
          // Top-level blocks are outside the chips' subject matter: the
          // filters choose which turns to show, and opening / skills /
          // run_finished / error / steer stay no matter what. Steer keeps
          // rendering through SteerBlock until phase 3 restyles it as
          // .msg-user.
          <FrozenBlock key={`${item.block.seq}-${item.block.type}`} block={item.block} />
        ),
      )}
    </>
  );
});
