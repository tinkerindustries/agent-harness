import { useMemo } from "react";
import { HourglassMedium } from "@phosphor-icons/react";
import type { SessionState } from "../api/types";
import type { TranscriptItem } from "../api/groups";
import type { LiveView } from "../api/fold";
import type { ToolCallPayload } from "../api/types";
import { useNow } from "../hooks";
import { Button } from "./ui/button";
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
}) {
  const now = useNow(1000);

  // The status line's numbers: the sub-turn the footer names — the live
  // turn, or the last frozen one — and the run's totals from the row.
  const status = useMemo(() => watchStatusFigures(meta, items, live), [meta, items, live]);

  // The elapsed figure is the run's wall time on the ticking now; the run
  // is live by construction (the screen renders this footer only then).
  const elapsedMs = now - Date.parse(meta.created_at);

  // What is running right now (.nowline): the last pending tool call,
  // the streaming turn's thinking, or the loop between turns. The plan
  // item it sits under is the tail phase's label — the same phase ref
  // the rail builds its disclosures from. ageMs is the running thing's
  // OWN age — the pending tool's start stamp from the fold, or the
  // streaming turn's — never the run's elapsed, which the status
  // line below owns ("1m 04s" under the tool, "4m 12s elapsed" in the
  // status line). Null between sub-turns, where there is nothing to age.
  const activity = useMemo(() => {
    let name = "";
    let arg = "";
    let ageMs: number | null = null;
    if (live.pendingTools.size > 0) {
      const pending = [...live.pendingTools.values()];
      const last = pending[pending.length - 1].call;
      const full = getToolCall(last.id) ?? last;
      name = full.name;
      arg = toolDetail(full);
      ageMs = now - Date.parse(pending[pending.length - 1].startedAt);
    } else if (live.turn) {
      name = "Thinking";
      ageMs = now - Date.parse(live.turn.startedAt);
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
    return { name, arg, phaseLabel, ageMs };
  }, [live.pendingTools, live.turn, items, getToolCall, now]);

  return (
    // "footer" stays a literal class only to keep the shared
    // .footer .statusline margin-top scoped (ChatComposer.tsx's footer
    // carries the same class) — every other footer/footer-inner property
    // is a direct Tailwind utility, since the two footers' grid columns
    // differ enough that a shared modifier class wasn't worth keeping.
    <div className="footer grid grid-cols-[244px_minmax(0,1fr)] border-t border-border bg-background px-6 pt-2.5 pb-[9px]">
      <div className="col-start-2 ml-4 max-w-[840px]">
        {stop.confirming && (
          <div className="flex flex-wrap items-center gap-2.5 rounded-[calc(var(--radius)-2px)] border border-[var(--status-failed)] bg-[var(--status-failed-bg)] px-3 py-2.5 text-sm">
            <b>Stop this run?</b>
            <span className="text-muted-foreground">
              It is {formatRunDuration(elapsedMs)} in{activity.name === "Between sub-turns" ? "" : `, mid ${activity.name}${activity.arg ? ` ${activity.arg}` : ""}`}. The work it has done stays in the workspace.
            </span>
            <span className="flex-1" />
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
        <div className="mb-1.5 flex items-center gap-2 text-sm">
          <span className="dot dot-pulse" style={{ color: "var(--status-running)" }} aria-hidden />
          <span className="font-mono text-xs font-semibold">{activity.name}</span>
          {activity.arg && <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{activity.arg}</span>}
          <span className="max-w-[45%] flex-none truncate text-xs text-muted-foreground">
            {activity.ageMs !== null ? formatRunDuration(activity.ageMs) : ""}
            {activity.phaseLabel ? `${activity.ageMs !== null ? " · " : ""}under “${activity.phaseLabel}”` : ""}
          </span>
          <span className="flex-1" />
          <button
            type="button"
            className="inline-flex h-[26px] cursor-pointer items-center gap-1.5 rounded-full border border-ring bg-secondary px-2.5 font-[inherit] text-xs text-foreground max-phone:min-h-11 max-phone:px-4"
            onClick={onToggleFollow}
          >
            <span className="dot" style={{ color: following ? "var(--status-running)" : "var(--muted-foreground)" }} aria-hidden />
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
