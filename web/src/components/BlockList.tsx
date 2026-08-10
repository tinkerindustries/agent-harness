import type { LiveView } from "../api/fold";
import type { TranscriptItem } from "../api/groups";
import { SubTurnList } from "./blocks/SubTurnList";
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
interface Props {
  items: TranscriptItem[];
  live: LiveView;
}

export function BlockList({ items, live }: Props) {
  const empty = items.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="transcript">
      <SubTurnList items={items} />
      {live.turn && <LiveAssistantBlock turn={live.turn} />}
      {[...live.pendingTools.entries()].map(([id, pending]) => (
        <LivePendingToolBlock key={id} toolCallId={id} pending={pending} />
      ))}
      {empty && <p className="empty-row">Waiting for the run to start…</p>}
    </div>
  );
}
