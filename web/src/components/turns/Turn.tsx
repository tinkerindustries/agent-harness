import { memo, useMemo, useState } from "react";
import type { SubTurnGroup, UsageBlock } from "../../api/groups";
import type { ToolCallPayload } from "../../api/types";
import { Markdown } from "../../render/Markdown";
import { DiffTable } from "../blocks/DiffTable";
import { formatElapsed } from "../blocks/ReasoningPanel";
import { ScreenshotGallery } from "../blocks/ScreenshotGallery";
import { formatCost, screenshotPaths, toolDetail } from "../blocks/toolArgs";
import { cachePercent, deniedBody, elideLines, toolStat, type ToolResultLike } from "./turnHelpers";
import { cn } from "@/lib/utils";

// Turn renders one frozen sub-turn as a turn (.turn), not a card: the
// gutter anchor with the sub-turn number, the hover
// telemetry line, a think disclosure, the assistant's prose, and one tool
// row per call. It is memoised on the group object exactly the way SubTurnCard
// was — SubTurnGroupState only replaces a group when its own blocks or usage
// change, so every group but the tail one holds a reference-stable group
// (and a reference-stable blocks array) forever and renders once, never
// again (docs/DESIGN.md §5.2's freeze, at group granularity). getToolCall is
// the fold's stable read-only arrow, so the bailout survives the token-rate
// hot path.
export const Turn = memo(function Turn({
  group,
  getToolCall,
  arrived = false,
}: {
  group: SubTurnGroup;
  getToolCall: (id: string) => ToolCallPayload | undefined;
  // Whether this sub-turn appeared in a transcript that was already on screen,
  // rather than being part of the history the page loaded with (hooks.ts
  // useArrivals). A boolean, so the memo above still bails out on it; it is
  // false for every turn in the backlog and never changes for a given turn.
  arrived?: boolean;
}) {
  const assistant = group.blocks[0];
  // The fold always opens a group with its assistant block; the guard is
  // defensive (SubTurnBody carries the same one).
  if (assistant.type !== "assistant") return null;

  return (
    <div
      className={cn("turn", arrived && "anim-row-in")}
      id={`sub-turn-${group.subTurn}`}
      data-seq={group.seq}
    >
      <a className="gutter" href={`#sub-turn-${group.subTurn}`} title={`Sub-turn ${group.subTurn}`}>
        {group.subTurn}
        {group.usage && <ChurnWarn usage={group.usage} />}
      </a>
      {group.usage && <TurnMeta usage={group.usage} elapsedMs={assistant.reasoningElapsedMs} />}
      {assistant.reasoning && (
        <details className="think">
          <summary>
            <span className="caret" aria-hidden>
              ▸
            </span>
            Thought{assistant.reasoningElapsedMs !== undefined ? ` for ${formatElapsed(assistant.reasoningElapsedMs)}` : ""} ·{" "}
            {assistant.reasoningTokens !== undefined
              ? assistant.reasoningTokens
              : `~${Math.max(1, Math.round(assistant.reasoning.length / 4))}`}{" "}
            tokens
          </summary>
          <div className="thought">{assistant.reasoning}</div>
        </details>
      )}
      {assistant.content && (
        // .anim-stream-in goes here — on the committed block, not the live
        // buffer. Turn is memoised on the group and renders exactly once, the
        // moment the sub-turn freezes, so the prose settles in as it lands. On
        // the live element the tail would re-animate on every rAF flush
        // (docs/DESIGN.md §5.3, web/CLAUDE.md).
        //
        // One gesture per thing that arrives: a turn that came in while the
        // page was open animates as a row and its prose does not animate
        // separately. They are not additive — row-in brings the parent down
        // from -8px while stream-in takes the child up from +8px, so run
        // together they cancel at the start and the text sits still through
        // the part of the curve where all the movement is. Backlog prose,
        // which no row-in touches, keeps the reveal.
        <div className={cn("say", !arrived && "anim-stream-in")}>
          {/* Prose parses once, on completion, memoised inside Markdown on
              the block's own immutable text (docs/DESIGN.md §5.3). */}
          <Markdown text={assistant.content} />
        </div>
      )}
      <ToolRows group={group} getToolCall={getToolCall} />
    </div>
  );
});

