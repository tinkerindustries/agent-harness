import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSyncExternalStore } from "react";
import { Broadcast, CaretRight, Copy, MagnifyingGlass, Play, Queue, X } from "@phosphor-icons/react";
import { sessionListStore } from "../api/sessionListStore";
import { isLive } from "../api/status";
import { listSettings } from "../api/settings";
import { controlToken } from "../api/operations";
import { clampPage } from "../api/paging";
import { FINISHED_PAGE_SIZE, useFinishedSessions } from "../api/finishedSessions";
import { startedBy } from "../api/provenance";
import { titleLines } from "./sessionListTitle";
import { tableEmptyState } from "./sessionListEmpty";
import type { QueueHealth, SessionListRow, SessionState, Usage } from "../api/types";
import { useArrivals, useHeldFrames, useLabelFlip, useNow, usePeakNote, useQueueHealth, useSettledFlip } from "../hooks";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Card } from "./ui/card";
import { Input } from "./ui/input";
import { Pager } from "./ui/Pager";
import { RTBody, RTCell, RTEmptyRow, RTHead, RTLead, RTMain, RTRow, RTTable, RTTh } from "./ui/ResponsiveTable";
import { Ticker } from "./ui/Ticker";
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

function formatElapsed(sess: SessionListRow, nowMs: number): string {
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

// matchesQuery is the session list's own filter: id, workspace, or
// request id, client-side over the snapshot the table already holds —
// no backend field, no endpoint.
function matchesQuery(s: SessionListRow, q: string): boolean {
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
  totalMs: number | null;
}

// computeDayStats rolls the snapshot up into the stat strip's four
// numbers: Running, Spend today, Median duration today, Total time
// today. "Today" is the current local calendar day, judged
// by created_at. Nothing here is a new backend field — running and the
// finished-today count come straight from the list, spend, the median and
// the total are reductions over it — and the duration figures cover only
// sessions that have a finished_at (a running session's duration is not a
// duration yet). The median and the total are over the same set; the strip
// shows them as the two figures, with no count small print under either.
function computeDayStats(sessions: SessionListRow[], dayStartMs: number): DayStats {
  let running = 0;
  let spendUsd = 0;
  const durations: number[] = [];
  for (const s of sessions) {
    if (isLive(s.status)) running++;
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
  // "" unless DeepSeek is currently billing peak rates (api/pricing.ts).
  const peak = usePeakNote(now);

  // One disclosure per in-flight card, independent of every other (several
  // can be open at once — not an accordion).
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

  // The list's own search. The input lives in the shared nav's right
  // slot (below); the Finished table's filter runs server-side on it
  // (useFinishedSessions, debounced), while the in-flight cards keep
  // filtering over the snapshot client-side — they are few, and the
  // snapshot is already in memory.
  const [query, setQuery] = useState("");

  // The Finished table is server-paged (useFinishedSessions): which 1-based
  // page is showing. It resets to 1 whenever the query changes — a new
  // filter starts at the top — and clamps to the last page when the total
  // shrinks under it, so deleting the last row of the last page can never
  // leave an empty table with no way back.
  const [page, setPage] = useState(1);
  const { items: finishedItems, total: finishedTotal } = useFinishedSessions(page, query.trim());
  useEffect(() => {
    setPage(1);
  }, [query]);
  useEffect(() => {
    setPage((p) => clampPage(p, finishedTotal, FINISHED_PAGE_SIZE));
  }, [finishedTotal]);

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
    () => snapshot.sessions.filter((s) => isLive(s.status) && matchesQuery(s, q)),
    [snapshot.sessions, q],
  );
  // Which rows arrived while this page was already open (hooks.ts
  // useArrivals). Keyed over the whole session set rather than either filtered
  // list, for two reasons: typing in the search box must not make the rows it
  // reveals read as new sessions, and a run finishing moves its row from the
  // in-flight cards to the table without being an arrival there — it is the
  // same session, and it was already on screen.
  const arrived = useArrivals(
    useMemo(() => snapshot.sessions.map((s) => s.id), [snapshot.sessions]),
    snapshot.sessions.length > 0,
  );
  // The empty-row state (sessionListEmpty.ts): "no-sessions" for a genuinely
  // empty harness, "no-match" when sessions exist but the Finished filter
  // matched none — a distinct state, so a blank page never reads as an empty
  // harness. The total-sessions argument is the SSE snapshot's length — the
  // stream still holds everything, so an empty harness stays distinguishable
  // from a filter that matched nothing — and the matched argument is the
  // paged envelope's total.
  const emptyState = tableEmptyState(snapshot.sessions.length, finishedTotal);

  // Whether anything is in flight, and whether that answer has changed since
  // the page settled. The strip is the page's hero — the first thing below
  // the nav — while nothing is running, and shrinks the moment something is,
  // making room for the In flight section that then lands above it: `busy`
  // compacts the four cards, and `flipped` is what allows the change to be a
  // gesture rather than the size the cards simply are. A page that loads with
  // a run already going renders the compact strip and the section without
  // either animating — neither of those facts just happened (hooks.ts
  // useSettledFlip).
  //
  // Settled is the list's own snapshot having landed, with the connection
  // holding open across two frames standing in for it on an empty harness,
  // which has no snapshot to wait for and whose first run is precisely the
  // onset worth animating.
  const busy = running.length > 0;
  const openHeld = useHeldFrames(snapshot.connection === "open");
  const flipped = useSettledFlip(busy, snapshot.sessions.length > 0 || openHeld);

  // The nav's right slot for this screen: the start-run trigger
  // (docs/RUN-CONTROL.md "The frontend"), the search input, and the LIVE
  // badge. The mark pulses while the stream is open and goes still while
  // EventSource reconnects. When run control is
  // not configured the trigger is replaced by a note saying so — a form
  // whose submit would 503 must not be offered as a button.
  useNavRight(
    <>
      {startTokenReady && startToken === null ? (
        <span className="text-xs whitespace-nowrap text-muted-foreground" title="start harness serve once to generate http.control_token">
          run control not configured — starting is disabled
        </span>
      ) : (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setStartOpen((o) => !o)}
          disabled={!startTokenReady}
          aria-expanded={startOpen}
          title={startOpen ? "Close" : "Start run"}
        >
          {startOpen ? <X /> : <Play />}
          <span className="max-nav:sr-only">{startOpen ? "Close" : "Start run"}</span>
        </Button>
      )}
      <Input
        type="search"
        icon={<MagnifyingGlass />}
        className="nav-search"
        placeholder="Filter by id, workspace, request…"
        // Uncontrolled on purpose: the input is rendered into the shared
        // nav's right slot through useNavRight, so a controlled value would
        // round-trip a render behind the keystrokes — the slot's node is
        // replaced on every screen render — and a keystroke landing inside
        // that window got overwritten when the stale value was written back
        // (typing abcdefghij at zero delay left aceghj). The DOM input always
        // holds exactly what was typed; onChange feeds the filter state, so
        // the list filtering still runs per keystroke, just never through the
        // input's own value.
        defaultValue=""
        onChange={(ev) => setQuery(ev.target.value)}
        spellCheck={false}
      />
      <Badge
        variant={snapshot.connection === "open" ? "running" : "outline"}
        title={snapshot.connection === "open" ? "LIVE" : "connecting"}
      >
        {/* Broadcast at bold rather than the app's light default: at 12px,
            beside an 11px uppercase label, the light stroke disappears. It
            takes the badge's currentColor and carries the pulse the plain dot
            used to — the badge is the one place a reader checks to see
            whether the screen is still being told anything. */}
        <Broadcast
          weight="bold"
          size={12}
          className={cn(snapshot.connection === "open" && "dot-pulse")}
        />
        <span className="max-nav:sr-only">{snapshot.connection === "open" ? "LIVE" : "connecting"}</span>
      </Badge>
    </>,
  );

  return (
    <div className="screen">
      {startOpen && startToken !== null && (
        <StartRunForm token={startToken} onClose={() => setStartOpen(false)} onOpen={onOpen} />
      )}
      <QueueHaltBanner health={queueHealth} />

      {/* No `anim-section-in` here: that fade/blur/lift reveal used to run on
          a section landing below the strip, where its instant full-height
          mount (only the reveal's own opacity/transform/filter animate, not
          layout) went unnoticed — nothing above it had to move. Now this
          section lands above the strip, so the same instant mount shoves the
          strip (and the transition it's mid-way through) straight down by
          the section's whole height before a single pixel of the reveal has
          drawn — a blank gap opening, then filling, on top of the strip's
          own shrink. Two competing gestures read worse than one: better to
          drop this reveal than ship the judder. The strip still shrinks
          smoothly on `flipped`; the section just appears at its settled
          position instead of animating into it. */}
      {busy && (
        <section className="[&+&]:mt-5">
          <div className="mb-2 flex items-baseline gap-2">
            <h2 className="m-0 text-[0.85rem] font-semibold">In flight</h2>
            <span className="text-xs text-muted-foreground">
              {running.length} session{running.length === 1 ? "" : "s"}
            </span>
          </div>
          <div className="flex flex-col gap-2">
            {running.map((sess) => (
              <InFlightCard
                key={sess.id}
                sess={sess}
                now={now}
                open={openIds.has(sess.id)}
                onOpenChange={(open) => toggleOpen(sess.id, open)}
                onOpen={onOpen}
                arrived={arrived(sess.id)}
              />
            ))}
          </div>
        </section>
      )}

      <StatStrip stats={stats} poolSize={poolSize} compact={busy} animate={flipped} peak={peak} />

      {(finishedTotal > 0 || emptyState !== "none") && (
        <section className="[&+&]:mt-5 mt-5">
          <div className="mb-2 flex items-baseline gap-2">
            <h2 className="m-0 text-[0.85rem] font-semibold">Finished</h2>
            <span className="text-xs text-muted-foreground">
              {finishedTotal} session{finishedTotal === 1 ? "" : "s"}
            </span>
          </div>
          <Pager
            className="mt-0 mb-2"
            label="Finished sessions, top pager"
            page={page}
            total={finishedTotal}
            perPage={FINISHED_PAGE_SIZE}
            onPage={setPage}
          />
          <div className="overflow-x-auto">
            <RTTable>
              <RTHead>
                <tr>
                  <RTTh>Status</RTTh>
                  <RTTh className="w-full whitespace-normal">Session</RTTh>
                  <RTTh>Elapsed</RTTh>
                  <RTTh>Cost</RTTh>
                  <RTTh>Model</RTTh>
                  <RTTh>Sub-turns</RTTh>
                  <RTTh>Cache</RTTh>
                </tr>
              </RTHead>
              <RTBody>
                {finishedItems.map((sess) => (
                  <FinishedRow key={sess.id} sess={sess} now={now} onOpen={onOpen} arrived={arrived(sess.id)} />
                ))}
                {emptyState !== "none" && (
                  <RTEmptyRow colSpan={7}>
                    {emptyState === "no-sessions" ? "No sessions yet." : "No sessions match this query."}
                  </RTEmptyRow>
                )}
              </RTBody>
            </RTTable>
          </div>
          <Pager
            label="Finished sessions, bottom pager"
            page={page}
            total={finishedTotal}
            perPage={FINISHED_PAGE_SIZE}
            onPage={setPage}
          />
        </section>
      )}
    </div>
  );
}

