import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { evalRunProgress, formatDelta, isRunning, metricLabel, type EvalRunRow } from "../api/evals";
import { openEvalListStream } from "../api/evalStreams";
import { controlToken, formatDuration } from "../api/operations";
import { EvalStartForm } from "./EvalStartForm";
import { useNow } from "../hooks";
import { Broadcast, Play } from "@phosphor-icons/react";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { RTBody, RTCell, RTHead, RTLead, RTRow, RTTable, RTTh } from "./ui/ResponsiveTable";
import { cn } from "@/lib/utils";
import { useNavRight } from "./TopNav";

// The eval list (docs/EVALS.md): every eval run over time, newest first, with
// what it compared and where it got to. The sessions an eval produced stay on
// the ordinary session list — this screen is about the comparison, not the
// runs.
//
// The list is pushed, not polled: the orchestrator runs in this process and
// wakes the hub after every write. Each frame is the whole list, so the
// render is a straight replace.

export function EvalListScreen({ onOpen }: { onOpen: (id: string) => void }) {
  const store = useMemo(() => openEvalListStream(), []);
  useEffect(() => () => store.close(), [store]);
  const runs = useSyncExternalStore(store.subscribe, store.snapshot);
  const connected = useSyncExternalStore(store.subscribe, store.connected);

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

  useNavRight(
    useMemo(
      () => (
        <>
          {tokenReady &&
            (token === null ? (
              <span className="text-muted-foreground">run control is not configured</span>
            ) : (
              <Button size="sm" onClick={() => setStartOpen((open) => !open)} title="Start an eval">
                <Play />
                <span className="max-nav:sr-only">Start an eval</span>
              </Button>
            ))}
          <Badge variant={connected ? "running" : "outline"} title={connected ? "LIVE" : "OFFLINE"}>
            <Broadcast weight="bold" size={12} className={cn(connected && "dot-pulse")} />
            <span className="max-nav:sr-only">{connected ? "LIVE" : "OFFLINE"}</span>
          </Badge>
        </>
      ),
      [connected, token, tokenReady],
    ),
  );

  const form = startOpen && token !== null && (
    <EvalStartForm
      token={token}
      priorRuns={runs ?? []}
      onClose={() => setStartOpen(false)}
      onStarted={(id) => {
        setStartOpen(false);
        onOpen(id);
      }}
    />
  );

  if (runs === null)
    return (
      <div className="screen">
        <p className="py-8 text-muted-foreground">Loading…</p>
      </div>
    );
  if (runs.length === 0) {
    return (
      <div className="screen pt-8">
        {form}
        <p className="py-8 text-muted-foreground">
          No eval runs yet. Start one above, or with{" "}
          <code>harness eval run -suite search -variants base,search-first</code>.
        </p>
      </div>
    );
  }

  return (
    <div className="screen pt-8">
      {form}
      <div className="overflow-x-auto">
        <RTTable>
          <RTHead>
            <tr>
              <RTTh>Status</RTTh>
              <RTTh>Eval</RTTh>
              <RTTh>Compared</RTTh>
              <RTTh>Runs</RTTh>
              <RTTh>Cost</RTTh>
              <RTTh>Elapsed</RTTh>
              <RTTh>Headline</RTTh>
            </tr>
          </RTHead>
          <RTBody>
            {runs.map((run) => (
              <EvalRow key={run.id} run={run} onOpen={onOpen} />
            ))}
          </RTBody>
        </RTTable>
      </div>
    </div>
  );
}

function EvalRow({ run, onOpen }: { run: EvalRunRow; onOpen: (id: string) => void }) {
  const now = useNow(isRunning(run) ? 1000 : 0);
  const started = new Date(run.started_at).getTime();
  const ended = run.finished_at ? new Date(run.finished_at).getTime() : now;

  return (
    <RTRow onClick={() => onOpen(run.id)}>
      <RTLead>
        <Badge variant={evalStatusVariant(run.status)}>{run.status.toUpperCase()}</Badge>
      </RTLead>
      <RTCell label="Eval" wide>
        <span className="block font-medium">{run.suite}</span>
        {run.note && <span className="block text-[0.85em] text-muted-foreground">{run.note}</span>}
      </RTCell>
      <RTCell label="Compared" wide>
        <span className="inline-flex flex-wrap gap-1">
          {run.variants.map((v) => (
            <Badge key={v} variant="outline">
              {v}
            </Badge>
          ))}
        </span>
      </RTCell>
      <RTCell label="Runs" wide>
        <span>
          {run.finished}/{run.total}
          {run.failed > 0 && <span className="text-destructive"> · {run.failed} failed</span>}
        </span>
        {isRunning(run) && (
          <span className="mt-[5px] block h-[5px] w-[6ch] overflow-hidden rounded-full bg-border" aria-hidden>
            <span className="block h-full bg-primary" style={{ width: `${Math.round(evalRunProgress(run) * 100)}%` }} />
          </span>
        )}
      </RTCell>
      <RTCell label="Cost" className="whitespace-nowrap">
        ${run.cost_usd.toFixed(run.cost_usd > 0 && run.cost_usd < 0.01 ? 4 : 2)}
      </RTCell>
      <RTCell label="Elapsed" className="whitespace-nowrap">
        {formatDuration(ended - started)}
      </RTCell>
      <RTCell label="Headline" wide>
        {run.headline ? (
          <span className={cn("tabular-nums", run.headline.significant && "font-semibold")}>
            {metricLabel(run.headline.metric)} {formatDelta(run.headline.metric, run.headline.diff)}
            {run.headline.significant && <span className="text-primary"> *</span>}
          </span>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </RTCell>
    </RTRow>
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
