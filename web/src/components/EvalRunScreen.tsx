import { useEffect, useMemo, useSyncExternalStore } from "react";

import {
  buildComparison,
  evalRunProgress,
  formatDelta,
  formatMetric,
  groupMembersByTask,
  isRunning,
  metricLabel,
  type ComparisonRow,
  type EvalMemberRow,
} from "../api/evals";
import { formatDuration } from "../api/operations";
import { openEvalRunStream } from "../api/evalStreams";
import { Badge, statusBadgeVariant } from "./ui/badge";
import { useNavRight } from "./TopNav";

// One eval run (docs/EVALS.md): the header, the comparison, and the runs it
// is built from. The comparison is the point of the screen and leads; the
// per-run table below it is how a reader checks a number they do not believe.
//
// Every statistic comes from the server, computed by the same code the CLI's
// table uses. This screen formats and lays out; it decides nothing.

export function EvalRunScreen({ id, onOpenSession }: { id: string; onOpenSession: (sessionId: string) => void }) {
  // Per-screen rather than an app-lifetime singleton: a run stream is about
  // one page, and holding every run a session opened would be a connection
  // each.
  const store = useMemo(() => openEvalRunStream(id), [id]);
  useEffect(() => () => store.close(), [store]);
  const detail = useSyncExternalStore(store.subscribe, store.snapshot);
  const connected = useSyncExternalStore(store.subscribe, store.connected);

  useNavRight(
    useMemo(
      () => <Badge variant={connected ? "running" : "outline"}>{connected ? "LIVE" : "OFFLINE"}</Badge>,
      [connected],
    ),
  );

  // An id that does not resolve leaves the stream closed and nothing to
  // show. Without this the screen sits on "Loading…" for ever.
  if (!detail) {
    return connected ? (
      <div className="screen">
        <p className="eval-empty">Loading…</p>
      </div>
    ) : (
      <div className="screen">
        <p className="eval-error">No eval run with id {id}. It may have been deleted.</p>
      </div>
    );
  }

  const rows = buildComparison(detail);
  // Any member that did not end ok leaves its metrics stopping where the run
  // stopped rather than where the work did. Counting only max_turns missed a
  // run where all eight members timed out and the page called it a clean
  // comparison. Pending members are the queue, not a failure: while the run
  // is live they are simply not started yet, and only once the run is over —
  // a cancelled run above all — do they mean measurements that never
  // happened, so they count only then.
  const runOver = detail.status !== "running";
  const unfinished = detail.members.filter(
    (m) => m.status !== "ok" && m.status !== "running" && (m.status !== "pending" || runOver),
  );

  return (
    <div className="screen eval-run">
      <header className="eval-header">
        <h1>{detail.suite}</h1>
        {detail.note && <p className="eval-note">{detail.note}</p>}
        <dl className="eval-facts">
          <Fact label="Comparing">{detail.variants.join(" against ")}</Fact>
          <Fact label="Replicates">{String(detail.replicates)}</Fact>
          <Fact label="Runs">
            {detail.finished}/{detail.total}
            {detail.failed > 0 && <span className="eval-failed"> · {detail.failed} failed</span>}
            {isRunning(detail) && (
              <span className="eval-progress" aria-hidden>
                <span style={{ width: `${Math.round(evalRunProgress(detail) * 100)}%` }} />
              </span>
            )}
          </Fact>
          <Fact label="Cost">
            ${detail.cost_usd.toFixed(detail.cost_usd > 0 && detail.cost_usd < 0.01 ? 4 : 2)}
          </Fact>
          {detail.finished_at && (
            <Fact label="Duration">
              {formatDuration(new Date(detail.finished_at).getTime() - new Date(detail.started_at).getTime())}
            </Fact>
          )}
          {detail.judge_model && <Fact label="Judge">{detail.judge_model}</Fact>}
          <Fact label="Status">
            <Badge variant={statusBadgeVariant(detail.status)}>{detail.status.toUpperCase()}</Badge>
          </Fact>
        </dl>
      </header>

      <section className="eval-section">
        <h2>Comparison</h2>
        {detail.headline && (
          <p className={detail.headline.significant ? "eval-verdict is-significant" : "eval-verdict"}>
            {metricLabel(detail.headline.metric)} {formatDelta(detail.headline.metric, detail.headline.diff)}
            {detail.headline.significant && <span className="eval-star"> *</span>}
            {!detail.headline.significant && " — not a result"}
          </p>
        )}
        <div className="eval-table-scroll">
          <table className="session-table eval-comparison">
            <thead>
              <tr>
                <th>Metric</th>
                <th>Delta</th>
                {detail.variants.map((v) => (
                  <th key={v}>{v}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <ComparisonTableRow key={row.metric} row={row} />
              ))}
            </tbody>
          </table>
        </div>
        <p className="eval-caption">
          Delta is {detail.variants[detail.variants.length - 1]} minus {detail.variants[0]}. ± is the standard
          error of the mean. A delta smaller than the two standard errors combined is not a result; add
          replicates.
        </p>
        {unfinished.length > 0 && (
          <p className="eval-warning">
            {unfinishedClause(unfinished, detail.total)}. Their metrics are missing or stop where the run
            stopped, not where the work did — read this comparison with that in mind, or run it again.
          </p>
        )}
      </section>

      <section className="eval-section">
        <h2>Runs</h2>
        <div className="eval-table-scroll">
          <table className="session-table">
            <thead>
              <tr>
                <th>Task</th>
                <th>Variant</th>
                <th>Rep</th>
                <th>Status</th>
                <th>Sub-turns</th>
                <th>Cost</th>
                {detail.judge_model && <th>Judge</th>}
                <th>Session</th>
              </tr>
            </thead>
            <tbody>
              {groupMembersByTask(detail.members).map((group) =>
                group.members.map((m, i) => (
                  <MemberTableRow
                    key={m.request_id}
                    member={m}
                    taskLabel={i === 0 ? group.taskId : ""}
                    judge={Boolean(detail.judge_model)}
                    onOpenSession={onOpenSession}
                  />
                )),
              )}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}

// unfinishedClause says which members did not count, in the run's own
// vocabulary: the header's finished count includes members that ran to a
// terminal state, failed ones among them, so a never-started member is a
// separate fact from a failed one.
function unfinishedClause(members: EvalMemberRow[], total: number): string {
  const failed = members.filter((m) => m.status === "failed").length;
  const pending = members.filter((m) => m.status === "pending").length;
  const other = members.length - failed - pending;
  const parts: string[] = [];
  if (pending > 0) parts.push(`${pending} of ${total} runs never started`);
  if (failed === 1) parts.push("1 run failed");
  else if (failed > 1) parts.push(`${failed} runs failed`);
  if (other === 1) parts.push("1 run ended abnormally");
  else if (other > 1) parts.push(`${other} runs ended abnormally`);
  return parts.join(" and ");
}

function ComparisonTableRow({ row }: { row: ComparisonRow }) {
  return (
    <tr>
      <th scope="row" className="eval-metric">
        {metricLabel(row.metric)}
      </th>
      {/* The delta is the answer to the screen's question, so it sits next to
          the metric rather than at the far end of the row: at a narrow
          viewport it is the one column a reader must never have to scroll
          to. The direction is in the caption. */}
      <td>
        {row.delta ? (
          <span className={row.delta.significant ? "eval-delta is-significant" : "eval-delta"}>
            {formatDelta(row.metric, row.delta.diff)}
            {row.delta.significant && <span className="eval-star"> *</span>}
          </span>
        ) : (
          <span className="eval-absent">—</span>
        )}
      </td>
      {row.cells.map((cell, i) =>
        cell ? (
          <td key={i}>
            <span className="eval-mean">{formatMetric(row.metric, cell.mean)}</span>
            <span className="eval-spread">
              {cell.stderr === null ? "no spread" : `±${formatMetric(row.metric, cell.stderr)}`} (n={cell.n})
            </span>
          </td>
        ) : (
          // Absent, never zero: a run that never searched has no share, and a
          // 0% would claim it searched badly.
          <td key={i} className="eval-absent">
            —
          </td>
        ),
      )}
    </tr>
  );
}

function MemberTableRow({
  member,
  taskLabel,
  judge,
  onOpenSession,
}: {
  member: EvalMemberRow;
  taskLabel: string;
  judge: boolean;
  onOpenSession: (sessionId: string) => void;
}) {
  return (
    <>
      <tr>
        <td className="eval-task">{taskLabel}</td>
        <td>{member.variant}</td>
        <td>{member.replicate}</td>
        <td>
          <Badge variant={statusBadgeVariant(member.status)}>{member.status.toUpperCase()}</Badge>
        </td>
        <td>{member.sub_turns || "—"}</td>
        {/* A member that has not run cost nothing because it has not
            happened, which is not the same as having been free. */}
        <td>{member.cost_usd > 0 ? `$${member.cost_usd.toFixed(4)}` : <span className="eval-absent">—</span>}</td>
        {judge && <td>{member.verdict ? `${member.verdict.score}/5` : <span className="eval-absent">—</span>}</td>}
        <td>
          {member.session_id ? (
            <button className="eval-session-link" onClick={() => onOpenSession(member.session_id!)}>
              {member.session_id.slice(0, 16)}…
            </button>
          ) : (
            <span className="eval-absent">—</span>
          )}
        </td>
      </tr>
      {member.error && (
        <tr className="eval-error-row">
          <td colSpan={judge ? 8 : 7}>{member.error}</td>
        </tr>
      )}
    </>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="eval-fact">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}
