import { useEffect, useState, useSyncExternalStore } from "react";
import type { SessionState } from "../api/types";
import { useTranscriptStore } from "../hooks";
import { BlockList } from "./BlockList";
import { PlanPanel } from "./PlanPanel";

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

  return (
    <div className="screen screen-transcript">
      <header className="screen-header">
        <button className="back-button" onClick={onBack}>
          ← sessions
        </button>
        <h1>{sessionId}</h1>
        <span className={`connection-badge connection-${snapshot.connection}`}>{snapshot.connection}</span>
      </header>
      {meta && (
        <div className="session-meta">
          <span className={`status-badge status-${meta.status}`}>{meta.status}</span>
          <span>
            {meta.model} ({meta.effort})
          </span>
          <span className="dim">{meta.workspace}</span>
          <span className="dim">{meta.permission_mode}</span>
          {meta.parent_id && (
            <span className="dim">
              forked from <code>{meta.parent_id}</code>
            </span>
          )}
        </div>
      )}
      <div className="transcript-layout">
        <BlockList blocks={snapshot.blocks} live={snapshot.live} />
        <PlanPanel todos={snapshot.todos} />
      </div>
    </div>
  );
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
