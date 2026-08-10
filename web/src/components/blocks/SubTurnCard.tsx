import { memo } from "react";
import type { SubTurnGroup, UsageBlock } from "../../api/groups";
import { Card } from "../ui/card";
import { AssistantBody } from "./AssistantBlock";
import { FrozenBlock } from "./FrozenBlock";
import { formatElapsed } from "./ReasoningPanel";

// SubTurnCard renders one sub-turn as a single card (design/transcript.html):
// reasoning, assistant text, tool calls and their results in one body, with
// the sub-turn's usage figures in the header instead of a sibling usage
// block. It is memoised on the group object, which SubTurnGroupState only
// replaces when the group's own children or usage change — so every group
// but the tail one renders once and never again (docs/DESIGN.md §5.2's
// freeze, at group granularity). The inner blocks render exactly as they do
// outside a group, via the same FrozenBlock dispatch; only their container
// and the header are new.
export const SubTurnCard = memo(function SubTurnCard({ group }: { group: SubTurnGroup }) {
  const assistant = group.blocks[0];
  return (
    <Card className="subturn gap-0 py-0 shadow-none overflow-hidden rounded-lg">
      <div className="subturn-header">
        <span className="subturn-n">SUB-TURN {group.subTurn}</span>
        <span className="subturn-sum">{turnSummary(group)}</span>
        {group.usage && (
          <UsageHeader usage={group.usage} elapsedMs={assistant.type === "assistant" ? assistant.reasoningElapsedMs : undefined} />
        )}
      </div>
      <div className="subturn-body">
        {group.blocks.map((block) =>
          block.type === "assistant" ? (
            // The assistant renders bare — no boxed wrapper, no duplicate
            // sub-turn label — since the card header already carries both
            // (design/transcript.html). Tool results and denials keep their
            // normal boxed rendering.
            <AssistantBody key={block.seq} block={block} />
          ) : (
            <FrozenBlock key={`${block.seq}-${block.type}`} block={block} />
          ),
        )}
      </div>
    </Card>
  );
});

// turnSummary is the card header's one-line description: the first non-empty
// line of the assistant's text, or of its reasoning when it produced no
// text. Truncated by CSS in the header row.
function turnSummary(group: SubTurnGroup): string {
  const first = group.blocks[0];
  if (first.type !== "assistant") return "";
  const source = first.content || first.reasoning;
  const line = source.split("\n").find((l) => l.trim().length > 0);
  return line ? line.trim() : "";
}

// UsageHeader is the usage block absorbed into the card header: cache-hit
// percentage, completion tokens, cost, and the sub-turn's wall time, in the
// order the design mockup's .turn-cost row shows them. Every figure comes
// from data the fold already carries — nothing here parses or re-derives.
function UsageHeader({ usage, elapsedMs }: { usage: UsageBlock; elapsedMs?: number }) {
  const hit = usage.prompt_cache_hit_tokens + usage.prompt_cache_miss_tokens;
  return (
    <span className="subturn-cost">
      {hit > 0 && (
        <span title={`prompt ${usage.prompt_tokens} · hit ${usage.prompt_cache_hit_tokens} · miss ${usage.prompt_cache_miss_tokens}`}>
          {((usage.prompt_cache_hit_tokens / hit) * 100).toFixed(1)}% hit
        </span>
      )}
      <span>{usage.completion_tokens} out</span>
      <span>${formatCost(usage.cost_usd)}</span>
      {elapsedMs !== undefined && <span>{formatElapsed(elapsedMs)}</span>}
      {usage.churn_point_index !== undefined && (
        <span className="churn-warning" title="cache prefix churn — see docs/CACHE.md">
          churn @{usage.churn_point_index}
        </span>
      )}
    </span>
  );
}

// formatCost trims a fixed-six-decimal cost to its significant digits, so a
// per-sub-turn figure reads $0.00035 rather than $0.000350.
function formatCost(cost: number): string {
  if (!Number.isFinite(cost) || cost <= 0) return "0";
  return cost.toFixed(6).replace(/\.?0+$/, "");
}
