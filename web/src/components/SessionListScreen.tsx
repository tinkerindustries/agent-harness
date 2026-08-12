import { useEffect, useMemo, useState } from "react";
import { useSyncExternalStore } from "react";
import { sessionListStore } from "../api/sessionListStore";
import { listSettings } from "../api/settings";
import { controlToken } from "../api/operations";
import { startedBy } from "../api/provenance";
import type { QueueHealth, SessionState, Usage } from "../api/types";
import { useNow, useQueueHealth } from "../hooks";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Card } from "./ui/card";
import { Input } from "./ui/input";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "./ui/collapsible";
import { outcome } from "./statusBadge";
import { PlanList } from "./PlanList";
import { planProgress, splitVerb } from "./planProgress";
import { StopControl } from "./StopControl";
import { StartRunForm } from "./StartRunForm";
import { useNavRight } from "./TopNav";

function formatElapsed(sess: SessionState, nowMs: number): string {
  const start = Date.parse(sess.created_at);
  const end = sess.finished_at ? Date.parse(sess.finished_at) : nowMs;
  return formatMs(end - start);
}

// formatMs is the m:ss shape both the table's Elapsed column and the stat
// strip's median use — the same granularity the elapsed figure has always
// carried, rolled up to a day.
function formatMs(ms: number): string {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
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

// matchesQuery is the session list's own filter (design/sessions-v2.html's
// search input): id, workspace, or request id, client-side over the snapshot
// the table already holds — no backend field, no endpoint.
function matchesQuery(s: SessionState, q: string): boolean {
  if (q === "") return true;
  return (
    s.id.toLowerCase().includes(q) ||
    s.workspace.toLowerCase().includes(q) ||
    (s.request_id ?? "").toLowerCase().includes(q)
  );
}

interface DayStats {
  running: number;
  spendUsd: number;
  medianMs: number | null;
  count: number;
  totalMs: number | null;
}

// computeDayStats rolls the snapshot up into the stat strip's four numbers
// (design/sessions-v2.html): Running, Spend today, Median duration today,
// Total time today. "Today" is the current local calendar day, judged
// by created_at. Nothing here is a new backend field — running and the
// finished-today count come straight from the list, spend, the median and
// the total are reductions over it — and the duration figures cover only
// sessions that have a finished_at (a running session's duration is not a
// duration yet). The median and the total are over the same set, so one
// count serves both cards' small print.
function computeDayStats(sessions: SessionState[], dayStartMs: number): DayStats {
  let running = 0;
  let spendUsd = 0;
  const durations: number[] = [];
  for (const s of sessions) {
    if (s.status === "running") running++;
    const created = Date.parse(s.created_at);
    if (Number.isNaN(created) || created < dayStartMs) continue;
    spendUsd += s.usage.cost_usd;
    if (s.finished_at) {
      const end = Date.parse(s.finished_at);
      if (!Number.isNaN(end)) durations.push(end - created);
    }
  }
  return {
    running,
    spendUsd,
    medianMs: median(durations),
    count: durations.length,
    totalMs: durations.length > 0 ? durations.reduce((a, b) => a + b, 0) : null,
  };
}

function median(nums: number[]): number | null {
  if (nums.length === 0) return null;
  const sorted = [...nums].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

interface Props {
  onOpen: (id: string) => void;
}

export function SessionListScreen({ onOpen }: Props) {
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);
  const now = useNow(1000);
  const queueHealth = useQueueHealth(5000);

  // One disclosure per in-flight card, independent of every other (several
  // can be open at once — docs/WEB-REDESIGN.md phase 3, not an accordion).
  // Held here rather than inside the card so the open state survives a list
  // update: when the SSE feed pushes a new snapshot the cards re-render
  // under the same keys, and a card that was open stays open.
  const [openIds, setOpenIds] = useState<ReadonlySet<string>>(new Set());
  const toggleOpen = (id: string, open: boolean) => {
    setOpenIds((prev) => {
      const next = new Set(prev);
      if (open) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  // The list's own search (design/sessions-v2.html). The input lives in the
  // shared nav's right slot (below); the filter runs over the snapshot the
  // table already holds, client-side, so a keystroke never hits the network.
  const [query, setQuery] = useState("");

  // The start form (docs/RUN-CONTROL.md "The frontend"): the trigger lives
  // in the nav's right slot, the form card opens at the top of the screen.
  // The control token is fetched once per page load (controlToken caches its
  // promise); a null token means run control is not configured, and the nav
  // says so instead of offering a button that would 503.
  const [startOpen, setStartOpen] = useState(false);
  const [startToken, setStartToken] = useState<string | null>(null);
  const [startTokenReady, setStartTokenReady] = useState(false);
  useEffect(() => {
    let cancelled = false;
    controlToken().then((t) => {
      if (!cancelled) {
        setStartToken(t);
        setStartTokenReady(true);
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // A follow-up run from a finished session's chat page lands here with
  // "#start" in the hash (SessionChatScreen's finished band navigates to
  // "/#start"): open the start form without the operator hunting for the
  // trigger, exactly as if they had clicked it.
  useEffect(() => {
    if (window.location.hash === "#start") setStartOpen(true);
  }, []);

  // The stat strip's "of N slots" reads worker.pool_size off the same
  // settings endpoint the settings screen calls. One fetch on mount — the
  // pool size changes only with a restart — and if it is unreachable the
  // strip renders the running count without the denominator rather than
  // failing the screen.
  const [poolSize, setPoolSize] = useState<number | null>(null);
  useEffect(() => {
    let cancelled = false;
    listSettings()
      .then((entries) => {
        const entry = entries.find((e) => e.key === "worker.pool_size");
        const raw = entry ? (entry.set ? entry.value : entry.default) : undefined;
        if (!cancelled && raw !== undefined) {
          const n = Number(raw);
          if (Number.isFinite(n)) setPoolSize(n);
        }
      })
      .catch(() => {
        // Settings unreachable: omit "of N slots". The queue bar and the
        // tables still work; this is a garnish, not the screen.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // The stats are "today"-scoped, so they recompute only when the list
  // changes or the local calendar day rolls over — never on the second tick.
  const dayStartMs = useMemo(() => {
    const d = new Date(now);
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  }, [now]);
  const stats = useMemo(
    () => computeDayStats(snapshot.sessions, dayStartMs),
    [snapshot.sessions, dayStartMs],
  );

  const q = query.trim().toLowerCase();
  const running = useMemo(
    () => snapshot.sessions.filter((s) => s.status === "running" && matchesQuery(s, q)),
    [snapshot.sessions, q],
  );
  const finished = useMemo(
    () => snapshot.sessions.filter((s) => s.status !== "running" && matchesQuery(s, q)),
    [snapshot.sessions, q],
  );
  const showEmpty = snapshot.sessions.length === 0;

  // The nav's right slot for this screen (design/nav.html's Sessions state):
  // the start-run trigger (phase 6, docs/RUN-CONTROL.md "The frontend"), the
  // search input, and the LIVE badge. The dot pulses while the stream is
  // open and goes still while EventSource reconnects. When run control is
  // not configured the trigger is replaced by a note saying so — a form
  // whose submit would 503 must not be offered as a button.
  useNavRight(
    <>
      {startTokenReady && startToken === null ? (
        <span className="nav-note" title="start harness serve once to generate http.control_token">
          run control not configured — starting is disabled
        </span>
      ) : (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setStartOpen((o) => !o)}
          disabled={!startTokenReady}
          aria-expanded={startOpen}
        >
          {startOpen ? "Close" : "Start run"}
        </Button>
      )}
      <Input
        type="search"
        className="nav-search"
        placeholder="Filter by id, workspace, request…"
        value={query}
        onChange={(ev) => setQuery(ev.target.value)}
        spellCheck={false}
      />
      <Badge variant={snapshot.connection === "open" ? "running" : "outline"}>
        <span className={cn("dot", snapshot.connection === "open" && "dot-pulse")} />
        {snapshot.connection === "open" ? "LIVE" : "connecting"}
      </Badge>
    </>,
  );

  return (
    <div className="screen">
      {startOpen && startToken !== null && (
        <StartRunForm token={startToken} onClose={() => setStartOpen(false)} onOpen={onOpen} />
      )}
      <StatStrip stats={stats} poolSize={poolSize} />
      <QueueHealthBar health={queueHealth} />

      {running.length > 0 && (
        <section className="list-section">
          <div className="section-head">
            <h2>In flight</h2>
            <span className="count">
              {running.length} session{running.length === 1 ? "" : "s"}
            </span>
          </div>
          <div className="inflight-list">
            {running.map((sess) => (
              <InFlightCard
                key={sess.id}
                sess={sess}
                now={now}
                open={openIds.has(sess.id)}
                onOpenChange={(open) => toggleOpen(sess.id, open)}
                onOpen={onOpen}
              />
            ))}
          </div>
        </section>
      )}

      {(finished.length > 0 || showEmpty) && (
        <section className="list-section">
          <div className="section-head">
            <h2>Finished</h2>
            <span className="count">
              {finished.length} session{finished.length === 1 ? "" : "s"}
            </span>
          </div>
          <div className="table-scroll">
            <table className="session-table">
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Session</th>
                  <th>Elapsed</th>
                  <th>Cost</th>
                  <th>Model</th>
                  <th>Sub-turns</th>
                  <th>Cache</th>
                </tr>
              </thead>
              <tbody>
                {finished.map((sess) => (
                  <FinishedRow key={sess.id} sess={sess} now={now} onOpen={onOpen} />
                ))}
                {showEmpty && (
                  <tr>
                    <td colSpan={7} className="empty-row">
                      No sessions yet.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}

// StatStrip is the four cards above the queue health bar
// (design/sessions-v2.html): Running (of the pool's slots), Spend today,
// Median duration today, Total time today. Each value carries the
// qualifying small print under it — the denominator, the count the
// duration figures are over — the way the drawing's cards do, so a
// rolled-up figure never floats free of what it is made of.
function StatStrip({ stats, poolSize }: { stats: DayStats; poolSize: number | null }) {
  return (
    <div className="stats">
      <Card className="stat">
        <span className="label">Running</span>
        <span className="value">
          {stats.running}
          {poolSize !== null && <small>of {poolSize} slots</small>}
        </span>
      </Card>
      <Card className="stat">
        <span className="label">Spend today</span>
        <span className="value">{formatCost(stats.spendUsd)}</span>
      </Card>
      <Card className="stat">
        <span className="label">Median duration</span>
        <span className="value">
          {stats.medianMs !== null ? formatMs(stats.medianMs) : "—"}
          {stats.count > 0 && <small>{stats.count} sessions today</small>}
        </span>
      </Card>
      <Card className="stat">
        <span className="label">Total time today</span>
        <span className="value">
          {stats.totalMs !== null ? formatMs(stats.totalMs) : "—"}
          {stats.totalMs !== null && <small>{stats.count} sessions today</small>}
        </span>
      </Card>
    </div>
  );
}

// InFlightCard is one running session as a collapsible plan card
// (docs/WEB-REDESIGN.md phase 3, design/sessions.html). Collapsed, its
// summary answers what the session is about — the job's description (sess.task)
// — and what it is doing (the in_progress item's activeForm) and how far in
// it is (the completed ratio); expanded, it shows the whole plan and the
// actions row. The caret is its own small toggle button (sibling of the
// summary, each keyboard-reachable): it toggles the plan disclosure, while
// clicking anywhere else on the summary opens the session page. A session
// that never wrote a plan shows no plan section at all, neither collapsed
// nor expanded, and a session with no task shows no description line.
function InFlightCard({
  sess,
  now,
  open,
  onOpenChange,
  onOpen,
}: {
  sess: SessionState;
  now: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onOpen: (id: string) => void;
}) {
  const badge = outcome(sess);
  const plan = sess.plan ?? [];
  const prog = planProgress(plan);
  const { verb, rest } = splitVerb(prog.activeForm);

  return (
    <Card className="run-card">
      <Collapsible open={open} onOpenChange={onOpenChange}>
        <div className="run-head">
          <CollapsibleTrigger asChild>
            <button
              type="button"
              className="caret-btn"
              aria-label={open ? "Collapse the plan" : "Expand the plan"}
            >
              <span className={cn("caret", open && "caret-open")}>▸</span>
            </button>
          </CollapsibleTrigger>
          <button type="button" className="run-summary" onClick={() => onOpen(sess.id)}>
            <span className="run-line1">
              <Badge variant={badge.variant}>{badge.label}</Badge>
              <span className="run-meta">
                {sess.model} · {sess.effort}
                {sess.job_type && <> · {sess.job_type}</>}
                {startedBy(sess) && <> · {startedBy(sess)}</>}
              </span>
              {/* The two figures a running session is judged by — elapsed
                  in full weight, sub-turns dimmer — the finished table's
                  columns minus Cost and Cache (design/sessions-v2.html). */}
              <span className="run-stats">
                <span className="primary">
                  {formatElapsed(sess, now)}
                  <span className="unit">elapsed</span>
                </span>
                <span className="secondary">
                  <b>{sess.sub_turns}</b> sub-turns
                </span>
              </span>
            </span>
            {sess.task && (
              <span className="run-desc" title={sess.task}>
                {sess.task}
              </span>
            )}
            {plan.length > 0 && (
              <span className="run-line2">
                <span className="run-now truncate">
                  {prog.activeForm !== "" && (
                    <>
                      <span className="verb">{verb}</span>
                      {rest && <> {rest}</>}
                    </>
                  )}
                </span>
                <span className="run-progress">
                  <span className="ratio">
                    {prog.done} / {prog.total}
                  </span>
                </span>
              </span>
            )}
          </button>
        </div>
        <CollapsibleContent>
          <div className="run-body">
            {plan.length > 0 && (
              <div className="run-plan">
                <div className="panel-label">Plan</div>
                <PlanList todos={plan} />
              </div>
            )}
            <div className="run-actions">
              <StopControl sessionId={sess.id} running={true} />
            </div>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}

// FinishedRow is one finished session in the dense table. The Session cell
// carries the subtitle — the plan ratio and the model's own summary
// (docs/WEB-REDESIGN.md phase 3), wrapped across up to three lines (the
// .sess-sub line clamp), so scanning the list does not require opening each
// transcript. The row itself is clickable (onOpen, on the <tr>) — the cell
// holds no id any more, and the click target never lived on the id span.
// Column order is Status, Session, Elapsed, Cost, Model, Sub-turns, Cache
// (design/sessions-v2.html): the two numbers the redesign asked to
// prioritise sit right after Session, where they stay visible before any
// column the scroll container might still need on a narrow viewport.
// Elapsed and Cost carry the same primary weight as the in-flight card's
// stat row.
function FinishedRow({
  sess,
  now,
  onOpen,
}: {
  sess: SessionState;
  now: number;
  onOpen: (id: string) => void;
}) {
  const badge = outcome(sess);
  const plan = sess.plan ?? [];
  const prog = planProgress(plan);
  const ratio = plan.length > 0 ? `${prog.done} of ${prog.total} plan items` : "";
  const subtitle = [ratio, sess.summary].filter(Boolean).join(" · ");
  return (
    <tr className="session-row" onClick={() => onOpen(sess.id)}>
      <td>
        <Badge variant={badge.variant}>{badge.label}</Badge>
      </td>
      <td>
        <div className="sess-cell">
          <span className="sess-sub">{subtitle || "—"}</span>
        </div>
      </td>
      <td className="primary">{formatElapsed(sess, now)}</td>
      <td className="primary" title={costTitle(sess)}>
        {formatCost(sess.usage.cost_usd)}
      </td>
      <td>
        {sess.model} <span className="dim">({sess.effort})</span>
        {sess.job_type && <span className="dim"> · {sess.job_type}</span>}
        {startedBy(sess) && <span className="dim"> · {startedBy(sess)}</span>}
      </td>
      <td>{sess.sub_turns}</td>
      <td className="dim" title={hitRateTitle(sess.usage)}>
        {formatHitRate(sess.usage)}
      </td>
    </tr>
  );
}

// QueueHealthBar surfaces consumer lag, in-flight count, and redelivery
// count, plus a halted state as an unmissable banner
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
