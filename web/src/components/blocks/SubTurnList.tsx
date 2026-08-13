import { memo } from "react";
import type { TranscriptFilter, TranscriptItem } from "../../api/groups";
import type { ToolCallPayload } from "../../api/types";
import { FrozenBlock } from "./FrozenBlock";
import { SubTurnCard } from "./SubTurnCard";
import { groupMatchesFilter } from "../../api/groups";

// SubTurnList is the display-side grouping: each sub-turn's assistant
// block, tool results, and usage render as one
// card instead of sibling blocks, and opening / skills / run_finished /
// error blocks stay top-level. It is memoised on the items array itself,
// the same second layer of memoisation FrozenBlocks had on the blocks array:
// a live-only update — a reasoning/content delta or a tool_stdout chunk —
// leaves the fold's blocks array reference-identical, so
// SubTurnGroupState.sync returns the same items array and this component
// bails out entirely (the token-rate hot path, docs/DESIGN.md §5.1). When a
// block freezes, only the tail can have changed: every earlier group keeps
// its children array reference and its SubTurnCard memo bails out on the
// group reference, so an append costs a walk over items plus a render of the
// tail group, not a re-render of the transcript (§5.2's freeze, at group
// granularity). This list serves only the child-transcript context
// (TaskChildBody) — the session screens render turns via
// components/turns/TurnTranscript — and there is no Compact/Full density
// toggle, so the cards render always full.
export const SubTurnList = memo(function SubTurnList({
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
            <SubTurnCard key={item.group.seq} group={item.group} getToolCall={getToolCall} />
          ) : null
        ) : (
          // Top-level blocks are outside the chips' subject matter: the
          // filters choose which sub-turn cards to show, and opening /
          // skills / run_finished / error stay no matter what.
          <FrozenBlock key={`${item.block.seq}-${item.block.type}`} block={item.block} />
        ),
      )}
    </>
  );
});
