import { memo } from "react";
import type { Block } from "../../api/fold";
import { FrozenBlock } from "./FrozenBlock";

// FrozenBlocks is memoised on the blocks array itself, not just on each
// block inside it. That second layer is what actually delivers "399 of 400
// blocks are inert" (docs/DESIGN.md §5.2): without it, a live-only update —
// which changes BlockList's `live` prop but not `blocks` — still forces
// React to re-run blocks.map and re-reconcile every list child, and that
// walk costs O(n) even when each child's own React.memo bails out
// immediately. Measurement in web/src/perf showed exactly that cost scaling
// with block count once this component existed to isolate it. FoldState's
// pushBlock keeps `blocks` a stable reference across every ingest that
// doesn't add a block, which is what lets the memo comparison below ever
// succeed.
export const FrozenBlocks = memo(function FrozenBlocks({ blocks }: { blocks: Block[] }) {
  return (
    <>
      {blocks.map((block) => (
        <FrozenBlock key={block.seq} block={block} />
      ))}
    </>
  );
});