// StatStrip is the four cards above the queue banner: Running (of
// the pool's slots), Spend today, Median duration today, Total time
// today. The Running card carries the pool-size denominator as its only
// small print — the running count means nothing without it; the duration
// cards show their figures bare, with no count under either — the way the
// drawing's cards do, so a
// rolled-up figure never floats free of what it is made of.
// Every figure here is a Ticker: these four move while an operator watches the
// list — a run claims a slot, a sub-turn adds to the day's spend — and a number
// that moved should be noticeable without the row twitching. The finished table
// below deliberately gets none: those figures are final and a roll there is
// noise.
//
// The strip has two sizes. `compact` is the size it takes while something is
// in flight: the four figures stay, at roughly half the height, because the
// running work is what the page is about the moment there is any and the
// strip is the day's context around it. `animate` is separate on purpose — it
// says the size CHANGED while somebody was watching, which is the only time
// the change is worth a transition; without it the compact strip is just the
// size the page loaded at.
function StatStrip({
  stats,
  poolSize,
  compact,
  animate,
  peak,
}: {
  stats: DayStats;
  poolSize: number | null;
  compact: boolean;
  animate: boolean;
  // The peak-rate note, or "" when there is nothing to say (api/pricing.ts
  // peakNote). It rides on Spend today because that is the figure it is
  // about, and because the decision it informs — start this run now, or
  // after the window closes — is taken on this screen.
  peak: string;
}) {
  return (
    <div
      className={cn(
        "mb-3.5 grid grid-cols-4 gap-2 motion-reduce:transition-none",
        compact && "mt-5 mb-2.5 gap-1.5",
        animate &&
          "transition-[gap,margin-bottom] [transition-duration:var(--dur-reveal)] [transition-timing-function:var(--ease-out)]",
      )}
    >
      <StatCard label="Running" compact={compact} animate={animate}>
        <Ticker value={stats.running} />
        {poolSize !== null && <StatFigureNote compact={compact} animate={animate}>of {poolSize} slots</StatFigureNote>}
      </StatCard>
      <StatCard label="Spend today" compact={compact} animate={animate}>
        <Ticker value={formatCost(stats.spendUsd)} />
        {peak && (
          <span
            className="ml-2 text-[0.6875rem] font-normal whitespace-nowrap text-[var(--status-gaveup)]"
            title="DeepSeek bills peak hours at twice off-peak (docs/DESIGN.md §4.9)"
          >
            {peak}
          </span>
        )}
      </StatCard>
      <StatCard label="Median duration" compact={compact} animate={animate}>
        <Ticker value={stats.medianMs !== null ? formatMs(stats.medianMs) : "—"} />
      </StatCard>
      <StatCard label="Total time today" compact={compact} animate={animate}>
        <Ticker value={stats.totalMs !== null ? formatMs(stats.totalMs) : "—"} />
      </StatCard>
    </div>
  );
}

