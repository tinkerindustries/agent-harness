import { memo, useMemo } from "react";
import type { LiveView } from "../api/fold";
import type { SubTurnGroup, TranscriptItem } from "../api/groups";
import type { SessionState, Todo, ToolCallPayload } from "../api/types";
import { cachePercent } from "./turns/turnHelpers";
import { formatCost, toolDetail } from "./blocks/toolArgs";
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
  const runLive = meta.status === "running";
  const liveSubTurn = live.turn?.subTurn ?? null;
  const view = useMemo(
    () => buildWatchPhases(items, liveSubTurn, todos, runLive, getToolCall),
    [items, liveSubTurn, todos, runLive, getToolCall],
  );

  return (
    <aside className="rail rail-left" aria-label="Navigator">
      <div className="railsec">
        <h3>Plan</h3>
        {todos.length === 0 ? (
          // A run that never wrote a plan ("a run with no plan"): no
          // empty plan card with a 0/0 bar — one line and
          // the flat list of sub-turns, which is all the navigation such a
          // run needs.
          <>
            <p className="rail-note">No plan — this run never wrote one.</p>
            {view.flatTicks.length > 0 && (
              <div className="ticks">
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
                className={`phase${phase.id === view.nowPhaseId ? " phase-now" : " phase-done"}`}
                open={phase.id === view.nowPhaseId}
              >
                <summary>
                  <span className="caret" aria-hidden>
                    ▸
                  </span>
                  <span className="idx">{phase.index > 0 ? phase.index : "·"}</span>
                  <span>{phase.label || "…"}</span>
                </summary>
                <div className="ticks">
                  {phase.ticks.map((tick) => (
                    <TickLink key={`${tick.subTurn}-${tick.cls}`} tick={tick} />
                  ))}
                </div>
              </details>
            ))}
            {view.notStarted.map((item) => (
              <details key={item.index} className="phase">
                <summary>
                  <span className="caret" aria-hidden>
                    ▸
                  </span>
                  <span className="idx">{item.index}</span>
                  <span>{item.label}</span>
                </summary>
                <div className="ticks">
                  <span className="not-started">not started</span>
                </div>
              </details>
            ))}
          </>
        )}
      </div>

      <div className="railsec">
        <h3>Legend</h3>
        <div className="legend">
          <span>
            <i className="tick tick-edit" /> edit
          </span>
          <span>
            <i className="tick tick-bash" /> bash
          </span>
          <span>
            <i className="tick tick-read" /> read
          </span>
          <span>
            <i className="tick tick-err" /> error
          </span>
          <span>
            <i className="tick tick-churn" /> churn
          </span>
          <span>
            <i className="tick" /> other
          </span>
        </div>
      </div>

      <div className="railsec">
        <h3>Session</h3>
        <div className="facts">
          <div>
            <span>model</span>
            <span className="v">{meta.model}</span>
          </div>
          <div>
            <span>effort</span>
            <span className="v">{meta.effort}</span>
          </div>
          <div>
            <span>workspace</span>
            <span className="v" title={meta.workspace}>
              {meta.workspace}
            </span>
          </div>
          <div>
            <span>cost</span>
            <span className="v">${formatCost(meta.usage.cost_usd)}</span>
          </div>
          <div>
            <span>cache hit</span>
            <span className="v">
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
    <div className="planhead">
      <span className="progress" aria-hidden>
        <span style={{ width: `${pct}%` }} />
      </span>
      <span className="count">
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
const TickLink = memo(function TickLink({ tick }: { tick: WatchTick }) {
  return (
    <a className={cn("tick", tick.cls)} href={`#sub-turn-${tick.subTurn}`} title={tick.title}>
      <span className="sr-only">{tick.title}</span>
    </a>
  );
});
