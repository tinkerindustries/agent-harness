import { memo, useMemo } from "react";
import type { LiveView } from "../api/fold";
import type { SubTurnGroup, TranscriptItem } from "../api/groups";
import type { SessionState, Todo, ToolCallPayload } from "../api/types";
import { isLive } from "../api/status";
import { cachePercent } from "./turns/turnHelpers";
import { formatCost, toolDetail } from "./blocks/toolArgs";
import { ElidedPath } from "./ui/ElidedPath";
import { cn } from "@/lib/utils";

// WatchRail is the watch page's left-hand navigator: one column that
// answers "where in this run" — the plan as phases with one tick per
// sub-turn, then the legend and the session facts. It replaces the
// transcript's old toolbar/rail/plan-panel trio, which the chat page
// already dropped; the two session pages now differ in silhouette (rail
// left, rail right) and this rail is how a watcher finds the thing they
// came for: the error among 78 sub-turns, the edit that touched a file,
// the churn point. The ticks' colours are the whole index — there is no
// find box and no filter chips above them.
//
// The phases are built from the groups' own phase refs (groups.ts:
// RailPhaseRef, captured the moment each group froze) — never by walking
// the TaskCreate/TaskUpdate history again. Each disclosure holds one tick
// per sub-turn that ran under it, coloured by what happened in that
// sub-turn; the running sub-turn pulses. A tick links to its turn, which
// carries id="sub-turn-N".
export function WatchRail({
  items,
  live,
  todos,
  meta,
  getToolCall,
}: {
  items: TranscriptItem[];
  live: LiveView;
  todos: Todo[];
  meta: SessionState;
  getToolCall: (id: string) => ToolCallPayload | undefined;
}) {
  // The run is live until the row says otherwise; the phase containing the
  // tail group is the one the next sub-turn continues. The builder depends
  // on primitives only (the live turn's number, not the live view object),
  // so the token-rate hot path — a live-only delta that leaves items
  // reference-identical — bails out instead of rebuilding the phases.
  const runLive = isLive(meta.status);
  const liveSubTurn = live.turn?.subTurn ?? null;
  const view = useMemo(
    () => buildWatchPhases(items, liveSubTurn, todos, runLive, getToolCall),
    [items, liveSubTurn, todos, runLive, getToolCall],
  );

  return (
    // "rail" stays a literal class: .app .rail's overflow-y:auto reaches in
    // from a body-level class SessionScreen computes outside React
    // (TopNav's own residual .app/.page rules are the same pattern). Every
    // other rail/rail-note/facts property below is now a direct
    // Tailwind utility, shared with ChatRail.tsx's identical chrome.
    <aside className="rail border-r border-border px-4 pt-4 pb-6" aria-label="Navigator">
      <div className="[&+&]:mt-5 [&+&]:border-t [&+&]:border-border [&+&]:pt-4">
        <h3 className="m-0 mb-2.5 text-micro font-semibold tracking-[0.06em] text-muted-foreground uppercase">Plan</h3>
        {todos.length === 0 ? (
          // A run that never wrote a plan ("a run with no plan"): no
          // empty plan card with a 0/0 bar — one line and
          // the flat list of sub-turns, which is all the navigation such a
          // run needs.
          <>
            <p className="m-0 text-xs text-muted-foreground">No plan — this run never wrote one.</p>
            {view.flatTicks.length > 0 && (
              <div className="flex flex-wrap gap-[3px] px-1.5 pt-1 pb-2 pl-6">
                {view.flatTicks.map((tick) => (
                  <TickLink key={`${tick.subTurn}-${tick.cls}`} tick={tick} />
                ))}
              </div>
            )}
          </>
        ) : (
          <>
            <PlanHead todos={todos} />
            {view.phases.map((phase) => (
              <details
                key={phase.id}
                // "phase" stays a literal class only to scope the
                // cross-browser marker-hiding rule
                // (.phase > summary::-webkit-details-marker in styles.css).
                className={cn("phase group mb-1", phase.id === view.nowPhaseId && "bg-[var(--status-running-bg)]")}
                // While the run is live only the phase it is in matters, so
                // the rest stay shut. Once it is over the rail is a review
                // instrument and the ticks are the whole of it — a column of
                // collapsed rows hides the one thing the reader came for.
                open={runLive ? phase.id === view.nowPhaseId : true}
              >
                <summary
                  className={cn(
                    "flex list-none items-baseline gap-1.5 rounded-[calc(var(--radius)-3px)] px-1.5 py-[5px] text-sm leading-[1.35] cursor-pointer hover:bg-accent focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-[-2px]",
                    phase.id !== view.nowPhaseId && "text-muted-foreground",
                    phase.id === view.nowPhaseId && "font-medium",
                  )}
                >
                  <span className="text-muted-foreground [transition:transform_150ms_ease] group-open:rotate-90" aria-hidden>
                    ▸
                  </span>
                  <span className="flex-none font-mono text-micro text-muted-foreground">
                    {phase.index > 0 ? phase.index : "·"}
                  </span>
                  {/* A phase that ran before the plan existed has no item to
                      name it after — say so rather than trailing an ellipsis
                      that reads like a truncation. */}
                  <span className={phase.label ? undefined : "text-muted-foreground italic"}>
                    {phase.label || "before the plan"}
                  </span>
                </summary>
                <div className="flex flex-wrap gap-[3px] px-1.5 pt-1 pb-2 pl-6">
                  {phase.ticks.map((tick) => (
                    <TickLink key={`${tick.subTurn}-${tick.cls}`} tick={tick} />
                  ))}
                </div>
              </details>
            ))}
            {view.notStarted.map((item) => (
              <details key={item.index} className="phase group mb-1">
                <summary className="flex list-none items-baseline gap-1.5 rounded-[calc(var(--radius)-3px)] px-1.5 py-[5px] text-sm leading-[1.35] text-muted-foreground cursor-pointer hover:bg-accent focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-[-2px]">
                  <span className="text-muted-foreground [transition:transform_150ms_ease] group-open:rotate-90" aria-hidden>
                    ▸
                  </span>
                  <span className="flex-none font-mono text-micro text-muted-foreground">{item.index}</span>
                  <span>{item.label}</span>
                </summary>
                <div className="flex flex-wrap gap-[3px] px-1.5 pt-1 pb-2 pl-6">
                  <span className="text-micro text-muted-foreground">not started</span>
                </div>
              </details>
            ))}
          </>
        )}
      </div>

      <div className="[&+&]:mt-5 [&+&]:border-t [&+&]:border-border [&+&]:pt-4">
        <h3 className="m-0 mb-2.5 text-micro font-semibold tracking-[0.06em] text-muted-foreground uppercase">Legend</h3>
        <div className="flex flex-wrap gap-x-2.5 gap-y-1 text-micro text-muted-foreground">
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls("tick-edit"))} /> edit
          </span>
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls("tick-bash"))} /> bash
          </span>
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls("tick-read"))} /> read
          </span>
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls("tick-err"))} /> error
          </span>
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls("tick-churn"))} /> churn
          </span>
          <span className="inline-flex items-center gap-1">
            <i className={cn(LEGEND_TICK, tickColorCls(""))} /> other
          </span>
        </div>
      </div>

      <div className="[&+&]:mt-5 [&+&]:border-t [&+&]:border-border [&+&]:pt-4">
        <h3 className="m-0 mb-2.5 text-micro font-semibold tracking-[0.06em] text-muted-foreground uppercase">Session</h3>
        <div className="[display:block] text-xs text-muted-foreground">
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>model</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{meta.model}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>effort</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{meta.effort}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>workspace</span>
            <ElidedPath
              className="v min-w-0 truncate text-right font-mono text-micro text-foreground"
              path={meta.workspace}
              keepSession
            />
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>cost</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">${formatCost(meta.usage.cost_usd)}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>cache hit</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">
              {cachePercent(meta.usage.cache_hit_tokens, meta.usage.cache_miss_tokens)}%
            </span>
          </div>
        </div>
      </div>
    </aside>
  );
}

