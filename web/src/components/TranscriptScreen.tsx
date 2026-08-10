import { useEffect, useState, useSyncExternalStore } from "react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import { useTranscriptStore } from "../hooks";
import { BlockList } from "./BlockList";
import { PlanPanel } from "./PlanPanel";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { outcome, type OutcomeSession } from "./statusBadge";

interface Props {
  sessionId: string;
  onBack: () => void;
}

export function TranscriptScreen({ sessionId, onBack }: Props) {
  const store = useTranscriptStore(sessionId);
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  // Re-fetch metadata when the stream closes, so the status badge picks up
  // the terminal status the transcript's own run_finished/error block
  // already shows below it, rather than freezing on whatever GET
  // /api/sessions/{id} returned back when the screen first mounted.
  const meta = useSessionMeta(sessionId, snapshot.connection);
  const badge = meta ? outcome(headerOutcomeSession(meta, snapshot.blocks)) : null;

  return (
    <div className="screen screen-transcript">
      <header className="screen-header">
        <Button variant="outline" size="sm" onClick={onBack}>
          ← sessions
        </Button>
        <h1>{sessionId}</h1>
        <Badge variant="outline" className={`connection-badge connection-${snapshot.connection}`}>
          {snapshot.connection}
        </Badge>
      </header>
      {meta && badge && (
        <div className="session-meta">
          <Badge variant={badge.variant}>{badge.label}</Badge>
          <span>
            {meta.model} ({meta.effort})
          </span>
          <span className="dim">{meta.workspace}</span>
          <span className="dim">{meta.permission_mode}</span>
          {meta.job_type && <span className="dim">{meta.job_type}</span>}
          {meta.parent_agent_type &&
            (meta.parent_agent_type === "user" ? (
              <span className="dim">started by a person</span>
            ) : (
              <span className="dim">
                started by {meta.parent_agent_type}
                {meta.parent_agent_id && ` (${meta.parent_agent_id})`}
              </span>
            ))}
          {meta.parent_id && (
            <span className="dim">
              forked from <code>{meta.parent_id}</code>
            </span>
          )}
        </div>
      )}
      <div className="transcript-layout">
        <BlockList items={snapshot.items} live={snapshot.live} />
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
