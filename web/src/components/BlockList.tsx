import type { LiveView } from "../api/fold";
import type { TranscriptFilter, TranscriptItem } from "../api/groups";
import type { ToolCallPayload } from "../api/types";
import { SubTurnList } from "./blocks/SubTurnList";
import type { Density } from "./blocks/SubTurnCard";
import { LiveAssistantBlock, LivePendingToolBlock } from "./blocks/LiveBlocks";

// BlockList is the whole transcript body: the grouped blocks — each
// sub-turn's card plus the top-level blocks outside any group, delegated to
// SubTurnList and memoised on the items array itself (see that file for why
// that second layer of memoisation, not just per-block, is what actually
// keeps completed sub-turns inert) — followed by whatever is still live: the
// in-progress sub-turn and any tool calls awaiting a result. A sub-turn's
// card cannot freeze until its last tool result lands, so the tail group's
// children keep growing while its results stream in as live blocks; the
// moment the last one freezes, the group holds an unchanged array and bails
// out forever. BlockList itself is not memoised: it re-runs on every live
// update, but all that costs is choosing which of two cheap JSX branches to
// return, since the expensive part is isolated inside SubTurnList. Reused
// both at the top level (TranscriptScreen) and recursively inside a Task
// call's collapsed child transcript (blocks/TaskChildBody), which is why it
// takes plain data rather than reading a store itself.
//
// density, filter, and getToolCall are the phase 5 additions
// (docs/WEB-REDESIGN.md). Their defaults keep the recursive and measurement
// callers — which have no toolbar of their own — on the full, unfiltered
// rendering.
interface Props {
  items: TranscriptItem[];
  live: LiveView;
  density?: Density;
  filter?: TranscriptFilter;
  getToolCall?: (id: string) => ToolCallPayload | undefined;
}

// Module-level default so every render hands SubTurnList the same function
// reference: an inline `() => undefined` default would be a fresh function
// per render and defeat the list's memo on every delta for the callers
// (TaskChildBody, the perf harness) that have no store registry to pass.
const NOOP_GET_TOOL_CALL = (): ToolCallPayload | undefined => undefined;

export function BlockList({ items, live, density = "full", filter = "all", getToolCall = NOOP_GET_TOOL_CALL }: Props) {
  const empty = items.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="transcript">
      <SubTurnList items={items} density={density} filter={filter} getToolCall={getToolCall} />
      {live.turn && <LiveAssistantBlock turn={live.turn} />}
      {[...live.pendingTools.entries()].map(([id, pending]) => (
        <LivePendingToolBlock key={id} toolCallId={id} pending={pending} />
      ))}
      {empty && <p className="empty-row">Waiting for the run to start…</p>}
    </div>
  );
}
