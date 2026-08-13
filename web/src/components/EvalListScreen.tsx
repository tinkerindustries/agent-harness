import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowsClockwise } from "@phosphor-icons/react";

import {
  evalRunProgress,
  formatDelta,
  isRunning,
  listEvalRuns,
  metricLabel,
  type EvalRunRow,
} from "../api/evals";
import { controlToken, formatDuration } from "../api/operations";
import { EvalStartForm } from "./EvalStartForm";
import { useNow } from "../hooks";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { useNavRight } from "./TopNav";

// The eval list (docs/EVALS.md): every eval run over time, newest first, with
// what it compared and where it got to. The sessions an eval produced stay on
// the ordinary session list — this screen is about the comparison, not the
// runs.
//
// A running run is polled rather than streamed. The orchestrator lives in the
// CLI process today, so the hub in `harness serve` has nothing to push; this
// is the one place in the frontend that polls, and it goes away when
// orchestration moves into serve.

const POLL_MS = 3000;

export function EvalListScreen({ onOpen }: { onOpen: (id: string) => void }) {
  const [runs, setRuns] = useState<EvalRunRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  // The start trigger lives in the nav's right slot and the form opens as a
  // card above the table, the shape the session list's start uses. A null
  // token means run control is not configured, and the nav says so rather
  // than offering a button that would 503.
  const [startOpen, setStartOpen] = useState(false);
  const [token, setToken] = useState<string | null>(null);
  const [tokenReady, setTokenReady] = useState(false);
  useEffect(() => {
    let cancelled = false;
    controlToken().then((t) => {
      if (cancelled) return;
      setToken(t);
      setTokenReady(true);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  const refresh = useCallback(async () => {
    try {
      setRuns(await listEvalRuns());
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const anyRunning = (runs ?? []).some(isRunning);
  useEffect(() => {
    if (!anyRunning) return;
    const timer = setInterval(() => void refresh(), POLL_MS);
    return () => clearInterval(timer);
  }, [anyRunning, refresh]);

  useNavRight(
    useMemo(
      () => (
        <>
          {tokenReady &&
            (token === null ? (
              <span className="eval-absent">run control is not configured</span>
            ) : (
              <Button size="sm" onClick={() => setStartOpen((open) => !open)}>
                Start an eval
              </Button>
            ))}
          <Button variant="ghost" size="sm" onClick={() => void refresh()} aria-label="Refresh">
            <ArrowsClockwise size={16} />
            Refresh
          </Button>
        </>
      ),
      [refresh, token, tokenReady],
    ),
  );

  const form = startOpen && token !== null && (
    <EvalStartForm
      token={token}
      priorRuns={runs ?? []}
      onClose={() => setStartOpen(false)}
      onStarted={(id) => {
        setStartOpen(false);
        void refresh();
        onOpen(id);
      }}
    />
  );

  if (error) return <p className="eval-error">{error}</p>;
  if (runs === null) return <p className="eval-empty">Loading…</p>;
  if (runs.length === 0) {
    return (
      <div className="eval-list">
        {form}
        <p className="eval-empty">
          No eval runs yet. Start one above, or with{" "}
          <code>harness eval run -suite search -variants base,search-first</code>.
        </p>
      </div>
    );
  }

  return (
    <div className="eval-list">
      {form}
      <table className="session-table">
        <thead>
          <tr>
            <th>Status</th>
            <th>Eval</th>
            <th>Compared</th>
            <th>Runs</th>
            <th>Cost</th>
            <th>Elapsed</th>
            <th>Headline</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <EvalRow key={run.id} run={run} onOpen={onOpen} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EvalRow({ run, onOpen }: { run: EvalRunRow; onOpen: (id: string) => void }) {
  const now = useNow(isRunning(run) ? 1000 : 0);
  const started = new Date(run.started_at).getTime();
  const ended = run.finished_at ? new Date(run.finished_at).getTime() : now;

  return (
    <tr className="eval-row" onClick={() => onOpen(run.id)}>
      <td>
        <Badge variant={evalStatusVariant(run.status)}>{run.status.toUpperCase()}</Badge>
      </td>
      <td>
        <span className="eval-suite">{run.suite}</span>
        {run.note && <span className="eval-note">{run.note}</span>}
      </td>
      <td>
        <span className="eval-variants">
          {run.variants.map((v) => (
            <Badge key={v} variant="outline">
              {v}
            </Badge>
          ))}
        </span>
      </td>
      <td>
        <span className="eval-runs">
          {run.finished}/{run.total}
          {run.failed > 0 && <span className="eval-failed"> · {run.failed} failed</span>}
        </span>
        {isRunning(run) && (
          <span className="eval-progress" aria-hidden>
            <span style={{ width: `${Math.round(evalRunProgress(run) * 100)}%` }} />
          </span>
        )}
      </td>
      <td>${run.cost_usd.toFixed(2)}</td>
      <td>{formatDuration(ended - started)}</td>
      <td>
        {run.headline ? (
          <span className={run.headline.significant ? "eval-headline is-significant" : "eval-headline"}>
            {metricLabel(run.headline.metric)} {formatDelta(run.headline.metric, run.headline.diff)}
            {run.headline.significant && <span className="eval-star"> *</span>}
          </span>
        ) : (
          <span className="eval-absent">—</span>
        )}
      </td>
    </tr>
  );
}

// evalStatusVariant reuses the session vocabulary so a badge means the same
// thing wherever it appears.
function evalStatusVariant(status: string): "running" | "done" | "failed" | "stopped" | "outline" {
  switch (status) {
    case "running":
      return "running";
    case "ok":
      return "done";
    case "failed":
      return "failed";
    case "cancelled":
      return "stopped";
    default:
      return "outline";
  }
}
