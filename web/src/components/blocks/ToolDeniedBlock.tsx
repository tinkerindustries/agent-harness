import { memo } from "react";
import type { Block } from "../../api/fold";
import { deniedBody } from "../turns/turnHelpers";
import { toolDetail } from "./toolArgs";

// A denied tool call renders as its own block, showing the call and the
// policy that refused it — the main thing an operator wants to find after a
// queue-driven run does less than expected (docs/DESIGN.md §5.8). Denials
// have their own block kind; this is the per-tool detail on top, matching
// ToolResultBlock's labelling.
export const ToolDeniedBlock = memo(function ToolDeniedBlock({ block }: { block: Extract<Block, { type: "tool_denied" }> }) {
  const detail = toolDetail(block.call);
  const body = deniedBody(block.rule, block.content);
  return (
    <section className="block block-denied">
      <div className="block-label">
        denied: {block.name}
        {detail && <code className="tool-detail"> {detail}</code>}
      </div>
      {body.rule !== null && (
        <p className="block-text">
          rule: <code>{body.rule}</code>
        </p>
      )}
      {body.content && <p className="block-text">{body.content}</p>}
    </section>
  );
});
