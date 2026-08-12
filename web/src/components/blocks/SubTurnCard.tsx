import { memo, useEffect, useState } from "react";
import type { SubTurnGroup, UsageBlock } from "../../api/groups";
import type { Block } from "../../api/fold";
import type { SessionState, ToolCallPayload } from "../../api/types";
import { Card } from "../ui/card";
import { Badge } from "../ui/badge";
import { Markdown } from "../../render/Markdown";
import { FrozenBlock } from "./FrozenBlock";
import { ReasoningPanel, formatElapsed } from "./ReasoningPanel";
import { ToolResultBody } from "./ToolResultBlock";
import { childStat, exitCode, formatCost, toolHeader } from "./toolArgs";

// SubTurnCard renders one sub-turn as a single card, always full
// (design/transcript.html): reasoning, assistant text, tool calls and their
// results in one body, with the sub-turn's usage figures in the header
// instead of a sibling usage block. The session redesign retired the
// Compact/Full toggle (design/README.md "The two session pages" — a turn is
// always full, and collapsing happens per tool row instead), so this card —
// now only used for a child transcript nested inside a Task tool result,
// where the design does not cover a turn rendering — is always expanded.
// It is memoised on the group object, which SubTurnGroupState only replaces
// when the group's own children or usage change — so every group but the
// tail one renders once and never again (docs/DESIGN.md §5.2's freeze, at
// group granularity). getToolCall is stable across a live-only delta, so the
// memo bailout survives the token-rate hot path.
export const SubTurnCard = memo(function SubTurnCard({
  group,
  getToolCall,
}: {
  group: SubTurnGroup;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  const assistant = group.blocks[0];

  return (
    <Card
      id={`sub-turn-${group.subTurn}`}
      // data-seq is the group's stable id for the phase 6 rail: the rail's
      // single IntersectionObserver watches these card elements and maps
      // them back to rail entries by this attribute (docs/WEB-REDESIGN.md
      // phase 6, "the group is what the observer watches").
      data-seq={group.seq}
      // .anim-stream-in rides the memoised card, which renders exactly once
      // when its group freezes — never the live buffer, where the tail would
      // re-animate on every rAF flush (docs/DESIGN.md §5.3, web/CLAUDE.md).
      className="subturn gap-0 py-0 shadow-none overflow-hidden rounded-lg anim-stream-in"
    >
      <div className="subturn-header">
        <span className="caret" aria-hidden>
          ▸
        </span>
        <span className="subturn-n">SUB-TURN {group.subTurn}</span>
        <span className="subturn-sum">{turnSummary(group)}</span>
        {group.usage && (
          <UsageHeader usage={group.usage} elapsedMs={assistant.type === "assistant" ? assistant.reasoningElapsedMs : undefined} />
        )}
      </div>
      <div className="subturn-body">
        <SubTurnBody group={group} getToolCall={getToolCall} />
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

// ToolResultLike is a group block that pairs with a tool call: the result or
// the denial. The fold attaches every result to the call that produced it,
// so the map below is these two kinds only.
type ToolResultLike = Extract<Block, { type: "tool_result" }> | Extract<Block, { type: "tool_denied" }>;

// SubTurnBody renders a group's card body: the assistant's reasoning and
// text, then one tool card per tool call paired with its result, then any
// block that paired with nothing (defensive — the fold attaches every result
// to the call that produced it, so this is empty in practice). A tool call
// whose result has not landed yet is deliberately absent here: the live
// pending-tool block below the cards is what shows it while it runs.
function SubTurnBody({ group, getToolCall }: { group: SubTurnGroup; getToolCall: (id: string) => ToolCallPayload | undefined }) {
  const assistant = group.blocks[0];
  if (assistant.type !== "assistant") return null;

  const resultsByCallId = new Map<string, ToolResultLike>();
  for (const block of group.blocks.slice(1)) {
    if (block.type === "tool_result" || block.type === "tool_denied") resultsByCallId.set(block.tool_call_id, block);
  }

  const paired = new Set<string>();
  const cards = assistant.toolCalls.map((call) => {
    const result = resultsByCallId.get(call.id);
    if (!result) return null;
    paired.add(call.id);
    return <ToolCallCard key={call.id} callId={call.id} fallbackCall={call} result={result} getToolCall={getToolCall} />;
  });

  const unpaired = group.blocks.slice(1).filter((block) => {
    if (block.type !== "tool_result" && block.type !== "tool_denied") return false;
    return !paired.has(block.tool_call_id);
  });

  return (
    <>
      {assistant.reasoning && (
        <ReasoningPanel text={assistant.reasoning} defaultOpen={false} elapsedMs={assistant.reasoningElapsedMs} tokens={assistant.reasoningTokens} />
      )}
      {assistant.content && <Markdown text={assistant.content} />}
      {cards}
      {unpaired.map((block) => (
        <FrozenBlock key={`${block.seq}-${block.type}`} block={block} />
      ))}
    </>
  );
}

// ToolCallCard is one tool call inside a sub-turn card (design/transcript.html's
// .tool): a header row with the tool's name, the target it acts on, and the
// trailing stat — the +n −n from the diff, the child session's figures, or an
// exit badge for a failed call — over the result body. The header is built
// from the call the fold keeps in toolCallsById, read through the store's
// exposed getToolCall (web/src/api/fold.ts); fallbackCall is the assistant's
// own copy, for a registry that predates the exposure.
function ToolCallCard({
  callId,
  fallbackCall,
  result,
  getToolCall,
}: {
  callId: string;
  fallbackCall: ToolCallPayload;
  result: ToolResultLike;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  const call = getToolCall(callId) ?? fallbackCall;
  const header = toolHeader(call, result.type === "tool_result" ? { diff: result.diff } : undefined);
  const isError = result.type === "tool_denied" || result.is_error === true;
  const childStatNode =
    result.type === "tool_result" && result.name === "Task" && result.child_session_id ? (
      <TaskChildStat sessionId={result.child_session_id} />
    ) : null;

  return (
    <div className={`tool-card${isError ? " tool-card-error" : ""}`}>
      <div className="tool-header">
        <span className="tool-name">{header.name}</span>
        {header.target && <span className="tool-target">{header.target}</span>}
        <span className="tool-stat">
          {header.stat && <span>{header.stat}</span>}
          {childStatNode}
          {isError && <Badge variant="failed">{exitCode(result.name, result.content) || "failed"}</Badge>}
        </span>
      </div>
      {result.type === "tool_result" ? (
        <ToolResultBody block={result} />
      ) : (
        <div className="tool-body">
          <p className="block-text">
            rule: <code>{result.rule}</code>
          </p>
          <p className="block-text">{result.content}</p>
        </div>
      )}
    </div>
  );
}

// TaskChildStat fetches the Task call's child session metadata and renders
// the header's "child · n sub-turns · $cost" figure. No network call is made
// for a Task call without a child session id; a failed fetch (child session
// gone, or the measurement harness with no backend) leaves the stat empty
// rather than blocking the header.
function TaskChildStat({ sessionId }: { sessionId: string }) {
  const meta = useChildSessionMeta(sessionId);
  if (!meta) return null;
  const cost = meta.usage?.cost_usd ?? 0;
  return <span>{childStat(meta.sub_turns, cost)}</span>;
}

function useChildSessionMeta(sessionId: string): SessionState | null {
  const [meta, setMeta] = useState<SessionState | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api/sessions/${encodeURIComponent(sessionId)}`, { signal: controller.signal })
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => data && setMeta(data))
      .catch(() => {
        // A transient fetch failure just leaves the stat empty.
      });
    return () => controller.abort();
  }, [sessionId]);
  return meta;
}
