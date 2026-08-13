import type { LiveView } from "../api/fold";
import type { TranscriptFilter, TranscriptItem } from "../api/groups";
import type { ToolCallPayload } from "../api/types";
import { SubTurnList } from "./blocks/SubTurnList";
import { LiveAssistantBlock, LivePendingToolBlock } from "./blocks/LiveBlocks";

// BlockList is the whole transcript body for the child-transcript context
// (blocks/TaskChildBody): the grouped blocks — each sub-turn's card plus the
// top-level blocks outside any group, delegated to SubTurnList and memoised
// on the items array itself (see that file for why that second layer of
// memoisation, not just per-block, is what actually keeps completed
// sub-turns inert) — followed by whatever is still live: the in-progress
// sub-turn and any tool calls awaiting a result. A sub-turn's card cannot
// freeze until its last tool result lands, so the tail group's children keep
// growing while its results stream in as live blocks; the moment the last
// one freezes, the group holds an unchanged array and bails out forever.
// BlockList itself is not memoised: it re-runs on every live update, but all
// that costs is choosing which of two cheap JSX branches to return, since
// the expensive part is isolated inside SubTurnList.
//
// The session screens and the perf harnesses render the turn vocabulary
// instead (components/turns/TurnTranscript); this component and its card
// vocabulary survive only for a child transcript nested inside a Task tool
// result, which that design does not cover.
//
// filter and getToolCall keep their unfiltered defaults so the recursive
// caller — which has no toolbar of its own — stays on the unfiltered
// rendering.
interface Props {
  items: TranscriptItem[];
  live: LiveView;
  filter?: TranscriptFilter;
  getToolCall?: (id: string) => ToolCallPayload | undefined;
}

// Module-level default so every render hands SubTurnList the same function
// reference: an inline `() => undefined` default would be a fresh function
// per render and defeat the list's memo on every delta for the caller
// (TaskChildBody) that has no store registry to pass.
const NOOP_GET_TOOL_CALL = (): ToolCallPayload | undefined => undefined;

export function BlockList({ items, live, filter = "all", getToolCall = NOOP_GET_TOOL_CALL }: Props) {
  const empty = items.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="transcript">
      <SubTurnList items={items} filter={filter} getToolCall={getToolCall} />
      {live.turn && <LiveAssistantBlock turn={live.turn} />}
      {[...live.pendingTools.entries()].map(([id, pending]) => (
        <LivePendingToolBlock key={id} toolCallId={id} pending={pending} />
      ))}
      {empty && <p className="empty-row">Waiting for the run to start…</p>}
    </div>
  );
}
