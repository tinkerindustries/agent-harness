import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptFilter } from "../api/groups";
import { useTranscriptStore } from "../hooks";
import { BlockList } from "./BlockList";
import { PlanPanel } from "./PlanPanel";
import { TimelineRail } from "./TimelineRail";
import { TranscriptToolbar } from "./TranscriptToolbar";
import { StopControl } from "./StopControl";
import { SteerControl } from "./SteerControl";
import { Badge } from "./ui/badge";
import { outcome, type OutcomeSession } from "./statusBadge";
import { startedBy } from "../api/provenance";
import { useNavRight } from "./TopNav";
import type { Density } from "./blocks/SubTurnCard";

interface Props {
  sessionId: string;
}

export function TranscriptScreen({ sessionId }: Props) {
  const store = useTranscriptStore(sessionId);
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  // Re-fetch metadata when the stream closes, so the status badge picks up
  // the terminal status the transcript's own run_finished/error block
  // already shows below it, rather than freezing on whatever GET
  // /api/sessions/{id} returned back when the screen first mounted.
  const meta = useSessionMeta(sessionId, snapshot.connection);
  const badge = meta ? outcome(headerOutcomeSession(meta, snapshot.blocks)) : null;
  // The one-line provenance label ("started by geoff", "started by
  // claude-code (sess-1)"), rendered only when the row carries provenance at
  // all (web/src/api/provenance.ts owns the legacy fallback).
  const startedByLabel = meta ? startedBy(meta) : null;

  // Phase 5 display state (docs/WEB-REDESIGN.md): density starts Compact so
  // a long session opens readable, and the filter starts at All. Both are
  // plain strings, so the memoised card/list components compare them by
  // value and still bail out on every live-only delta; toggling one is the
  // one deliberate re-render.
  const [density, setDensity] = useState<Density>("compact");
  const [filter, setFilter] = useState<TranscriptFilter>("all");
  // The transcript column the phase 6 rail's single IntersectionObserver
  // watches for the current marker (docs/WEB-REDESIGN.md phase 6).
  const transcriptRef = useRef<HTMLDivElement>(null);

  // The nav's right slot for this screen (design/nav.html's Session detail
  // state): the connection badge, and the Stop control while the run is
  // live. The crumb for the session id is the nav's own, rendered from the
  // route.
  //
  // Stop lives here rather than in a screen header because phase 9 retired
  // the transcript's own header into this slot; StopControl renders nothing
  // once the session stops running, so the slot falls back to the badge
  // alone without a conditional here.
  useNavRight(
    <>
      <StopControl sessionId={sessionId} running={meta?.status === "running"} className="stop-nav" />
      <Badge variant="outline" className={`connection-badge connection-${snapshot.connection}`}>
        {snapshot.connection}
      </Badge>
    </>,
  );

  return (
    <div className="screen screen-transcript">
      {meta && badge && (
        <div className="session-meta">
          <Badge variant={badge.variant}>{badge.label}</Badge>
          <span>
            {meta.model} ({meta.effort})
          </span>
          <span className="dim">{meta.workspace}</span>
          <span className="dim">{meta.permission_mode}</span>
          {meta.job_type && <span className="dim">{meta.job_type}</span>}
          {startedByLabel && <span className="dim">{startedByLabel}</span>}
          {meta.parent_id && (
            <span className="dim">
              forked from <code>{meta.parent_id}</code>
            </span>
          )}
        </div>
      )}
      <TranscriptToolbar
        density={density}
        onDensityChange={setDensity}
        filter={filter}
        onFilterChange={setFilter}
        counts={snapshot.counts}
      />
      <SteerControl sessionId={sessionId} running={meta?.status === "running"} className="steer-inline" />
      {snapshot.churnPoint && (
        <div className="notice churn-banner">
          <b>
            Cache churn at sub-turn {snapshot.churnPoint.subTurn}: {snapshot.churnPoint.excessTokens.toLocaleString("en-US")} tokens
            re-sent above the expected miss.
          </b>{" "}
          The prefix moved — see docs/CACHE.md. <a href={`#sub-turn-${snapshot.churnPoint.subTurn}`}>Jump to it →</a>
        </div>
      )}
      <div className="transcript-layout">
        <TimelineRail
          items={snapshot.items}
          getToolCall={snapshot.getToolCall}
          filter={filter}
          containerRef={transcriptRef}
        />
        <div ref={transcriptRef} className="transcript-col">
          <BlockList
            items={snapshot.items}
            live={snapshot.live}
            density={density}
            filter={filter}
            getToolCall={snapshot.getToolCall}
          />
        </div>
        <PlanPanel todos={snapshot.todos} />
      </div>
    </div>
  );
}

// headerOutcomeSession is the outcome() input for the transcript header: the
// metadata row plus the run_finished block's reason, when the fold has one.
// The metadata endpoint carries complete_status but not run_finished's
// reason, so STOPPED (answered in prose, no Complete call) is only
// distinguishable on this screen, straight from the folded event log
// (design/components.html). The session list has no reason and renders the
// plain terminal status for the same row.
function headerOutcomeSession(meta: SessionState, blocks: Block[]): OutcomeSession {
  return { status: meta.status, complete_status: meta.complete_status, reason: lastRunFinishedReason(blocks) };
}

function lastRunFinishedReason(blocks: Block[]): string | undefined {
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i];
    if (b.type === "run_finished") return b.reason;
  }
  return undefined;
}

function useSessionMeta(sessionId: string, refreshOn: unknown): SessionState | null {
  const [meta, setMeta] = useState<SessionState | null>(null);

  // Clear on a genuine session switch only, so a refreshOn change (the
  // stream opening or closing) re-fetches without a visible blank flicker
  // in between.
  useEffect(() => setMeta(null), [sessionId]);

  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api/sessions/${encodeURIComponent(sessionId)}`, { signal: controller.signal })
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => data && setMeta(data))
      .catch(() => {
        // A transient fetch failure just leaves the header showing
        // whatever it last had; the transcript itself still streams from
        // the SSE connection regardless.
      });
    return () => controller.abort();
  }, [sessionId, refreshOn]);

  return meta;
}
