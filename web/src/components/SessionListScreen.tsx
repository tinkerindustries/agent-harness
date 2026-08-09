import { useSyncExternalStore } from "react";
import { sessionListStore } from "../api/sessionListStore";
import type { QueueHealth, SessionState, Usage } from "../api/types";
import { useNow, useQueueHealth } from "../hooks";

function formatElapsed(sess: SessionState, nowMs: number): string {
  const start = Date.parse(sess.created_at);
  const end = sess.finished_at ? Date.parse(sess.finished_at) : nowMs;
  const totalSeconds = Math.max(0, Math.round((end - start) / 1000));
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  return `${m}:${s.toString().padStart(2, "0")}`;
}

function formatCost(usd: number): string {
  return `$${usd.toFixed(4)}`;
}

// costTitle is the tooltip on the Cost cell: the price table's own capture
// date, wherever a cost figure is shown (docs/DESIGN.md §4.9 — "a cost
// figure computed from a stale table is worse than no figure, because it
// looks authoritative").
function costTitle(sess: SessionState): string {
  return sess.price_table_date ? `price table captured ${sess.price_table_date}` : "price table date unknown";
}

// formatHitRate is shown next to the raw hit/miss token counts, never in
// place of them or of the per-turn churn diagnostic (rendered in the
// transcript) — a hit rate alone proves nothing about whether the prefix is
// healthy (docs/CACHE.md).
function formatHitRate(usage: Usage): string {
  const total = usage.cache_hit_tokens + usage.cache_miss_tokens;
  if (total === 0) return "—";
  return `${((usage.cache_hit_tokens / total) * 100).toFixed(1)}%`;
}

function hitRateTitle(usage: Usage): string {
  return `cache hit ${usage.cache_hit_tokens} / miss ${usage.cache_miss_tokens} tokens`;
}

interface Props {
  onOpen: (id: string) => void;
}

export function SessionListScreen({ onOpen }: Props) {
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);
  const now = useNow(1000);
  const queueHealth = useQueueHealth(5000);

  return (
    <div className="screen">
      <header className="screen-header">
        <h1>Sessions</h1>
        <span className={`connection-badge connection-${snapshot.connection}`}>{snapshot.connection}</span>
      </header>
      <QueueHealthBar health={queueHealth} />
      <table className="session-table">
        <thead>
          <tr>
            <th>Status</th>
            <th>Model</th>
            <th>Workspace</th>
            <th>Elapsed</th>
            <th>Sub-turns</th>
            <th>Cache hit</th>
            <th>Cost</th>
            <th>Request</th>
          </tr>
        </thead>
        <tbody>
          {snapshot.sessions.map((sess) => (
            <tr key={sess.id} className="session-row" onClick={() => onOpen(sess.id)}>
              <td>
                <span className={`status-badge status-${sess.status}`}>{sess.status}</span>
              </td>
              <td>
                {sess.model} <span className="dim">({sess.effort})</span>
              </td>
              <td className="workspace-cell" title={sess.workspace}>
                {sess.workspace}
              </td>
              <td>{formatElapsed(sess, now)}</td>
              <td>{sess.sub_turns}</td>
              <td className="dim" title={hitRateTitle(sess.usage)}>
                {formatHitRate(sess.usage)}
              </td>
              <td title={costTitle(sess)}>{formatCost(sess.usage.cost_usd)}</td>
              <td className="dim">{sess.request_id ?? "—"}</td>
            </tr>
          ))}
          {snapshot.sessions.length === 0 && (
            <tr>
              <td colSpan={8} className="empty-row">
                No sessions yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

// QueueHealthBar surfaces consumer lag, in-flight count, and redelivery
// count (PLAN.md phase 6), plus a halted state as an unmissable banner
// rather than another quiet figure — an operator watching the list is
// exactly who needs to know the pool stopped pulling work on an empty
// account (docs/DESIGN.md §4.5). Renders nothing for a CLI-only harness
// with no queue wired up (health.available === false, health.halted ===
// false) and nothing while the first poll is still in flight.
function QueueHealthBar({ health }: { health: QueueHealth | null }) {
  if (!health || (!health.available && !health.halted)) return null;
  return (
    <div className={`queue-health${health.halted ? " queue-health-halted" : ""}`}>
      {health.halted ? (
        <span>
          queue halted — {health.halt_reason || "reason unknown"}
        </span>
      ) : (
        <span>
          queue: {health.consumer_lag ?? 0} pending, {health.in_flight ?? 0} in flight, {health.redelivered ?? 0} redelivered
        </span>
      )}
      {health.error && <span className="dim"> ({health.error})</span>}
    </div>
  );
}
