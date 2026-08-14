import { useCallback, useEffect, useState } from "react";
import { ArrowsClockwise, LockOpen, Trash } from "@phosphor-icons/react";
import {
  closeSession,
  closeWorkRequest,
  deleteSession,
  deleteWorkRequest,
  errorMessage,
  fetchLastEventAt,
  formatDuration,
  isStuckSession,
  listLeases,
  listSessions,
  listWorkRequests,
  quietMs,
  releaseLease,
} from "../api/operations";
import type { WorkRequestRow, WorkspaceLeaseRow } from "../api/operations";
import type { SessionState } from "../api/types";
import { useNow } from "../hooks";
import { Button } from "./ui/button";
import { useNavRight } from "./TopNav";

// The operations screen (docs/DATA-API.md): the one place the
// browser acts on the harness. Three sections — stuck sessions, running
// work requests, and workspace leases — each offering the writes that
// rewrite operational state: Close (PATCH to a terminal status), Delete,
// and Release. Destructive controls live here rather than on the session
// list or transcript, deliberately: closing a run is something an operator
// went to this screen to do.
//
// The discipline, copied from the settings screen (web/CLAUDE.md):
//   - Every write echoes the row's version in If-Match. A 412 means someone
//     else changed the row — the screen re-fetches and shows the server's
//     message (which names the current version); it never retries silently
//     with a fresher one.
//   - A refused write shows the server's own words. A 409 names the
//     evidence — the last event, the last heartbeat — and the screen renders
//     that text, not a generic failure.
//   - After a write the screen re-fetches rather than guessing at the new
//     state, which is why only one write is in flight at a time (everything
//     is disabled while one runs).
//   - A delete is confirmed inline on the row, naming what is about to be
//     destroyed. No window.confirm: a modal blocks the page and the
//     repository's automation cannot dismiss it.
//
// Stop is not offered here: it lives on the in-flight session card and the
// transcript header, where an operator sees the run it would end
// (docs/RUN-CONTROL.md "The frontend").

// RunningSession is one still-running session row plus the quiet signal the
// row itself does not carry: its most recent event's time, read from the
// tail of the event log (or null when the log is empty).
interface RunningSession {
  session: SessionState;
  lastEventAt: string | null;
}

interface OpsData {
  running: RunningSession[];
  // requests holds only running rows — the ones this screen exists to close.
  requests: WorkRequestRow[];
  leases: WorkspaceLeaseRow[];
}

// parseStamp turns an ISO timestamp into ms since epoch, or null for a
// missing or unparseable value — a malformed timestamp renders as "no
// events" rather than NaN.
function parseStamp(iso: string | null): number | null {
  if (iso === null) return null;
  const ms = Date.parse(iso);
  return Number.isNaN(ms) ? null : ms;
}

function statusOf(err: unknown): number | undefined {
  return (err as { status?: number }).status;
}

// formatStamp renders an absolute timestamp: the time an event or heartbeat
// happened, full precision for an operator deciding whether a row is dead.
function formatStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

// rowKey is the stable identity a row's UI state hangs on (busy, confirm,
// error). A lease is keyed by its workspace, which is unique in the table.
type RowKey =
  | { kind: "session"; id: string }
  | { kind: "request"; id: string }
  | { kind: "lease"; workspace: string };

function keyOf(row: RowKey): string {
  return row.kind === "lease" ? `lease:${row.workspace}` : `${row.kind}:${row.id}`;
}

