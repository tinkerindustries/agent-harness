import type { Block, LiveView } from "../api/fold";
import { FrozenBlocks } from "./blocks/FrozenBlocks";
import { LiveAssistantBlock, LivePendingToolBlock } from "./blocks/LiveBlocks";

// BlockList is the whole transcript body: the frozen blocks (delegated to
// FrozenBlocks, memoised on the array itself — see that file for why that
// second layer of memoisation, not just per-block, is what actually keeps
// completed blocks inert) followed by whatever is still live — the
// in-progress sub-turn and any tool calls awaiting a result. BlockList
// itself is not memoised: it re-runs on every live update, but all that
// costs is choosing which of two cheap JSX branches to return, since the
// expensive part is isolated inside FrozenBlocks. Reused both at the top
// level (TranscriptScreen) and recursively inside a Task call's collapsed
// child transcript (blocks/TaskChildBody), which is why it takes plain data
// rather than reading a store itself.
interface Props {
  blocks: Block[];
  live: LiveView;
}

export function BlockList({ blocks, live }: Props) {
  const empty = blocks.length === 0 && !live.turn && live.pendingTools.size === 0;
  return (
    <div className="transcript">
      <FrozenBlocks blocks={blocks} />
      {live.turn && <LiveAssistantBlock turn={live.turn} />}
      {[...live.pendingTools.entries()].map(([id, pending]) => (
        <LivePendingToolBlock key={id} toolCallId={id} pending={pending} />
      ))}
      {empty && <p className="empty-row">Waiting for the run to start…</p>}
    </div>
  );
}