// The transition-list utilities StatCard/StatFigureNote share, only while
// `animate` says the strip's size just changed under a watcher (never on
// the load a page already settled at).
const statFigureTransition =
  "transition-[font-size,line-height,margin-top] [transition-duration:var(--dur-reveal)] [transition-timing-function:var(--ease-out)] motion-reduce:transition-none";

function StatCard({
  label,
  compact,
  animate,
  children,
}: {
  label: string;
  compact: boolean;
  animate: boolean;
  children: ReactNode;
}) {
  return (
    <Card
      className={cn(
        "gap-0 rounded-[var(--radius)] px-3 py-2.5 shadow-none motion-reduce:transition-none",
        compact && "px-2.5 py-1",
        animate && "transition-[padding] [transition-duration:var(--dur-reveal)] [transition-timing-function:var(--ease-out)]",
      )}
    >
      <span className={cn("text-[11px] tracking-[0.06em] text-muted-foreground uppercase", compact && "text-[10px] leading-[1.3]", statFigureTransition)}>
        {label}
      </span>
      <span
        className={cn(
          "mt-0.5 text-[1.375rem] font-semibold tracking-[-0.02em] tabular-nums",
          compact && "mt-0 text-sm leading-[1.3]",
          statFigureTransition,
        )}
      >
        {children}
      </span>
    </Card>
  );
}