// churnExcess is the "tokens re-sent above the expected miss" figure the
// churn diagnostics state (docs/CACHE.md), the same max(0, ...) the grouping
// pass's observeChurn uses for the banner.
function churnExcess(usage: UsageBlock): number {
  return Math.max(0, usage.prompt_cache_miss_tokens - usage.expected_miss_tokens);
}

// ChurnWarn is the gutter's amber warn glyph: a churn point is the one
// thing on this page that states itself without being asked.
function ChurnWarn({ usage }: { usage: UsageBlock }) {
  if (usage.churn_point_index === undefined) return null;
  const excess = churnExcess(usage);
  return (
    <span className="warn" title={`Cache churn: ${excess.toLocaleString("en-US")} tokens re-sent above the expected miss`}>
      ⚠
    </span>
  );
}

// TurnMeta is the per-turn telemetry line (.turnmeta): elapsed, cache
// hit, output tokens, cost — churn first when the
// usage carried a churn point. Every figure comes from data the fold already
// carries; nothing here parses or re-derives.
function TurnMeta({ usage, elapsedMs }: { usage: UsageBlock; elapsedMs?: number }) {
  const hit = usage.prompt_cache_hit_tokens + usage.prompt_cache_miss_tokens;
  const churn = usage.churn_point_index !== undefined;
  const excess = churnExcess(usage);
  return (
    <div className="turnmeta">
      {churn && (
        <span className="warn" title={`Cache churn: ${excess.toLocaleString("en-US")} tokens re-sent above the expected miss`}>
          churn · {excess.toLocaleString("en-US")} re-sent
        </span>
      )}
      {elapsedMs !== undefined && <span>{formatElapsed(elapsedMs)}</span>}
      {hit > 0 && <span>{cachePercent(usage.prompt_cache_hit_tokens, usage.prompt_cache_miss_tokens)}% cache</span>}
      <span>{usage.completion_tokens} out</span>
      <span>${formatCost(usage.cost_usd)}</span>
    </div>
  );
}

