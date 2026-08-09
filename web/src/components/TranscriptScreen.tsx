import { useEffect, useState, useSyncExternalStore } from "react";
import type { Block } from "../api/fold";
import type { SessionState } from "../api/types";
import { useTranscriptStore } from "../hooks";

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
    <div className="screen">
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
      <div className="transcript">
        {snapshot.blocks.map((block) => (
          <BlockView key={block.seq} block={block} />
        ))}
        {snapshot.blocks.length === 0 && <p className="empty-row">Waiting for the run to start…</p>}
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

function BlockView({ block }: { block: Block }) {
  switch (block.type) {
    case "opening":
      return (
        <section className="block block-opening">
          <div className="block-label">task</div>
          <p className="block-text">{block.text}</p>
        </section>
      );
    case "assistant":
      return (
        <section className="block block-assistant">
          <div className="block-label">sub-turn {block.subTurn}</div>
          {block.reasoning && (
            <details className="reasoning">
              <summary>reasoning ({block.reasoning.length} chars)</summary>
              <p className="block-text">{block.reasoning}</p>
            </details>
          )}
          {block.content && <p className="block-text">{block.content}</p>}
          {block.toolCalls.map((call) => (
            <div className="tool-call" key={call.id}>
              <code>
                {call.name}({call.arguments})
              </code>
            </div>
          ))}
        </section>
      );
    case "tool_result":
      return (
        <section className={`block block-tool-result${block.is_error ? " block-tool-error" : ""}`}>
          <div className="block-label">
            {block.name} {block.is_error && "(error)"} {block.truncated && "(truncated)"}
          </div>
          <pre className="block-pre">{block.content}</pre>
        </section>
      );
    case "tool_denied":
      return (
        <section className="block block-denied">
          <div className="block-label">denied: {block.name}</div>
          <p className="block-text">
            rule: <code>{block.rule}</code>
          </p>
          <p className="block-text">{block.content}</p>
        </section>
      );
    case "usage":
      return (
        <div className="block block-usage">
          prompt {block.prompt_tokens} (hit {block.prompt_cache_hit_tokens} / miss {block.prompt_cache_miss_tokens}), completion{" "}
          {block.completion_tokens}, cost ${block.cost_usd.toFixed(6)}
          {block.churn_point_index !== undefined && (
            <span className="churn-warning"> — churn at message {block.churn_point_index}</span>
          )}
        </div>
      );
    case "run_finished":
      return (
        <section className="block block-run-finished">
          <div className="block-label">run finished: {block.reason}</div>
          {block.summary && <p className="block-text">{block.summary}</p>}
          {block.text && <p className="block-text">{block.text}</p>}
        </section>
      );
    case "error":
      return (
        <section className="block block-error">
          <div className="block-label">error</div>
          <p className="block-text">{block.message}</p>
        </section>
      );
  }
}