function StatFigureNote({ compact, animate, children }: { compact: boolean; animate: boolean; children: ReactNode }) {
  return (
    <small
      className={cn(
        "mt-px [display:block] text-xs font-normal tracking-normal text-muted-foreground",
        compact && "text-[10px] leading-[1.3]",
        animate && statFigureTransition,
      )}
    >
      {children}
    </small>
  );
}

// InFlightCard is one running session as a collapsible plan card.
// Collapsed, its summary answers what the
// session is about — the run's title bold with the description
// under it, no description line when the title exists without one (the raw
// prompt is not repeated), and the raw prompt only when there is no title at
// all — and what it is
// doing (the in_progress item's activeForm) and how far in
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
  arrived = false,
}: {
  // The in-flight cards and the stat strip run off the SSE snapshot, which
  // carries the list projection (SessionListRow) rather than the whole row.
  // The Finished table below is the other half — it reads full rows from the
  // paged REST endpoint, which is why FinishedRow can render the summary and
  // the cache figures and this card cannot.
  sess: SessionListRow;
  now: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onOpen: (id: string) => void;
  // Whether this run started while the list was already open, rather than
  // being one of the rows the page loaded with (hooks.ts useArrivals).
  arrived?: boolean;
}) {
  const badge = outcome(sess);
  const flip = useLabelFlip(badge.label);
  const plan = sess.plan ?? [];
  const prog = planProgress(plan);
  const { verb, rest } = splitVerb(prog.activeForm);
  const lines = titleLines(sess);
  // The meta line renders the model name alone, the way the Finished table's
  // Model cell does. The detail it used to print inline — the effort, the
  // job type when there is one, and the full provenance label, id included
  // — rides on the span's title, one hover away, so the line stays short
  // enough to leave the stat figures their room on the row.
  const metaTitle = [sess.effort, sess.job_type, startedBy(sess)].filter(Boolean).join(" · ");

  return (
    <Card className={cn("gap-0 overflow-hidden p-0", arrived && "anim-row-in")} interactive>
      <Collapsible open={open} onOpenChange={onOpenChange}>
        <div className="flex items-stretch">
          <CollapsibleTrigger asChild>
            <button
              type="button"
              className="group flex flex-none cursor-pointer items-start border-none bg-transparent pt-[13px] pr-1 pb-0 pl-3.5 font-[inherit] max-phone:min-w-11"
              aria-label={open ? "Collapse the plan" : "Expand the plan"}
            >
              {/* The one place the design's ▸ caret becomes an icon: it is an
                  affordance here, a button of its own, not the plan and rail
                  vocabulary the text glyph carries elsewhere. */}
              <CaretRight
                className={cn(
                  "text-muted-foreground [transition:transform_var(--dur-caret)_var(--ease)] group-hover:text-foreground",
                  open && "rotate-90",
                )}
              />
            </button>
          </CollapsibleTrigger>
          <button
            type="button"
            className="min-w-0 flex-1 cursor-pointer border-none bg-transparent py-3 pr-3.5 pl-2 text-left font-[inherit] text-inherit hover:bg-[var(--surface-hover)]"
            onClick={() => onOpen(sess.id)}
          >
            <span className="flex items-center gap-2.5 max-phone:flex-wrap max-phone:gap-y-1">
              <Badge key={badge.label} variant={badge.variant} className={flip}>
                {badge.label}
              </Badge>
              <span className="min-w-0 overflow-hidden text-sm whitespace-nowrap text-ellipsis text-muted-foreground" title={metaTitle}>
                {sess.model}
              </span>
              {/* The two figures a running session is judged by — elapsed
                  in full weight, sub-turns dimmer — the finished table's
                  columns minus Cost and Cache. */}
              <span className="ml-auto flex flex-none items-center gap-4 text-sm whitespace-nowrap tabular-nums max-phone:ml-0 max-phone:w-full">
                <span className="text-[0.875rem] font-semibold text-foreground">
                  {/* Elapsed is the one figure on this card that moves every
                      second, so it rolls; sub-turns beside it does not. */}
                  <Ticker value={formatElapsed(sess, now)} />
                  <span className="ml-[3px] font-normal text-muted-foreground">elapsed</span>
                </span>
                <span className="text-muted-foreground">
                  <b className="font-semibold text-foreground">{sess.sub_turns}</b> sub-turns
                </span>
              </span>
            </span>
            {(lines.title || lines.desc) && (
              <span className="mt-[7px] [display:block] text-sm" title={sess.task}>
                {lines.title && (
                  <span className="flex items-baseline gap-2 font-semibold text-foreground">
                    {lines.title}
                    {lines.phase && (
                      <span className="flex-none rounded-full border border-border px-1.5 text-micro leading-[1.5] whitespace-nowrap text-muted-foreground">
                        {lines.phase}
                      </span>
                    )}
                  </span>
                )}
                {lines.desc !== "" && (
                  <span className="line-clamp-3 overflow-hidden text-ellipsis">{lines.desc}</span>
                )}
              </span>
            )}
            {plan.length > 0 && (
              <span className="mt-[7px] flex items-center gap-2.5">
                <span className="min-w-0 truncate text-sm whitespace-normal">
                  {prog.activeForm !== "" && (
                    <>
                      <span className="font-medium text-[var(--status-running)]">{verb}</span>
                      {rest && <> {rest}</>}
                    </>
                  )}
                </span>
                <span className="ml-auto flex flex-none items-center gap-2">
                  <span className="text-xs whitespace-nowrap text-muted-foreground tabular-nums">
                    {prog.done} / {prog.total}
                  </span>
                </span>
              </span>
            )}
          </button>
        </div>
        <CollapsibleContent>
          <div className="border-t border-border">
            {plan.length > 0 && (
              <div className="pt-3 pr-3.5 pb-3.5 pl-[34px]">
                <div className="mb-2 text-xs tracking-[0.06em] text-muted-foreground uppercase">Plan</div>
                <PlanList todos={plan} compact />
              </div>
            )}
            <div className={cn("flex gap-2 px-3.5 py-3", plan.length > 0 && "border-t border-border")}>
              <StopControl sessionId={sess.id} running={true} />
              <CopyIdButton sessionId={sess.id} />
            </div>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}

// CopyIdButton puts the session id on the clipboard — the id is what every
// other tool in the harness takes as its argument (`harness export`, the MCP
// tools, a URL), and reading it off the screen to retype it is the one thing
// the card asked an operator to do by hand. It confirms itself the way the
// result panel's copy control does, rather than saying nothing.
function CopyIdButton({ sessionId }: { sessionId: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(sessionId).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    });
  };
  return (
    <Button variant="outline" size="sm" onClick={copy} title={sessionId}>
      <Copy />
      {copied ? "Copied" : "Copy id"}
    </Button>
  );
}

