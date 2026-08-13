import { useEffect, useMemo, useSyncExternalStore } from "react";

import {
  buildComparison,
  formatDelta,
  formatMetric,
  groupMembersByTask,
  metricLabel,
  type ComparisonRow,
  type EvalMemberRow,
} from "../api/evals";
import { openEvalRunStream } from "../api/evalStreams";
import { Badge } from "./ui/badge";
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
  // comparison.
  const terminated = detail.members.filter(
    (m) => m.status !== "ok" && m.status !== "pending" && m.status !== "running",
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
            {detail.failed > 0 && ` · ${detail.failed} failed`}
          </Fact>
          <Fact label="Cost">${detail.cost_usd.toFixed(4)}</Fact>
          {detail.judge_model && <Fact label="Judge">{detail.judge_model}</Fact>}
          <Fact label="Status">
            <Badge variant={detail.status === "running" ? "running" : "outline"}>
              {detail.status.toUpperCase()}
            </Badge>
          </Fact>
        </dl>
      </header>

      <section className="eval-section">
        <h2>Comparison</h2>
        <div className="eval-table-scroll">
          <table className="session-table eval-comparison">
            <thead>
              <tr>
                <th>Metric</th>
                {detail.variants.map((v) => (
                  <th key={v}>{v}</th>
                ))}
                <th>Delta</th>
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
          ± is the standard error of the mean. A delta smaller than the two standard errors combined is
          not a result; add replicates.
        </p>
        {terminated.length > 0 && (
          <p className="eval-warning">
            {terminated.length} of {detail.total} runs did not finish cleanly (
            {[...new Set(terminated.map((m) => m.status))].join(", ")}). Their metrics stop where the
            run stopped, not where the work did — read this comparison with that in mind, or run it
            again with a larger budget.
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
                <th>Judge</th>
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

function ComparisonTableRow({ row }: { row: ComparisonRow }) {
  return (
    <tr>
      <th scope="row" className="eval-metric">
        {metricLabel(row.metric)}
      </th>
      {row.cells.map((cell, i) =>
        cell ? (
          <td key={i}>
            <span className="eval-mean">{formatMetric(row.metric, cell.mean)}</span>
            <span className="eval-spread">
              {cell.stderr === null ? "no spread" : `±${formatMetric(row.metric, cell.stderr)}`} (n=
              {cell.n})
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
    </tr>
  );
}

function MemberTableRow({
  member,
  taskLabel,
  onOpenSession,
}: {
  member: EvalMemberRow;
  taskLabel: string;
  onOpenSession: (sessionId: string) => void;
}) {
  return (
    <>
      <tr>
        <td className="eval-task">{taskLabel}</td>
        <td>{member.variant}</td>
        <td>{member.replicate}</td>
        <td>
          <Badge variant={member.error ? "failed" : "outline"}>{member.status}</Badge>
        </td>
        <td>{member.sub_turns || "—"}</td>
        {/* A member that has not run cost nothing because it has not
            happened, which is not the same as having been free. */}
        <td>{member.cost_usd > 0 ? `$${member.cost_usd.toFixed(4)}` : <span className="eval-absent">—</span>}</td>
        <td>{member.verdict ? `${member.verdict.score}/5` : <span className="eval-absent">—</span>}</td>
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
          <td colSpan={8}>{member.error}</td>
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
