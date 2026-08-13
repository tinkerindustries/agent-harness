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

// InstructionBlock renders the launching agent's instruction as a plain
// top-level block — the default, used where no page has a message-shaped
// rendering of it (the chat page and the perf harnesses). The watch page
// replaces it with its .msg-user rendering through TurnTranscript's
// renderInstruction ("from claude-code · delivered · sub-turn 1").
export const InstructionBlock = memo(function InstructionBlock({ block }: { block: Extract<Block, { type: "instruction" }> }) {
  return (
    <section className="block block-instruction">
      <div className="block-label">instruction</div>
      <p className="block-text">{block.text}</p>
    </section>
  );
});

// SteerBlock renders an operator steer (docs/RUN-CONTROL.md "The frontend"):
// an operator instruction the loop will fold into the model's next request.
// It is deliberately styled distinctly from the model's own turns — a
// top-level block, not a sub-turn card — and the two states are the point:
// *pending* (only the steer_message event exists) is the operator's signal
// that the run is wedged or still mid-tool-call, and *delivered* (the
// matching steer_applied arrived) is the run having actually picked it up.
export const SteerBlock = memo(function SteerBlock({ block }: { block: Extract<Block, { type: "steer" }> }) {
  return (
    <section className={`block block-steer block-steer-${block.state}`}>
      <div className="block-label">
        {block.state === "pending" ? "steer · sent, not yet delivered" : "steer · delivered"}
      </div>
      <p className="block-text">{block.text}</p>
    </section>
  );
});