// PlanHead is the plan section's progress bar with the completed ratio
// (.planhead), the same markup the chat rail uses.
function PlanHead({ todos }: { todos: Todo[] }) {
  const done = todos.filter((t) => t.status === "completed").length;
  const pct = Math.round((done / todos.length) * 100);
  return (
    <div className="mb-2.5 flex items-center gap-2">
      <span className="h-1 flex-1 overflow-hidden rounded-full bg-muted" aria-hidden>
        <span className="[display:block] h-full bg-[var(--status-running)]" style={{ width: `${pct}%` }} />
      </span>
      <span className="text-micro text-muted-foreground tabular-nums">
        {done}/{todos.length}
      </span>
    </div>
  );
}

// --- the phases view model, pure and unit-tested (TESTING.md: test the
// helpers, not the components) ---

// WatchTick is one sub-turn's tick: the anchor to its turn and the colour
// class for what happened in it. cls is "" for a plain sub-turn.
export interface WatchTick {
  subTurn: number;
  cls: "tick-edit" | "tick-bash" | "tick-read" | "tick-err" | "tick-churn" | "tick-now" | "";
  title: string;
}

export interface WatchPhase {
  // The first group's phase ref (groups.ts RailPhaseRef): a stable id for
  // the disclosure's key and now-marker, plus the 1-based plan position and
  // subject as captured when the sub-turn froze.
  id: number;
  index: number;
  label: string;
  ticks: WatchTick[];
}

