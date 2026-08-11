import { useMemo } from "react";
import type { SessionState } from "../api/types";
import type { TranscriptItem } from "../api/groups";
import type { LiveView } from "../api/fold";
import type { ToolCallPayload } from "../api/types";
import { useNow } from "../hooks";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { outcome } from "./statusBadge";
import { cachePercent, formatRunDuration } from "./turns/turnHelpers";
import { formatCost, toolDetail } from "./blocks/toolArgs";

// WatchFooter is the watch page's footer band (design/session-watch.html's
// .footer): the answer to the only question a spectator has while the run is
// live — what is it doing right now — kept visible even when they have
// scrolled back through history, which is the point of it. The chat page's
// footer is a composer; this one is a status bar: the running tool and its
// argument, how long it has been running, the plan item it sits under, the
// follow toggle, and the status line of sub-turn, cache, cost, output and
// elapsed. It carries the stop flow too — the nav's Stop arms the same
// inline confirm strip the chat page uses, because the strip belongs where
// the run's controls live, not in a modal over the transcript.
export function WatchFooter({
  running,
  meta,
  items,
  live,
  following,
  onToggleFollow,
  stop,
  getToolCall,
}: {
  running: boolean;
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
  const status = useMemo(() => {
    let subTurn: number | null = live.turn?.subTurn ?? null;
    if (subTurn === null) {
      for (let i = items.length - 1; i >= 0; i--) {
        const item = items[i];
        if (item.kind === "group") {
          subTurn = item.group.subTurn;
          break;
        }
      }
    }
    return {
      subTurn,
      cacheHitTokens: meta.usage.cache_hit_tokens,
      cacheMissTokens: meta.usage.cache_miss_tokens,
      costUsd: meta.usage.cost_usd,
      completionTokens: meta.usage.completion_tokens,
    };
  }, [meta, live.turn?.subTurn, items]);

  const elapsedMs = now - Date.parse(meta.created_at);
  const finishedOutcome = running ? null : outcome(meta);

  // What is running right now (design/session-watch.html's .nowline): the
  // last pending tool call, the streaming turn's thinking, or the loop
  // between turns. The plan item it sits under is the tail phase's label —
  // the same phase ref the rail builds its disclosures from. ageMs is the
  // running thing's OWN age — the pending tool's start stamp from the fold,
  // or the streaming turn's — never the run's elapsed, which the status
  // line below owns (design/session-watch.html: "1m 04s" under the tool,
  // "4m 12s elapsed" in the status line). Null between sub-turns, where
  // there is nothing to age.
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
    <div className="footer footer-watch">
      <div className="footer-inner">
        {stop.confirming && (
          <div className="confirm">
            <b>Stop this run?</b>
            <span className="muted">
              {running ? `It is ${formatRunDuration(elapsedMs)} in${activity.name === "Between sub-turns" ? "" : `, mid ${activity.name}${activity.arg ? ` ${activity.arg}` : ""}`}. The work it has done stays in the workspace.` : "The work it has done stays in the workspace."}
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
            <span className="dot dot-pulse" style={{ color: "var(--status-gaveup)" }} aria-hidden />
            <span>
              <b>Stopping…</b> waiting for the current tool call to return. The run ends at the next boundary.
            </span>
          </div>
        )}
        {stop.error && <span className="field-error">{stop.error}</span>}
        {running && (
          <div className="nowline">
            <span className="dot dot-pulse" style={{ color: "var(--status-running)" }} aria-hidden />
            <span className="name">{activity.name}</span>
            {activity.arg && <span className="arg">{activity.arg}</span>}
            <span className="under">
              {activity.ageMs !== null ? formatRunDuration(activity.ageMs) : ""}
              {activity.phaseLabel ? `${activity.ageMs !== null ? " · " : ""}under “${activity.phaseLabel}”` : ""}
            </span>
            <span className="spacer" />
            <button type="button" className={`follow${following ? "" : " follow-off"}`} onClick={onToggleFollow}>
              <span className="dot" aria-hidden />
              {following ? "Following live" : "Follow live"}
            </button>
          </div>
        )}
        <div className="statusline">
          {status.subTurn !== null ? (
            <>
              <span>sub-turn {status.subTurn}</span>
              <span className="sep">·</span>
              <span>{cachePercent(status.cacheHitTokens, status.cacheMissTokens)}% cache</span>
              <span className="sep">·</span>
              <span title="Price table captured by the server's pricing config">${formatCost(status.costUsd)}</span>
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
          {running ? (
            <span>read-only — no steering on an agent-launched run</span>
          ) : (
            finishedOutcome && <Badge variant={finishedOutcome.variant}>{finishedOutcome.label}</Badge>
          )}
        </div>
      </div>
    </div>
  );
}