export function OperationsScreen() {
  const [data, setData] = useState<OpsData | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // One write in flight at a time: the screen re-fetches after every write,
  // and two concurrent writes would race each other's re-fetch. busy
  // disables every control on the screen while a write runs.
  const [busy, setBusy] = useState(false);
  // confirming is the key of the row whose delete/release is awaiting its
  // second click — the inline confirm step that names what will be removed.
  const [confirming, setConfirming] = useState<string | null>(null);
  // errors holds the server's own message from the last failed write, one
  // per row: a 409's evidence, a 412's current version, a 404's absence.
  const [errors, setErrors] = useState<Record<string, string>>({});
  const now = useNow(1000);

  // refresh re-fetches the whole screen from the server. Sessions come from
  // GET /api/sessions filtered server-side: this screen only ever wants the
  // running rows, so it asks for ?status=running at the largest page the
  // server accepts rather than pulling every session and filtering
  // client-side (docs/DATA-API.md "Pagination"). Work requests come from
  // their own list endpoint (docs/DATA-API.md): every work_requests
  // row, newest first, each carrying the version a write echoes back in
  // If-Match. Listing the rows directly — rather than walking the sessions
  // each one produced — is what makes a request whose worker died during
  // workspace preparation visible: it never got a session, so it has no
  // session to be found through, only its row. The screen shows the running
  // ones. A running session's quiet time comes from the tail of its event
  // log, which is read-only and paged exactly like the transcript reads it.
  const refresh = useCallback(async () => {
    try {
      const [sessions, requests, leases] = await Promise.all([
        listSessions({ status: "running", limit: 200 }),
        listWorkRequests(),
        listLeases(),
      ]);

      const running = (
        await Promise.all(
          sessions.items.map(async (session) => {
              try {
                return { session, lastEventAt: await fetchLastEventAt(session.id) };
              } catch (err) {
                if (statusOf(err) === 404) return null; // the session vanished mid-read
                throw err;
              }
            }),
        )
      ).filter((r): r is RunningSession => r !== null);

      setData({ running, requests: requests.filter((r) => r.status === "running"), leases });
      setLoadError(null);
    } catch (err) {
      setLoadError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // stuck is recomputed on every render so a ticking now keeps both the
  // classification and the quiet durations current: a session stops being
  // "stuck" the moment its last event crosses back inside the threshold.
  const stuck = useStuckRows(data, now);

  // runWrite is every write's single path: mark the screen busy, clear the
  // row's old error, run the write, and on success re-fetch rather than
  // guessing at the new state. A 412 — someone else changed the row — also
  // re-fetches, so the screen shows the current versions, and the server's
  // message (which names the current version) stays visible under the row.
  // The write is never retried with a fresher version. A 404 means the row
  // is already gone (a delete landed elsewhere), which is the same staleness
  // and gets the same re-fetch.
  async function runWrite(key: string, write: () => Promise<void>) {
    if (busy) return;
    setBusy(true);
    setErrors((prev) => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
    try {
      await write();
      setConfirming(null);
      await refresh();
    } catch (err) {
      setErrors((prev) => ({ ...prev, [key]: errorMessage(err) }));
      if (statusOf(err) === 412 || statusOf(err) === 404) {
        await refresh();
      }
    } finally {
      setBusy(false);
    }
  }

  function closeSessionRow(s: SessionState) {
    runWrite(keyOf({ kind: "session", id: s.id }), () => closeSession(s.id, s.version, "cancelled"));
  }

  function deleteSessionRow(s: SessionState) {
    runWrite(keyOf({ kind: "session", id: s.id }), () => deleteSession(s.id, s.version));
  }

  function closeRequestRow(r: WorkRequestRow) {
    runWrite(keyOf({ kind: "request", id: r.request_id }), () =>
      closeWorkRequest(r.request_id, r.version, "cancelled"),
    );
  }

  function deleteRequestRow(r: WorkRequestRow) {
    runWrite(keyOf({ kind: "request", id: r.request_id }), () => deleteWorkRequest(r.request_id, r.version));
  }

  function releaseLeaseRow(l: WorkspaceLeaseRow) {
    runWrite(keyOf({ kind: "lease", workspace: l.workspace }), () => releaseLease(l.workspace, l.version));
  }

  // The nav's right slot for this screen: the refresh button.
  useNavRight(
    <Button variant="outline" size="sm" onClick={refresh} disabled={busy || data === null}>
      <ArrowsClockwise />
      Refresh
    </Button>,
  );

  return (
    <div className="screen">
      {loadError && (
        <div className="ops-error ops-error-banner">could not load operations: {loadError}</div>
      )}
      {data === null && !loadError && <p className="dim">Loading operations…</p>}

      {data && (
        <>
          <div className="ops-strip">
            <span>
              <b>{stuck.length}</b> stuck session{stuck.length === 1 ? "" : "s"}
            </span>
            <span className="sep">·</span>
            <span>
              <b>{data.requests.length}</b> running work request{data.requests.length === 1 ? "" : "s"}
            </span>
            <span className="sep">·</span>
            <span>
              <b>{data.leases.length}</b> workspace lease{data.leases.length === 1 ? "" : "s"}
            </span>
            <span className="spacer" />
            <span>“quiet” means no event — or no heartbeat — for more than 10 minutes</span>
          </div>

          <section className="list-section">
            <div className="section-head">
              <h2>Stuck sessions</h2>
              <span className="count">
                {stuck.length} session{stuck.length === 1 ? "" : "s"} still running, quiet past the idle
                threshold
              </span>
            </div>
            {stuck.length === 0 ? (
              <p className="dim ops-empty">No stuck sessions.</p>
            ) : (
              <div className="table-scroll">
                <table className="ops-table">
                  <thead>
                    <tr>
                      <th>Session</th>
                      <th>Quiet</th>
                      <th>Last event</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {stuck.map(({ session, lastEventAt }) => (
                      <StuckSessionRow
                        key={session.id}
                        session={session}
                        lastEventAt={lastEventAt}
                        now={now}
                        busy={busy}
                        confirming={confirming === keyOf({ kind: "session", id: session.id })}
                        error={errors[keyOf({ kind: "session", id: session.id })] ?? null}
                        onClose={() => closeSessionRow(session)}
                        onDelete={() => setConfirming(keyOf({ kind: "session", id: session.id }))}
                        onConfirmDelete={() => deleteSessionRow(session)}
                        onCancel={() => setConfirming(null)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="list-section">
            <div className="section-head">
              <h2>Work requests</h2>
              <span className="count">
                {data.requests.length} running request{data.requests.length === 1 ? "" : "s"}
              </span>
            </div>
            {data.requests.length === 0 ? (
              <p className="dim ops-empty">No running work requests.</p>
            ) : (
              <div className="table-scroll">
                <table className="ops-table">
                  <thead>
                    <tr>
                      <th>Request</th>
                      <th>Session</th>
                      <th>Deliveries</th>
                      <th>Received</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {data.requests.map((r) => (
                      <WorkRequestRowView
                        key={r.request_id}
                        row={r}
                        busy={busy}
                        confirming={confirming === keyOf({ kind: "request", id: r.request_id })}
                        error={errors[keyOf({ kind: "request", id: r.request_id })] ?? null}
                        onClose={() => closeRequestRow(r)}
                        onDelete={() => setConfirming(keyOf({ kind: "request", id: r.request_id }))}
                        onConfirmDelete={() => deleteRequestRow(r)}
                        onCancel={() => setConfirming(null)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="list-section">
            <div className="section-head">
              <h2>Workspace leases</h2>
              <span className="count">
                {data.leases.length} lease{data.leases.length === 1 ? "" : "s"}
              </span>
            </div>
            {data.leases.length === 0 ? (
              <p className="dim ops-empty">No leases.</p>
            ) : (
              <div className="table-scroll">
                <table className="ops-table">
                  <thead>
                    <tr>
                      <th>Workspace</th>
                      <th>Session</th>
                      <th>Acquired</th>
                      <th>Last heartbeat</th>
                      <th>Quiet</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {data.leases.map((l) => (
                      <LeaseRow
                        key={l.workspace}
                        row={l}
                        now={now}
                        busy={busy}
                        confirming={confirming === keyOf({ kind: "lease", workspace: l.workspace })}
                        error={errors[keyOf({ kind: "lease", workspace: l.workspace })] ?? null}
                        onRelease={() => setConfirming(keyOf({ kind: "lease", workspace: l.workspace }))}
                        onConfirmRelease={() => releaseLeaseRow(l)}
                        onCancel={() => setConfirming(null)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}

// useStuckRows derives the stuck-session list from the raw data and a
// ticking now: running sessions whose last event is absent or older than
// the idle threshold (the module's isStuckSession, the same rule the
// server's close precondition applies).
function useStuckRows(data: OpsData | null, now: number): RunningSession[] {
  if (!data) return [];
  return data.running.filter(({ session, lastEventAt }) =>
    isStuckSession(session.status, parseStamp(lastEventAt), now),
  );
}

// quietCell renders the "how long has it been quiet" headline for a row: the
// duration since its last activity, or the honest absence of one.
function quietCell(lastActivityAtMs: number | null, now: number): string {
  const quiet = quietMs(lastActivityAtMs, now);
  return quiet === null ? "no events yet" : formatDuration(quiet);
}

interface StuckSessionRowProps {
  session: SessionState;
  lastEventAt: string | null;
  now: number;
  busy: boolean;
  confirming: boolean;
  error: string | null;
  onClose: () => void;
  onDelete: () => void;
  onConfirmDelete: () => void;
  onCancel: () => void;
}

function StuckSessionRow({
  session,
  lastEventAt,
  now,
  busy,
  confirming,
  error,
  onClose,
  onDelete,
  onConfirmDelete,
  onCancel,
}: StuckSessionRowProps) {
  return (
    <tr className="ops-row">
      <td>
        <div className="sess-cell">
          <span className="sess-id">{session.id}</span>
          <span className="sess-sub truncate">
            {session.workspace}
            {session.model && <> · {session.model}</>}
          </span>
        </div>
      </td>
      <td className="ops-quiet">{quietCell(parseStamp(lastEventAt), now)}</td>
      <td className="dim">{lastEventAt ? formatStamp(lastEventAt) : "no events"}</td>
      <td className="ops-actions-cell">
        {confirming ? (
          <div className="ops-confirm">
            <span>
              Delete session <code>{session.id}</code> and its entire event log? This cannot be undone.
            </span>
            <div className="ops-confirm-buttons">
              <Button variant="destructive" size="sm" onClick={onConfirmDelete} disabled={busy}>
                <Trash />
                Delete
              </Button>
              <Button variant="outline" size="sm" onClick={onCancel} disabled={busy}>
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <div className="ops-actions">
            <Button variant="outline" size="sm" onClick={onClose} disabled={busy}>
              Close
            </Button>
            <Button variant="destructive" size="sm" onClick={onDelete} disabled={busy}>
              <Trash />
              Delete
            </Button>
          </div>
        )}
        {error && <span className="field-error">{error}</span>}
      </td>
    </tr>
  );
}

interface WorkRequestRowProps {
  row: WorkRequestRow;
  busy: boolean;
  confirming: boolean;
  error: string | null;
  onClose: () => void;
  onDelete: () => void;
  onConfirmDelete: () => void;
  onCancel: () => void;
}

function WorkRequestRowView({
  row,
  busy,
  confirming,
  error,
  onClose,
  onDelete,
  onConfirmDelete,
  onCancel,
}: WorkRequestRowProps) {
  return (
    <tr className="ops-row">
      <td className="ops-mono">{row.request_id}</td>
      <td className="ops-mono dim">{row.session_id ?? "—"}</td>
      <td>{row.delivery_count}</td>
      <td className="dim">{formatStamp(row.received_at)}</td>
      <td className="ops-actions-cell">
        {confirming ? (
          <div className="ops-confirm">
            <span>
              Delete work request <code>{row.request_id}</code> and its row? This cannot be undone.
            </span>
            <div className="ops-confirm-buttons">
              <Button variant="destructive" size="sm" onClick={onConfirmDelete} disabled={busy}>
                <Trash />
                Delete
              </Button>
              <Button variant="outline" size="sm" onClick={onCancel} disabled={busy}>
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <div className="ops-actions">
            <Button variant="outline" size="sm" onClick={onClose} disabled={busy}>
              Close
            </Button>
            <Button variant="destructive" size="sm" onClick={onDelete} disabled={busy}>
              <Trash />
              Delete
            </Button>
          </div>
        )}
        {error && <span className="field-error">{error}</span>}
      </td>
    </tr>
  );
}

interface LeaseRowProps {
  row: WorkspaceLeaseRow;
  now: number;
  busy: boolean;
  confirming: boolean;
  error: string | null;
  onRelease: () => void;
  onConfirmRelease: () => void;
  onCancel: () => void;
}

function LeaseRow({ row, now, busy, confirming, error, onRelease, onConfirmRelease, onCancel }: LeaseRowProps) {
  return (
    <tr className="ops-row">
      <td className="ops-mono">{row.workspace}</td>
      <td className="ops-mono dim">{row.session_id}</td>
      <td className="dim">{formatStamp(row.acquired_at)}</td>
      <td className="dim">{formatStamp(row.heartbeat_at)}</td>
      <td className="ops-quiet">{quietCell(parseStamp(row.heartbeat_at), now)}</td>
      <td className="ops-actions-cell">
        {confirming ? (
          <div className="ops-confirm">
            <span>
              Release the lease on <code>{row.workspace}</code>? Its row is removed.
            </span>
            <div className="ops-confirm-buttons">
              <Button variant="destructive" size="sm" onClick={onConfirmRelease} disabled={busy}>
                <LockOpen />
                Release
              </Button>
              <Button variant="outline" size="sm" onClick={onCancel} disabled={busy}>
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <div className="ops-actions">
            <Button variant="destructive" size="sm" onClick={onRelease} disabled={busy}>
              <LockOpen />
              Release
            </Button>
          </div>
        )}
        {error && <span className="field-error">{error}</span>}
      </td>
    </tr>
  );
}
