import { memo } from "react";
import type { Block } from "../../api/fold";

export const OpeningBlock = memo(function OpeningBlock({ block }: { block: Extract<Block, { type: "opening" }> }) {
  return (
    <section className="block block-opening">
      <div className="block-label">task</div>
      <p className="block-text">{block.text}</p>
    </section>
  );
});