export interface WatchPhasesView {
  phases: WatchPhase[];
  // The plan items the run never reached — the current plan beyond the
  // last phase's position — drawn as "not started" disclosures.
  notStarted: { index: number; label: string }[];
  // The phase the running sub-turn (or the pending tool round) continues,
  // highlighted; null when the run is over or nothing has run yet.
  nowPhaseId: number | null;
  // Every tick in order, for the no-plan fallback's flat row.
  flatTicks: WatchTick[];
}

// buildWatchPhases is the rail's view over the grouped transcript: groups
// under the same phase ref become one phase, and so do consecutive groups
// whose phase refs name the same plan item — a new id is minted on every
// TaskCreate or TaskUpdate, even when the item did not change. The ticks
// carry the sub-turn numbers and colours, built from the groups' own tags
// and phase refs — never by walking the TaskCreate/TaskUpdate history
// again. The running sub-turn's tick pulses inside the tail phase, which is
// the one the next sub-turn continues while the run is live. liveSubTurn is
// the streaming turn's number, or null between turns — the only part of the
// live view this view needs, so callers can memoise on primitives.
export function buildWatchPhases(
  items: TranscriptItem[],
  liveSubTurn: number | null,
  todos: Todo[],
  runLive: boolean,
  getToolCall: (id: string) => ToolCallPayload | undefined,
): WatchPhasesView {
  const phases: WatchPhase[] = [];
  let current: WatchPhase | null = null;
  for (const item of items) {
    if (item.kind !== "group") continue;
    const group = item.group;
    // burstPhases are the other plan items this sub-turn's TaskUpdate calls
    // moved forward, ahead of the one group.phase names it after (groups.ts
    // SubTurnGroup.burstPhases) — each gets its own row, with no ticks of
    // its own, so a burst of completions within one sub-turn doesn't drop
    // every item but the last from the rail. The same merge-on-name rule
    // applies: a burst item naming the phase already current (typically the
    // sub-turn's own predecessor, still open from an earlier sub-turn) is a
    // no-op rather than a duplicate row.
    for (const burst of group.burstPhases) {
      if (!current || current.index !== burst.index || current.label !== burst.label) {
        current = { id: burst.id, index: burst.index, label: burst.label, ticks: [] };
        phases.push(current);
      }
    }
    // A new phase id is minted on every TaskCreate or TaskUpdate, so the
    // common create-then-mark-in_progress pair names two phases after the
    // same plan item; merge on the captured name, keeping the first phase's
    // id for the disclosure keys and the now-marker.
    if (!current || current.index !== group.phase.index || current.label !== group.phase.label) {
      current = { id: group.phase.id, index: group.phase.index, label: group.phase.label, ticks: [] };
      phases.push(current);
    }
    current.ticks.push({
      subTurn: group.subTurn,
      cls: tickCls(group),
      title: tickTitle(group, getToolCall),
    });
  }

  // The sub-turn still streaming has no group yet; it continues the tail
  // phase, and its tick is the pulsing one. Only while the run is live: a
  // finished run's live view can still name its last sub-turn (the fold
  // clears the live turn's text, not the turn), and without the gate the
  // rail ends every finished run with a second, pulsing copy of the last
  // sub-turn claiming to be running now.
  if (runLive && liveSubTurn !== null && current) {
    current.ticks.push({
      subTurn: liveSubTurn,
      cls: "tick-now",
      title: `Sub-turn ${liveSubTurn} · running now`,
    });
  }

  const nowPhaseId = runLive && phases.length > 0 ? phases[phases.length - 1].id : null;

  // Plan items the run never reached: the current plan beyond the highest
  // 1-based position any phase reached, minus completed items. A phase with
  // index 0 (no plan item when it ran) must not reset the anchor.
  let lastIndex = 0;
  for (const phase of phases) {
    if (phase.index > lastIndex) lastIndex = phase.index;
  }
  const notStarted: { index: number; label: string }[] = [];
  for (let i = lastIndex; i < todos.length; i++) {
    if (todos[i].status === "completed") continue;
    notStarted.push({ index: i + 1, label: todos[i].subject });
  }

  return { phases, notStarted, nowPhaseId, flatTicks: phases.flatMap((p) => p.ticks) };
}

