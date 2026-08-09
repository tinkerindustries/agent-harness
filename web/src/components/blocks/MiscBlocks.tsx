import { memo } from "react";
import type { Block } from "../../api/fold";

export const UsageBlock = memo(function UsageBlock({ block }: { block: Extract<Block, { type: "usage" }> }) {
  return (
    <div className="block block-usage">
      prompt {block.prompt_tokens} (hit {block.prompt_cache_hit_tokens} / miss {block.prompt_cache_miss_tokens}), completion{" "}
      {block.completion_tokens}, cost ${block.cost_usd.toFixed(6)}
      {block.churn_point_index !== undefined && <span className="churn-warning"> — churn at message {block.churn_point_index}</span>}
    </div>
  );
});

export const RunFinishedBlock = memo(function RunFinishedBlock({ block }: { block: Extract<Block, { type: "run_finished" }> }) {
  return (
    <section className="block block-run-finished">
      <div className="block-label">run finished: {block.reason}</div>
      {block.summary && <p className="block-text">{block.summary}</p>}
      {block.text && <p className="block-text">{block.text}</p>}
    </section>
  );
});

export const ErrorBlock = memo(function ErrorBlock({ block }: { block: Extract<Block, { type: "error" }> }) {
  return (
    <section className="block block-error">
      <div className="block-label">error</div>
      <p className="block-text">{block.message}</p>
    </section>
  );
});
