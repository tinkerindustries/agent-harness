import { useMemo, type CSSProperties } from "react";
import { HourglassMedium } from "@phosphor-icons/react";
import type { SessionState } from "../api/types";
import type { TranscriptItem } from "../api/groups";
import type { LiveView } from "../api/fold";
import type { ToolCallPayload } from "../api/types";
import { liveness, stallNote, type DurationStats, type PulseMeter } from "../api/pulse";
import { useNow } from "../hooks";
import { Button } from "./ui/button";
import { RunPulse } from "./ui/RunPulse";
import { Ticker } from "./ui/Ticker";
import { cachePercent, formatRunDuration, watchStatusFigures } from "./turns/turnHelpers";
import { formatCost, toolDetail } from "./blocks/toolArgs";

// WatchFooter is the watch page's footer band (.footer): the answer to
// the only question a spectator has while the run is live — what is it
// doing right now — kept visible even when they have
// scrolled back through history, which is the point of it. The chat page's
// footer is a composer; this one is a status bar: the running tool and its
// argument, how long it has been running, the plan item it sits under, the
// follow toggle, and the status line of sub-turn, cache, cost, output and
// elapsed. It carries the stop flow too — the nav's Stop arms the same
// inline confirm strip the chat page uses, because the strip belongs where
// the run's controls live, not in a modal over the transcript. It renders
// only while the run is live — the screen gates it — because a finished run
// is an ordinary page with the footer's figures in the nav instead.
export function WatchFooter({
  meta,
  items,
  live,
  following,
  onToggleFollow,
  stop,
  getToolCall,
  pulse,
  durations,
}: {
  meta: SessionState;
  items: TranscriptItem[];
  live: LiveView;
  // Whether the stream is pinned to the tail (the follow toggle's state,
  // owned by the screen with the scroll logic).
  following: boolean;
  onToggleFollow: () => void;
  stop: {
    confirming: boolean;
    stopping: boolean;
    error: string | null;
    onCancelStop: () => void;
    onConfirmStop: () => void;
  };
  getToolCall: (id: string) => ToolCallPayload | undefined;
  // The liveness layer, straight off the store's snapshot (api/pulse.ts).
  // Both are stable references and neither is state: the meter is read by the
  // canvas on its own frame, and the baselines are read here once a second on
  // the clock this footer already re-renders on.
  pulse: PulseMeter;
  durations: DurationStats;
}) {
  const now = useNow(1000);

  // How hard this run is breathing. The dot's --pulse-period comes from the
  // current activity's age measured against this session's own median, so a
  // tool call that has been going four times as long as normal for this run
  // slows the dot to a heavy throb — and a run doing ordinary work looks
  // exactly as it always did. The gate is per-run rather than absolute
  // because there is no absolute: a session of Reads and a session of builds
  // have nothing in common but the dot.
  const life = liveness(live, durations, now);
  const stall = stallNote(life);

  // The status line's numbers: the sub-turn the footer names — the live
  // turn, or the last frozen one — and the run's totals from the row.
  const status = useMemo(() => watchStatusFigures(meta, items, live), [meta, items, live]);

  // The elapsed figure is the run's wall time on the ticking now; the run
  // is live by construction (the screen renders this footer only then).
  const elapsedMs = now - Date.parse(meta.created_at);

  // What is running right now (.nowline): the last pending tool call,
  // the streaming turn's thinking, or the loop between turns. The plan
  // item it sits under is the tail phase's label — the same phase ref
  // the rail builds its disclosures from. The running thing's OWN age —
  // never the run's elapsed, which the status line below owns ("1m 04s"
  // under the tool, "4m 12s elapsed" in the status line) — now comes from
  // `life`, which ages the same activity to decide the dot's period; two
  // clocks on one fact is how they come to disagree.
  const activity = useMemo(() => {
    let name = "";
    let arg = "";
    if (live.pendingTools.size > 0) {
      const pending = [...live.pendingTools.values()];
      const last = pending[pending.length - 1].call;
      const full = getToolCall(last.id) ?? last;
      name = full.name;
      arg = toolDetail(full);
    } else if (live.turn) {
      name = "Thinking";
    } else {
      name = "Between sub-turns";
    }
    let phaseLabel: string | null = null;
    for (let i = items.length - 1; i >= 0; i--) {
      const item = items[i];
      if (item.kind === "group") {
        phaseLabel = item.group.phase.label || null;
        break;
      }
    }
    return { name, arg, phaseLabel };
  }, [live.pendingTools, live.turn, items, getToolCall]);

  return (
    <div className="footer footer-watch">
      <div className="footer-inner">
        {stop.confirming && (
          <div className="confirm">
            <b>Stop this run?</b>
            <span className="muted">
              It is {formatRunDuration(elapsedMs)} in{activity.name === "Between sub-turns" ? "" : `, mid ${activity.name}${activity.arg ? ` ${activity.arg}` : ""}`}. The work it has done stays in the workspace.
            </span>
            <span className="spacer" />
            <Button variant="outline" size="sm" onClick={stop.onCancelStop} disabled={stop.stopping}>
              Keep running
            </Button>
            <Button variant="destructive" size="sm" onClick={stop.onConfirmStop} disabled={stop.stopping}>
              {stop.stopping ? "Stopping…" : "Stop run"}
            </Button>
          </div>
        )}
        {stop.stopping && (
          <div className="banner">
            {/* The hourglass takes the pulse the dot carried: waiting is what
                the banner is about, and the mark now says so. */}
            <HourglassMedium className="dot-pulse" style={{ color: "var(--status-gaveup)" }} aria-hidden />
            <span>
              <b>Stopping…</b> waiting for the current tool call to return. The run ends at the next boundary.
            </span>
          </div>
        )}
        {stop.error && <span className="field-error">{stop.error}</span>}
        {/* The run pulse leads the band: what the run has actually produced
            over the last minute and a half, above the line that says what it
            is doing. The order is deliberate — the shape is the thing you
            read at a glance and the words are what you read when the shape
            looks wrong. */}
        <RunPulse meter={pulse} className="pulse-strip" />
        <div className="nowline" title={stall}>
          <span
            className="dot dot-pulse"
            // The one place a live figure drives a duration rather than a
            // label. Set inline because it is per-run data, not a theme
            // choice: the token in styles.css stays the resting value and
            // this overrides it only while there is a reading to override
            // it with.
            style={{ color: "var(--status-running)", "--pulse-period": `${Math.round(life.periodMs)}ms` } as CSSProperties}
            aria-hidden
          />
          <span className="name">{activity.name}</span>
          {activity.arg && <span className="arg">{activity.arg}</span>}
          <span className="under">
            {life.ageMs !== null ? formatRunDuration(life.ageMs) : ""}
            {activity.phaseLabel ? `${life.ageMs !== null ? " · " : ""}under “${activity.phaseLabel}”` : ""}
          </span>
          <span className="spacer" />
          <button type="button" className={`follow${following ? "" : " follow-off"}`} onClick={onToggleFollow}>
            <span className="dot" aria-hidden />
            {following ? "Following live" : "Follow live"}
          </button>
        </div>
        <div className="statusline">
          {status.subTurn !== null ? (
            <>
              <span>sub-turn {status.subTurn}</span>
              <span className="sep">·</span>
              {/* Cost and cache rate move while the run is live, so they roll;
                  the rest of the line is either static or ticking too fast to
                  be worth a gesture. */}
              <Ticker value={cachePercent(status.cacheHitTokens, status.cacheMissTokens)} suffix="% cache" />
              <span className="sep">·</span>
              <span title="Price table captured by the server's pricing config">
                <Ticker value={formatCost(status.costUsd)} prefix="$" />
              </span>
              <span className="sep">·</span>
              <span>{status.completionTokens.toLocaleString("en-US")} out</span>
              <span className="sep">·</span>
              <span>{formatRunDuration(elapsedMs)} elapsed</span>
            </>
          ) : (
            <>
              <span>{meta.model}</span>
              <span className="sep">·</span>
              <span>effort {meta.effort}</span>
            </>
          )}
          <span className="spacer" />
          <span>read-only — no steering on an agent-launched run</span>
        </div>
      </div>
    </div>
  );
}