// ToolRows is a frozen group's tool calls, one .tool disclosure per call in
// the order the model made them. Two or more calls in one turn read as a
// set, wrapped in a .toolset with the "N calls in parallel" label. A
// call whose result has not landed is deliberately absent here — the
// live section below the list is what
// shows it while it runs.
function ToolRows({
  group,
  getToolCall,
}: {
  group: SubTurnGroup;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  const assistant = group.blocks[0];
  if (assistant.type !== "assistant") return null;

  const resultsByCallId = new Map<string, ToolResultLike>();
  for (const block of group.blocks.slice(1)) {
    if (block.type === "tool_result" || block.type === "tool_denied") resultsByCallId.set(block.tool_call_id, block);
  }

  const paired = new Set<string>();
  const rows: { call: ToolCallPayload | undefined; result: ToolResultLike }[] = [];
  for (const call of assistant.toolCalls) {
    const result = resultsByCallId.get(call.id);
    if (!result) continue;
    paired.add(call.id);
    rows.push({ call: getToolCall(call.id) ?? call, result });
  }
  // Defensive: a result the fold did not pair with any call (should not
  // happen — the fold attaches every result to the call that produced it).
  for (const block of group.blocks.slice(1)) {
    if ((block.type === "tool_result" || block.type === "tool_denied") && !paired.has(block.tool_call_id)) {
      rows.push({ call: block.call, result: block });
    }
  }
  if (rows.length === 0) return null;

  const rowsEl = rows.map((row) => <ToolRow key={row.result.tool_call_id} call={row.call} result={row.result} />);
  if (rows.length >= 2) {
    return (
      <div className="toolset">
        <div className="setlabel">{rows.length} calls in parallel</div>
        {rowsEl}
      </div>
    );
  }
  return rowsEl;
}

// ToolRow is one frozen tool call (.tool): closed it is glyph, name,
// target, and the single most useful number for that
// tool; open it shows the result. A failed call is .tool-err and open by
// default — an error you have to click to see is an error you will miss.
function ToolRow({ call, result }: { call: ToolCallPayload | undefined; result: ToolResultLike }) {
  const isError = result.type === "tool_denied" || result.is_error === true;
  const isEdit = result.name === "Edit" || result.name === "Write";
  const stat = toolStat(result);
  const target = toolDetail(call);
  return (
    <details className={`tool${isError ? " tool-err" : isEdit ? "" : " tool-ok"}`} open={isError}>
      <summary>
        <span className="glyph" aria-hidden>
          {isError ? "✗" : isEdit ? "±" : "✓"}
        </span>
        <span className="name">{result.name}</span>
        {target && <span className="target">{target}</span>}
        {stat.length > 0 && (
          <span className={`timing${stat.some((p) => p.cls) ? " diffstat" : ""}`}>
            {stat.map((part, i) => (
              <span key={i} className={part.cls}>
                {i > 0 ? " " : ""}
                {part.text}
              </span>
            ))}
          </span>
        )}
      </summary>
      <ToolBody call={call} result={result} />
    </details>
  );
}

// ToolBody is a frozen tool result's body: the computed diff table for an
// edit, the images for a capture or a vision review, the raw output for
// everything else. Long output truncates to a head and a tail with an elided
// row — no scroll container inside the turn list.
function ToolBody({ call, result }: { call: ToolCallPayload | undefined; result: ToolResultLike }) {
  if (result.type === "tool_denied") {
    const body = deniedBody(result.rule, result.content);
    return (
      <pre className="tool-out">
        {[body.rule === null ? "" : `rule: ${body.rule}`, body.content].filter(Boolean).join("\n")}
      </pre>
    );
  }
  if ((result.name === "Edit" || result.name === "Write") && result.diff && result.diff.length > 0) {
    // The diff was already computed server-side and arrives as a structured
    // line array (docs/DESIGN.md §5.4); the browser only renders the table.
    return (
      <div className="tool-out">
        <DiffTable diff={result.diff} />
      </div>
    );
  }
  // The images a capture wrote or a review looked at, above the text that
  // describes them. Without this the transcript says what Gemini thought of a
  // page and never shows the page (docs/TOOLS.md, "Seeing the screenshots").
  // Read from the call rather than the result, so they still appear when the
  // call itself failed — a capture that came out blank is exactly when
  // seeing it matters.
  if (result.name === "Screenshot" || result.name === "ReviewScreenshot") {
    const paths = screenshotPaths(call);
    if (paths.length > 0) {
      return (
        <>
          <ScreenshotGallery paths={paths} />
          <ElidedOutput text={result.content} />
        </>
      );
    }
  }
  return <ElidedOutput text={result.content} />;
}

// ElidedOutput renders a tool result's text whole when it is short, and
// as head, elided row, tail when it is long. Expanding grows the page —
// the elided row is a button, never a scroll container.
function ElidedOutput({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const split = useMemo(() => elideLines(text), [text]);
  if (!split) return <pre className="tool-out">{text}</pre>;
  if (expanded) {
    return (
      <>
        <pre className="tool-out">{text}</pre>
        <button type="button" className="elided" onClick={() => setExpanded(false)}>
          Show less
        </button>
      </>
    );
  }
  return (
    <>
      <pre className="tool-out">{split.head}</pre>
      <button type="button" className="elided" onClick={() => setExpanded(true)}>
        ⋯ {split.hidden.toLocaleString("en-US")} lines hidden — show all
      </button>
      <pre className="tool-out">{split.tail}</pre>
    </>
  );
}