// FinishedRow is one finished session in the dense table. The Session cell
// carries the run's title bold on its own line with the description beneath
// it (clamped to two lines), and the old subtitle — the plan ratio and the
// model's own summary — moved below the description, dimmer (the .sess-sub
// line clamp), so scanning the list still does not require opening each
// transcript. A row with a description renders it under the title; a title
// without a description shows no description line (the raw prompt is not
// repeated under a title that says what the run is); and a row with no title
// at all — pre-migration, blank browser start — renders the raw prompt as
// the description line with no bold title, so no row ever goes blank. The
// row itself is clickable (onOpen, on the <tr>) —
// the cell holds no id any more, and the click target never lived on the id
// span. Column order is Status, Session, Elapsed, Cost, Model, Sub-turns,
// Cache: the two numbers an operator scans for sit right after Session,
// where they stay visible before any column the scroll container might
// still need on a narrow viewport.
// The Model cell renders the model name alone; the effort, the job type and
// the full provenance label (id included) moved onto the cell's title, so
// the column stays narrow and the detail is one hover away.
// Elapsed and Cost carry the same primary weight as the in-flight card's
// stat row.
function FinishedRow({
  sess,
  now,
  onOpen,
  arrived = false,
}: {
  sess: SessionState;
  now: number;
  onOpen: (id: string) => void;
  // Whether this session appeared while the list was already open (hooks.ts
  // useArrivals). A run that finishes moves from the cards above into this
  // table, and that is not an arrival — useArrivals keys over the whole
  // session set, so the row it moved from covers it.
  arrived?: boolean;
}) {
  const badge = outcome(sess);
  const plan = sess.plan ?? [];
  const prog = planProgress(plan);
  const ratio = plan.length > 0 ? `${prog.done} of ${prog.total} plan items` : "";
  const subtitle = [ratio, sess.summary].filter(Boolean).join(" · ");
  const lines = titleLines(sess);
  // The Model cell renders the model name alone. The detail it used to print
  // inline — the effort, the job type when there is one, and the full
  // provenance label, id included — rides on the cell's title, one hover
  // away; the full label's UUID is what wrapped the old cell to three lines
  // and pushed the Sub-turns and Cache columns out of the table's container.
  const modelTitle = [sess.effort, sess.job_type, startedBy(sess)].filter(Boolean).join(" · ");
  return (
    <RTRow className={cn(arrived && "anim-row-in")} onClick={() => onOpen(sess.id)}>
      <RTLead>
        <Badge variant={badge.variant}>{badge.label}</Badge>
      </RTLead>
      <RTMain>
        <div className="flex min-w-0 flex-col gap-px whitespace-normal wrap-anywhere">
          {lines.title && (
            <span className="flex items-baseline gap-2 font-semibold text-foreground">
              {lines.title}
              {lines.phase && (
                <span className="flex-none rounded-full border border-border px-1.5 text-micro leading-[1.5] whitespace-nowrap text-muted-foreground">
                  {lines.phase}
                </span>
              )}
            </span>
          )}
          {lines.desc !== "" && <span className="line-clamp-2 text-sm">{lines.desc}</span>}
          <span className="line-clamp-3 text-xs text-muted-foreground">{subtitle || "—"}</span>
        </div>
      </RTMain>
      <RTCell label="Elapsed" className="whitespace-nowrap font-semibold tabular-nums">
        {formatElapsed(sess, now)}
      </RTCell>
      <RTCell label="Cost" className="whitespace-nowrap font-semibold tabular-nums" title={costTitle(sess)}>
        {formatCost(sess.usage.cost_usd)}
      </RTCell>
      <RTCell label="Model" wide className="truncate" title={modelTitle}>
        {sess.model}
      </RTCell>
      <RTCell label="Sub-turns" className="whitespace-nowrap">
        {sess.sub_turns}
      </RTCell>
      <RTCell label="Cache" className="whitespace-nowrap text-muted-foreground" title={hitRateTitle(sess.usage)}>
        {formatHitRate(sess.usage)}
      </RTCell>
    </RTRow>
  );
}

// QueueHaltBanner is what is left of the old queue-health bar: the routine
// counters (pending / in flight / redelivered) are gone, and the bar now
// exists only for the one thing on it an operator cannot afford to miss — a
// halted pool, as an unmissable banner rather than another quiet figure, plus
// the health error when there is one (docs/DESIGN.md §4.5). Renders nothing
// at all unless the queue is halted or health.error is set, and nothing while
// the first poll is still in flight.
function QueueHaltBanner({ health }: { health: QueueHealth | null }) {
  if (!health || (!health.halted && !health.error)) return null;
  return (
    <div
      className={cn(
        "mb-2 flex items-center gap-1.5 rounded-[calc(var(--radius)-4px)] border border-border px-2 py-1 text-[0.8rem] text-muted-foreground [&_svg]:h-[13px] [&_svg]:w-[13px] [&_svg]:flex-none",
        health.halted && "border-[var(--status-gaveup)] font-semibold text-[var(--status-gaveup)]",
      )}
    >
      <Queue aria-hidden />
      {health.halted && <span>queue halted — {health.halt_reason || "reason unknown"}</span>}
      {health.error && <span className="text-muted-foreground"> ({health.error})</span>}
    </div>
  );
}
