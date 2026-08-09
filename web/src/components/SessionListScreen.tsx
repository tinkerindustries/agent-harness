import { useSyncExternalStore } from "react";
import { sessionListStore } from "../api/sessionListStore";
import type { SessionState } from "../api/types";
import { useNow } from "../hooks";

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

interface Props {
  onOpen: (id: string) => void;
}

export function SessionListScreen({ onOpen }: Props) {
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);
  const now = useNow(1000);

  return (
    <div className="screen">
      <header className="screen-header">
        <h1>Sessions</h1>
        <span className={`connection-badge connection-${snapshot.connection}`}>{snapshot.connection}</span>
      </header>
      <table className="session-table">
        <thead>
          <tr>
            <th>Status</th>
            <th>Model</th>
            <th>Workspace</th>
            <th>Elapsed</th>
            <th>Sub-turns</th>
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
              <td>{formatCost(sess.usage.cost_usd)}</td>
              <td className="dim">{sess.request_id ?? "—"}</td>
            </tr>
          ))}
          {snapshot.sessions.length === 0 && (
            <tr>
              <td colSpan={7} className="empty-row">
                No sessions yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