// tickCls is the colour rule (.tick-*), in priority order: an error
// sub-turn is red — the thing a watcher hunts for — then churn amber,
// then what the sub-turn did, most consequential first: an edit green, a
// shell command blue, a file read violet. A sub-turn that did none of
// those (a search, a plan call, thinking alone) stays plain. The families
// are the timeline rail's glyph families (toolArgs.ts), so one colour
// means the same thing in both rails.
function tickCls(group: SubTurnGroup): WatchTick["cls"] {
  if (group.tags.errors > 0) return "tick-err";
  if (group.tags.churn) return "tick-churn";
  if (group.tags.edits > 0) return "tick-edit";
  if (group.tags.bash > 0) return "tick-bash";
  if (group.tags.reads > 0) return "tick-read";
  return "";
}

// tickTitle is the tick's hover text (design: "Sub-turn 8 · Edit launch.go",
// "Sub-turn 10 · go build failed"): the number plus the last tool call's
// one-line target, read from the fold's registry.
function tickTitle(group: SubTurnGroup, getToolCall: (id: string) => ToolCallPayload | undefined): string {
  const assistant = group.blocks[0];
  let detail = "";
  if (assistant.type === "assistant" && assistant.toolCalls.length > 0) {
    const call = assistant.toolCalls[assistant.toolCalls.length - 1];
    const full = getToolCall(call.id) ?? call;
    detail = toolDetail(full) || full.name;
  }
  return `Sub-turn ${group.subTurn}${detail ? ` · ${detail}` : ""}`;
}

// TickLink is one tick: a plain anchor to its turn's id="sub-turn-N",
// carrying the title and the colour.
const TICK_BASE = "[display:block] h-[13px] w-[13px] rounded-[3px] border";
// The legend's own ticks are the same colours at a smaller, fixed size
// (.legend .tick), never "tick-now" — nothing in the legend pulses.
const LEGEND_TICK = "[display:block] h-[9px] w-[9px] rounded-[3px] border";

// tickColorCls computes a tick's border/background colour straight from its
// cls, the same "read the value already in scope" swap Turn.tsx's ToolRow
// and TimelineRail's glyphClass made for their own state colouring, in
// place of the old .tick-edit/.tick-bash/etc. classes.
function tickColorCls(cls: WatchTick["cls"]): string {
  switch (cls) {
    case "tick-edit":
      return "border-[var(--diff-add-fg)] bg-[color-mix(in_srgb,var(--diff-add-fg)_55%,var(--diff-add-bg))]";
    case "tick-bash":
      return "border-[var(--status-running)] bg-[color-mix(in_srgb,var(--status-running)_55%,var(--status-running-bg))]";
    case "tick-read":
      return "border-[var(--tick-read)] bg-[color-mix(in_srgb,var(--tick-read)_55%,var(--tick-read-bg))]";
    case "tick-err":
      return "border-[var(--status-failed)] bg-[color-mix(in_srgb,var(--status-failed)_55%,var(--status-failed-bg))]";
    case "tick-churn":
      return "border-[var(--status-gaveup)] bg-[color-mix(in_srgb,var(--status-gaveup)_55%,var(--status-gaveup-bg))]";
    case "tick-now":
      return "border-[var(--status-running)] bg-[var(--status-running)] [animation:pulse_var(--pulse-period)_ease-in-out_infinite] motion-reduce:animate-none";
    default:
      return "border-[color-mix(in_srgb,var(--muted-foreground)_45%,transparent)] bg-[color-mix(in_srgb,var(--muted-foreground)_28%,var(--secondary))]";
  }
}

// TickLink is one tick: a plain anchor to its turn's id="sub-turn-N",
// carrying the title and the colour.
const TickLink = memo(function TickLink({ tick }: { tick: WatchTick }) {
  return (
    <a className={cn(TICK_BASE, tickColorCls(tick.cls))} href={`#sub-turn-${tick.subTurn}`} title={tick.title}>
      <span className="sr-only">{tick.title}</span>
    </a>
  );
});
