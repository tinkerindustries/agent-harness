import { useRef, useState } from "react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptFilter } from "../api/groups";
import type { TranscriptSnapshot } from "../api/transcriptStore";
import { TurnTranscript } from "./turns/TurnTranscript";
import { PlanPanel } from "./PlanPanel";
import { TimelineRail } from "./TimelineRail";
import { TranscriptToolbar } from "./TranscriptToolbar";
import { StopControl } from "./StopControl";
import { SteerControl } from "./SteerControl";
import { Badge } from "./ui/badge";
import { outcome, type OutcomeSession } from "./statusBadge";
import { startedBy } from "../api/provenance";
import { useNavRight } from "./TopNav";

interface Props {
  sessionId: string;
  // The metadata row the fork read before choosing this screen — null only
  // when the row's fetch failed and the fork kept this screen as the safe
  // default (SessionScreen).
  meta: SessionState | null;
  snapshot: TranscriptSnapshot;
}

// The interactive session page (design/session-chat.html): a run a person
// started, inside the full-height app shell the two session pages share
// (design/session.css). Phase 2 renders the conversation as turns — one
// .turn per sub-turn, tool rows that collapse individually, no density
// mode — with the filter chips, the timeline rail and the plan panel still
// mounted where they were; phase 3 builds the composer and the plan rail,
// phase 4 moves the chips into the watch page's rail.
export function SessionChatScreen({ sessionId, meta, snapshot }: Props) {
  const badge = meta ? outcome(headerOutcomeSession(meta, snapshot.blocks)) : null;
  // The one-line provenance label ("started by geoff", "started by
  // claude-code (sess-1)"), rendered only when the row carries provenance at
  // all (web/src/api/provenance.ts owns the legacy fallback).
  const startedByLabel = meta ? startedBy(meta) : null;

  // The filter starts at All; it is a plain string, so the memoised turn
  // list compares it by value and still bails out on every live-only delta;
  // toggling a chip is the one deliberate re-render.
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
    <div className="work">
      <main className="stream">
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
          <TranscriptToolbar filter={filter} onFilterChange={setFilter} counts={snapshot.counts} />
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
              <TurnTranscript
                items={snapshot.items}
                live={snapshot.live}
                filter={filter}
                getToolCall={snapshot.getToolCall}
              />
            </div>
            <PlanPanel todos={snapshot.todos} />
          </div>
        </div>
      </main>
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
