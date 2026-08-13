// The eval client: the fetch calls, the wire types, and the pure formatting
// the eval screens are built from (docs/EVALS.md). Kept separate from the
// components like operations.ts and fold.ts — the components render, this
// talks to the server, and the tests pin the shapes and the number formatting
// without any DOM.
//
// No statistics happen here. The server computes every mean, standard error
// and delta through the same code the CLI's table uses, and decides whether a
// difference is worth looking at. This module formats what it is given.

// EvalDelta is one metric's comparison of the last variant against the first.
// significant is the server's decision, not a threshold to re-derive here.
export interface EvalDelta {
  metric: string;
  baseline_variant: string;
  variant: string;
  diff: number;
  combined_stderr: number;
  significant: boolean;
}

// EvalSummary is one metric under one variant, across every run of it.
export interface EvalSummary {
  metric: string;
  variant: string;
  n: number;
  mean: number;
  stddev: number;
  stderr: number;
}

export interface EvalVerdict {
  score: number;
  completed: boolean;
  reasoning: string;
}

export interface EvalMemberRow {
  request_id: string;
  task_id: string;
  variant: string;
  replicate: number;
  session_id?: string;
  status: string;
  scores?: Record<string, number>;
  verdict?: EvalVerdict;
  cost_usd: number;
  sub_turns: number;
  error?: string;
}

export interface EvalRunRow {
  id: string;
  suite: string;
  note?: string;
  variants: string[];
  replicates: number;
  judge_model?: string;
  status: string;
  started_at: string;
  finished_at?: string;
  total: number;
  finished: number;
  failed: number;
  cost_usd: number;
  headline?: EvalDelta;
  version: number;
}

export interface EvalRunDetail extends EvalRunRow {
  members: EvalMemberRow[];
  summary: EvalSummary[];
  deltas: EvalDelta[];
}

export interface EvalMembership {
  eval_run_id: string;
  suite: string;
  task_id: string;
  variant: string;
  replicate: number;
  status: string;
}

export interface EvalVariantRow {
  name: string;
  description: string;
  reminder?: string;
}

export interface EvalSuiteRow {
  name: string;
  description?: string;
  rubric?: string;
  task_ids: string[];
}

// EvalSpec is what POST /api/evals takes, mirroring evals.Spec. suite names a
// built-in; a suite from a file is the CLI's business and is posted inline
// there, so the browser only ever names one this build already has.
export interface EvalSpec {
  suite: string;
  variants: string[];
  replicates: number;
  concurrency?: number;
  max_sub_turns?: number;
  model?: string;
  effort?: string;
  judge?: boolean;
  judge_model?: string;
  note?: string;
}

// --- reads ---

export async function listEvalRuns(): Promise<EvalRunRow[]> {
  const res = await fetch("/api/evals");
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()) as EvalRunRow[];
}

export async function getEvalRun(id: string): Promise<EvalRunDetail> {
  const res = await fetch(`/api/evals/${encodeURIComponent(id)}`);
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()) as EvalRunDetail;
}

export async function listEvalVariants(): Promise<EvalVariantRow[]> {
  const res = await fetch("/api/evals/variants");
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()) as EvalVariantRow[];
}

export async function listEvalSuites(): Promise<EvalSuiteRow[]> {
  const res = await fetch("/api/evals/suites");
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()) as EvalSuiteRow[];
}

// --- writes ---
//
// Starting and cancelling are run control and carry the bearer token, the
// rule docs/RUN-CONTROL.md sets for anything that spends money. The server's
// own message is what a refusal shows: a 409 says an eval is already in
// flight, and the screen renders that rather than a generic failure.

export async function startEval(token: string, spec: EvalSpec): Promise<string> {
  const res = await fetch("/api/evals", {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify(spec),
  });
  const body = (await res.json().catch(() => ({}))) as { eval_run_id?: string; error?: string };
  if (!res.ok) throw new Error(body.error ?? res.statusText);
  return body.eval_run_id ?? "";
}

export async function cancelEval(token: string, id: string): Promise<void> {
  const res = await fetch(`/api/evals/${encodeURIComponent(id)}/cancel`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: "{}",
  });
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error ?? res.statusText);
  }
}

