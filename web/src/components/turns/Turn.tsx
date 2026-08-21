import { memo, useMemo, useState } from "react";
import type { SubTurnGroup, UsageBlock } from "../../api/groups";
import type { ToolCallPayload } from "../../api/types";
import { Markdown } from "../../render/Markdown";
import { DiffTable } from "../blocks/DiffTable";
import { formatElapsed } from "../blocks/ReasoningPanel";
import { ScreenshotGallery } from "../blocks/ScreenshotGallery";
import { formatCost, screenshotPaths, toolDetail } from "../blocks/toolArgs";
import { cachePercent, deniedBody, elideLines, toolStat, type ToolResultLike } from "./turnHelpers";
import { InlineImage } from "../blocks/InlineImage";
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
      className={cn(
        "group relative mb-6 scroll-mt-[var(--nav-height)] pl-10 [&>*+*]:mt-2 max-nav:pl-0",
        arrived && "anim-row-in",
      )}
      id={`sub-turn-${group.subTurn}`}
      data-seq={group.seq}
    >
      <a
        className="absolute top-px left-0 w-[30px] text-right font-mono text-micro text-muted-foreground tabular-nums no-underline group-hover:text-foreground focus-visible:text-foreground max-nav:static max-nav:mb-0.5 max-nav:block max-nav:w-auto max-nav:text-left max-phone:min-h-11 max-phone:pt-3 max-phone:pb-2"
        href={`#sub-turn-${group.subTurn}`}
        title={`Sub-turn ${group.subTurn}`}
      >
        {group.subTurn}
        {group.usage && <ChurnWarn usage={group.usage} />}
      </a>
      {group.usage && <TurnMeta usage={group.usage} elapsedMs={assistant.reasoningElapsedMs} />}
      {assistant.reasoning && (
        // "think" stays as a literal class only so the cross-browser
        // <details> marker hiding below stays scoped to it
        // (.think > summary::-webkit-details-marker in styles.css) — every
        // other property here is a direct Tailwind utility on the elements
        // that carry it.
        <details className="think">
          <summary className="-ml-1 inline-flex list-none items-center gap-1.5 rounded px-1 py-px text-xs text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-1 max-phone:min-h-11 max-phone:px-2 max-phone:py-2.5">
            <span
              className="text-muted-foreground transition-transform [transition-duration:var(--dur-caret)] motion-reduce:transition-none"
              aria-hidden
            >
              ▸
            </span>
            Thought{assistant.reasoningElapsedMs !== undefined ? ` for ${formatElapsed(assistant.reasoningElapsedMs)}` : ""} ·{" "}
            {assistant.reasoningTokens !== undefined
              ? assistant.reasoningTokens
              : `~${Math.max(1, Math.round(assistant.reasoning.length / 4))}`}{" "}
            tokens
          </summary>
          <div className="mt-1.5 border-l border-border pl-[13px] text-sm leading-[1.6] whitespace-pre-wrap text-muted-foreground">
            {assistant.reasoning}
          </div>
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
        //
        // "say" stays as a literal class so the Markdown output's own <p>
        // and <code> tags — DOM this component doesn't control — keep their
        // spacing and inline-code chrome (.say p/.say code in styles.css);
        // the container's own font size and line height are direct
        // utilities.
        <div
          className={cn(
            "say text-md leading-[1.65] max-phone:text-base max-phone:leading-[1.7]",
            !arrived && "anim-stream-in",
          )}
        >
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
// thing on this page that states itself without being asked. Forced to
// `[display:block]` rather than the bare `block` utility — a real custom
// `.block` class (unconverted transcript block styling) still exists in
// styles.css and collides on the compiled class name.
function ChurnWarn({ usage }: { usage: UsageBlock }) {
  if (usage.churn_point_index === undefined) return null;
  const excess = churnExcess(usage);
  return (
    <span
      className="[display:block] text-[var(--status-gaveup)]"
      title={`Cache churn: ${excess.toLocaleString("en-US")} tokens re-sent above the expected miss`}
    >
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
    <div className="absolute top-0 right-0 flex gap-2.5 bg-background pl-2.5 text-micro text-muted-foreground tabular-nums opacity-0 [transition:opacity_100ms_ease] group-hover:opacity-100 group-focus-within:opacity-100 motion-reduce:transition-none">
      {churn && (
        <span
          className="text-[var(--status-gaveup)]"
          title={`Cache churn: ${excess.toLocaleString("en-US")} tokens re-sent above the expected miss`}
        >
          churn · {excess.toLocaleString("en-US")} re-sent
        </span>
      )}
      {elapsedMs !== undefined && <span>{formatElapsed(elapsedMs)}</span>}
      {hit > 0 && <span>{cachePercent(usage.prompt_cache_hit_tokens, usage.prompt_cache_miss_tokens)}% cache</span>}
      <span>{usage.completion_tokens} out</span>
      <span>${formatCost(usage.cost_usd)}</span>
      {/* Only peak. "off_peak" is two thirds of the day and "flat" is every
          sub-turn before the split and every one on a provider that never had
          one, so a chip for those would sit on nearly every turn in the
          transcript — and a mark that is always there is a mark nobody reads.
          Peak is the one that explains a cost the token counts beside it do
          not (docs/DESIGN.md §4.9). .rate-peak stays a residual class: it is
          shared with blocks/SubTurnCard.tsx, out of scope here. */}
      {usage.rate_tier === "peak" && (
        <span className="uppercase text-[0.625rem] tracking-[0.06em] text-[var(--status-gaveup)]" title="billed at DeepSeek's peak rate — twice off-peak">
          peak
        </span>
      )}
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
      <div className="border-l border-border pl-[11px]">
        <div className="mb-[5px] text-micro text-muted-foreground">{rows.length} calls in parallel</div>
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
// The error/edit/ok state reads straight off isError/isEdit rather than a
// tool-err/tool-ok class plus a descendant selector — the same "boolean
// already in scope" swap the SubTurnCard pass made for its own error
// background. "tool" survives as a literal class only to keep the
// marker-hiding rule scoped (.tool > summary::-webkit-details-marker).
function ToolRow({ call, result }: { call: ToolCallPayload | undefined; result: ToolResultLike }) {
  const isError = result.type === "tool_denied" || result.is_error === true;
  const isEdit = result.name === "Edit" || result.name === "Write";
  const stat = toolStat(result);
  const target = toolDetail(call);
  return (
    <details
      className={cn(
        "tool rounded-[calc(var(--radius)-2px)] border bg-card [&+&]:mt-1",
        isError ? "border-[var(--status-failed)]" : "border-border",
      )}
      open={isError}
    >
      <summary className="flex list-none items-center gap-2 rounded-[calc(var(--radius)-3px)] px-2.5 py-[7px] text-sm cursor-pointer hover:bg-accent focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-[-2px] max-phone:min-h-11 max-phone:px-3 max-phone:py-2.5">
        <span
          aria-hidden
          className={cn(
            "w-[13px] flex-none text-center text-xs",
            isError ? "text-[var(--status-failed)]" : isEdit ? "text-muted-foreground" : "text-[var(--status-done)]",
          )}
        >
          {isError ? "✗" : isEdit ? "±" : "✓"}
        </span>
        <span className="flex-none font-mono text-xs font-semibold">{result.name}</span>
        {target && <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{target}</span>}
        {stat.length > 0 && (
          <span
            className={cn(
              "flex-none text-micro tabular-nums",
              isError ? "text-[var(--status-failed)]" : "text-muted-foreground",
            )}
          >
            {stat.map((part, i) => (
              <span
                key={i}
                // A diffstat's own +/− colour only shows through when the
                // call did not also fail — the same outcome the old
                // .tool-err > summary .timing / .diffstat .add /.del
                // specificity fight produced (three classes beat two).
                className={
                  isError
                    ? undefined
                    : part.cls === "add"
                      ? "text-[var(--diff-add-fg)]"
                      : part.cls === "del"
                        ? "text-[var(--diff-remove-fg)]"
                        : undefined
                }
              >
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
      <pre className="m-0 border-t border-border px-2.5 py-2 font-mono text-xs leading-[1.55] whitespace-pre-wrap wrap-anywhere text-muted-foreground max-phone:text-sm">
        {[body.rule === null ? "" : `rule: ${body.rule}`, body.content].filter(Boolean).join("\n")}
      </pre>
    );
  }
  if ((result.name === "Edit" || result.name === "Write") && result.diff && result.diff.length > 0) {
    // The diff was already computed server-side and arrives as a structured
    // line array (docs/DESIGN.md §5.4); the browser only renders the table.
    return (
      // The diff table sets its own font-size but inherits font-family, so
      // this wrapper still needs font-mono even though it carries no text
      // of its own — matching what it inherited from the old .tool-out div.
      <div className="border-t border-border px-2.5 py-2 font-mono">
        <DiffTable diff={result.diff} />
      </div>
    );
  }
  // The images a capture wrote, a vision tool looked at, or a crop cut out,
  // above the text that describes them. Without this the transcript says
  // what a vision tool thought of a page and never shows the page
  // (docs/TOOLS.md, "Seeing the screenshots"). Read from the call rather than
  // the result, so they still appear when the call itself failed — a capture
  // that came out blank is exactly when seeing it matters.
  if (
    result.name === "Screenshot" ||
    result.name === "Glance" ||
    result.name === "Ground" ||
    result.name === "Detect" ||
    result.name === "Transcribe" ||
    result.name === "Crop"
  ) {
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
  // The image a Read returned to a vision provider, above the line of text
  // that names the file — the same ordering the screenshot gallery above
  // uses, and for the same reason: the picture is what the result is.
  //
  // image_href is what the HTTP surface sends, a URL onto the bytes
  // (internal/httpapi/eventimage.go); image_url is the same picture inline
  // as a data URI, which is what the store holds and what the perf harness
  // and the tests produce. This screen rendered neither until the split went
  // in, so a transcript reported that the model had looked at a render and
  // never showed the render.
  const image = result.image_href ?? result.image_url;
  if (image) {
    return (
      <>
        <InlineImage url={image} />
        <ElidedOutput text={result.content} />
      </>
    );
  }
  return <ElidedOutput text={result.content} />;
}

// ElidedOutput renders a tool result's text whole when it is short, and
// as head, elided row, tail when it is long. Expanding grows the page —
// the elided row is a button, never a scroll container.
function ElidedOutput({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const split = useMemo(() => elideLines(text), [text]);
  const outCls = "m-0 border-t border-border px-2.5 py-2 font-mono text-xs leading-[1.55] whitespace-pre-wrap wrap-anywhere text-muted-foreground max-phone:text-sm";
  // `[display:block]` rather than the bare `block` utility — see ChurnWarn's
  // comment above on the collision with the real .block class.
  const elidedCls =
    "[display:block] w-full cursor-pointer border-y border-dashed border-border bg-muted px-2.5 py-[5px] text-left font-mono text-micro text-muted-foreground hover:text-foreground max-phone:min-h-11 max-phone:px-3 max-phone:py-2.5";
  if (!split) return <pre className={outCls}>{text}</pre>;
  if (expanded) {
    return (
      <>
        <pre className={outCls}>{text}</pre>
        <button type="button" className={elidedCls} onClick={() => setExpanded(false)}>
          Show less
        </button>
      </>
    );
  }
  return (
    <>
      <pre className={outCls}>{split.head}</pre>
      <button type="button" className={elidedCls} onClick={() => setExpanded(true)}>
        ⋯ {split.hidden.toLocaleString("en-US")} lines hidden — show all
      </button>
      <pre className={cn(outCls, "border-t-0")}>{split.tail}</pre>
    </>
  );
}
