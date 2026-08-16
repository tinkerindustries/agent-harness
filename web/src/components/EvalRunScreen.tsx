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
import { Broadcast } from "@phosphor-icons/react";
import { Badge } from "./ui/badge";
import { RTTh } from "./ui/ResponsiveTable";
import { cn } from "@/lib/utils";
import { useNavRight } from "./TopNav";

const tableCell = "border-b border-border px-2 py-1.5 align-top whitespace-nowrap";

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
      () => (
        <Badge variant={connected ? "running" : "outline"} title={connected ? "LIVE" : "OFFLINE"}>
          <Broadcast weight="bold" size={12} className={cn(connected && "dot-pulse")} />
          <span className="max-nav:sr-only">{connected ? "LIVE" : "OFFLINE"}</span>
        </Badge>
      ),
      [connected],
    ),
  );

  // An id that does not resolve leaves the stream closed and nothing to
  // show. Without this the screen sits on "Loading…" for ever.
  if (!detail) {
    return connected ? (
      <div className="screen">
        <p className="py-8 text-muted-foreground">Loading…</p>
      </div>
    ) : (
      <div className="screen">
        <p className="py-8 text-destructive">No eval run with id {id}. It may have been deleted.</p>
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
    <div className="screen pt-8">
      <header>
        <h1 className="m-0 text-[1.4rem]">{detail.suite}</h1>
        {detail.note && <p className="block text-[0.85em] text-muted-foreground">{detail.note}</p>}
        <dl className="mt-8 flex flex-wrap gap-8">
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

      <section className="mt-8">
        <h2 className="m-0 mb-6 text-base">Comparison</h2>
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-[0.9rem]">
            <thead>
              <tr>
                <RTTh>Metric</RTTh>
                {detail.variants.map((v) => (
                  <RTTh key={v}>{v}</RTTh>
                ))}
                <RTTh>Delta</RTTh>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <ComparisonTableRow key={row.metric} row={row} />
              ))}
            </tbody>
          </table>
        </div>
        <p className="mt-6 max-w-[62ch] text-[0.85em] text-muted-foreground">
          ± is the standard error of the mean. A delta smaller than the two standard errors combined is
          not a result; add replicates.
        </p>
        {terminated.length > 0 && (
          <p className="mt-6 max-w-[62ch] text-[0.85em] text-destructive">
            {terminated.length} of {detail.total} runs did not finish cleanly (
            {[...new Set(terminated.map((m) => m.status))].join(", ")}). Their metrics stop where the
            run stopped, not where the work did — read this comparison with that in mind, or run it
            again with a larger budget.
          </p>
        )}
      </section>

      <section className="mt-8">
        <h2 className="m-0 mb-6 text-base">Runs</h2>
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-[0.9rem]">
            <thead>
              <tr>
                <RTTh>Task</RTTh>
                <RTTh>Variant</RTTh>
                <RTTh>Rep</RTTh>
                <RTTh>Status</RTTh>
                <RTTh>Sub-turns</RTTh>
                <RTTh>Cost</RTTh>
                <RTTh>Judge</RTTh>
                <RTTh>Session</RTTh>
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
      <th
        scope="row"
        className={cn(tableCell, "text-left font-medium whitespace-nowrap text-muted-foreground max-phone:min-w-[11em]")}
      >
        {metricLabel(row.metric)}
      </th>
      {row.cells.map((cell, i) =>
        cell ? (
          <td key={i} className={tableCell}>
            <span className="block">{formatMetric(row.metric, cell.mean)}</span>
            <span className="block text-[0.8em] text-muted-foreground">
              {cell.stderr === null ? "no spread" : `±${formatMetric(row.metric, cell.stderr)}`} (n=
              {cell.n})
            </span>
          </td>
        ) : (
          // Absent, never zero: a run that never searched has no share, and a
          // 0% would claim it searched badly.
          <td key={i} className={cn(tableCell, "text-muted-foreground")}>
            —
          </td>
        ),
      )}
      <td className={tableCell}>
        {row.delta ? (
          <span className={cn("tabular-nums", row.delta.significant && "font-semibold")}>
            {formatDelta(row.metric, row.delta.diff)}
            {row.delta.significant && <span className="text-primary"> *</span>}
          </span>
        ) : (
          <span className="text-muted-foreground">—</span>
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
        <td className={cn(tableCell, "font-medium")}>{taskLabel}</td>
        <td className={tableCell}>{member.variant}</td>
        <td className={tableCell}>{member.replicate}</td>
        <td className={tableCell}>
          <Badge variant={member.error ? "failed" : "outline"}>{member.status}</Badge>
        </td>
        <td className={tableCell}>{member.sub_turns || "—"}</td>
        {/* A member that has not run cost nothing because it has not
            happened, which is not the same as having been free. */}
        <td className={tableCell}>
          {member.cost_usd > 0 ? `$${member.cost_usd.toFixed(4)}` : <span className="text-muted-foreground">—</span>}
        </td>
        <td className={tableCell}>
          {member.verdict ? `${member.verdict.score}/5` : <span className="text-muted-foreground">—</span>}
        </td>
        <td className={tableCell}>
          {member.session_id ? (
            <button
              className="cursor-pointer border-0 bg-transparent p-0 font-[inherit] text-primary underline"
              onClick={() => onOpenSession(member.session_id!)}
            >
              {member.session_id.slice(0, 16)}…
            </button>
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </td>
      </tr>
      {member.error && (
        <tr>
          <td colSpan={8} className={cn(tableCell, "pt-0 text-[0.85em] text-destructive")}>
            {member.error}
          </td>
        </tr>
      )}
    </>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-[0.8em] tracking-[0.04em] text-muted-foreground uppercase">{label}</dt>
      <dd className="mt-0.5">{children}</dd>
    </div>
  );
}