// getSessionEval answers "which eval does this session belong to?". A session
// in no eval is a 404, which is every ordinary session, so this returns null
// rather than throwing — the caller renders nothing.
export async function getSessionEval(sessionId: string): Promise<EvalMembership | null> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/eval`);
  if (res.status === 404) return null;
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()) as EvalMembership;
}

// --- formatting ---

// formatMetric mirrors internal/evals/report.go's format, so a number reads
// the same in the browser as in the terminal: shares as percentages, money to
// four places, context in thousands, everything else to two.
export function formatMetric(metric: string, value: number): string {
  if (
    metric.endsWith("_rate") ||
    metric.endsWith("_via_tool") ||
    metric.endsWith("_decay") ||
    metric === "judge_completed"
  ) {
    return `${(value * 100).toFixed(1)}%`;
  }
  if (metric === "cost_usd") return `$${value.toFixed(4)}`;
  if (metric === "context_tokens_max") return `${Math.round(value / 1000)}k`;
  return value.toFixed(2);
}

// formatDelta signs the difference, because the direction is the point. An
// exact zero is words rather than "0.0%", which beside a metric name reads as
// the value rather than the change.
export function formatDelta(metric: string, diff: number): string {
  if (diff === 0) return "no change";
  const body = formatMetric(metric, Math.abs(diff));
  return diff > 0 ? `+${body}` : `-${body}`;
}

// metricLabel is the human name for a metric. An unknown one — a metric added
// to the Go side and not here — falls back to its own key rather than
// disappearing from the table.
const METRIC_LABELS: Record<string, string> = {
  search_via_tool: "Searches via Grep/Glob",
  search_via_tool_decay: "…decay across the window",
  searches_total: "Searches",
  tool_error_rate: "Tool errors",
  read_before_edit_misses: "Edits refused, unread file",
  edit_miss_rate_decay: "…decay across the window",
  tool_calls: "Tool calls",
  context_tokens_max: "Largest request",
  judge_score: "Judge score",
  judge_completed: "Judged complete",
  sub_turns: "Sub-turns",
  cost_usd: "Cost",
};

export function metricLabel(metric: string): string {
  return METRIC_LABELS[metric] ?? metric;
}

// A cell is one (metric, variant) as the table renders it, or absent. A
// metric with nothing to measure under a variant must render as a dash and
// never as a zero: the server omits the summary row rather than sending
// mean 0, and that distinction has to survive to the screen.
export interface ComparisonCell {
  mean: number;
  stderr: number;
  n: number;
}

export interface ComparisonRow {
  metric: string;
  cells: (ComparisonCell | null)[];
  delta: EvalDelta | null;
}

// buildComparison arranges the server's summary into one row per metric and
// one cell per variant, in the server's own metric order.
export function buildComparison(detail: EvalRunDetail): ComparisonRow[] {
  const byMetric = new Map<string, Map<string, EvalSummary>>();
  const order: string[] = [];
  for (const s of detail.summary) {
    let row = byMetric.get(s.metric);
    if (!row) {
      row = new Map();
      byMetric.set(s.metric, row);
      order.push(s.metric);
    }
    row.set(s.variant, s);
  }
  const deltaFor = new Map(detail.deltas.map((d) => [d.metric, d]));
  return order.map((metric) => ({
    metric,
    cells: detail.variants.map((v) => {
      const s = byMetric.get(metric)?.get(v);
      return s ? { mean: s.mean, stderr: s.stderr, n: s.n } : null;
    }),
    delta: deltaFor.get(metric) ?? null,
  }));
}

// groupMembersByTask keeps the arms of one task adjacent, which is the
// arrangement that lets a reader compare a single task across variants
// without scanning the whole table.
export function groupMembersByTask(members: EvalMemberRow[]): { taskId: string; members: EvalMemberRow[] }[] {
  const groups = new Map<string, EvalMemberRow[]>();
  for (const m of members) {
    const existing = groups.get(m.task_id);
    if (existing) existing.push(m);
    else groups.set(m.task_id, [m]);
  }
  return [...groups].map(([taskId, rows]) => ({ taskId, members: rows }));
}

// evalRunProgress is the fraction of a run's members that have finished, for
// the list's progress bar. A run with no members is complete rather than
// dividing by zero.
export function evalRunProgress(run: EvalRunRow): number {
  if (run.total === 0) return 1;
  return run.finished / run.total;
}

// estimateCost is what a start is about to spend, from the mean cost of the
// runs already recorded. It is an estimate and says so: a suite nobody has
// run yet has nothing to estimate from and returns null rather than a
// confident zero.
export function estimateCost(totalRuns: number, priorRuns: EvalRunRow[]): number | null {
  const finished = priorRuns.filter((r) => r.finished > 0);
  if (finished.length === 0) return null;
  const perRun = finished.reduce((sum, r) => sum + r.cost_usd / r.finished, 0) / finished.length;
  return perRun * totalRuns;
}

// isRunning says whether a run is still going, which is what makes the detail
// screen poll and the list show a progress bar.
export function isRunning(run: { status: string }): boolean {
  return run.status === "running";
}
