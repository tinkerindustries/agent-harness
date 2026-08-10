import { useState } from "react";
import { useSyncExternalStore } from "react";
import { sessionListStore } from "../api/sessionListStore";
import type { QueueHealth, RecentToolCall, SessionState, Usage } from "../api/types";
import { useNow, useQueueHealth } from "../hooks";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Card } from "./ui/card";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "./ui/collapsible";
import { outcome } from "./statusBadge";
import { PlanList } from "./PlanList";
import { planProgress, splitVerb } from "./planProgress";
import { StopControl } from "./StopControl";

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

// formatCallTime is the activity panel's timestamp: the local HH:MM of the
// call, the granularity the drawing's "Last five calls" rows carry
// (design/sessions.html).
function formatCallTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toTimeString().slice(0, 5);
}

// toolCallTarget is the activity panel's one-line answer to "what did the
// call touch": the file_path for Edit/Write/Read, the pattern for Grep, the
// command for Bash, the description for Task. Shaped from the raw arguments
// the model produced, exactly as the transcript's tool headers do
// (docs/WEB-REDESIGN.md phase 5); arguments that do not parse render
// nothing after the tool tag.
function toolCallTarget(name: string, argumentsJSON: string): string {
  if (!argumentsJSON) return "";
  let args: Record<string, unknown>;
  try {
    args = JSON.parse(argumentsJSON) as Record<string, unknown>;
  } catch {
    return "";
  }
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  switch (name) {
    case "Edit":
    case "Write":
    case "Read":
      return str(args.file_path);
    case "Grep":
      return str(args.pattern);
    case "Bash":
      return str(args.command);
    case "Task":
      return str(args.description);
    default:
      return "";
  }
}

interface Props {
  onOpen: (id: string) => void;
  onSettings: () => void;
  onOperations: () => void;
}

export function SessionListScreen({ onOpen, onSettings, onOperations }: Props) {
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

  const running = snapshot.sessions.filter((s) => s.status === "running");
  const finished = snapshot.sessions.filter((s) => s.status !== "running");
  const showEmpty = snapshot.sessions.length === 0;

  return (
    <div className="screen">
      <header className="screen-header">
        <h1>Sessions</h1>
        <Badge variant="outline" className={`connection-badge connection-${snapshot.connection}`}>
          {snapshot.connection}
        </Badge>
        <Button variant="outline" size="sm" onClick={onSettings}>
          settings
        </Button>
        <Button variant="outline" size="sm" onClick={onOperations}>
          operations
        </Button>
      </header>
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
                  <th>Model</th>
                  <th>Elapsed</th>
                  <th>Sub-turns</th>
                  <th>Cache hit</th>
                  <th>Cost</th>
                  <th>Request</th>
                </tr>
              </thead>
              <tbody>
                {finished.map((sess) => (
                  <FinishedRow key={sess.id} sess={sess} now={now} onOpen={onOpen} />
                ))}
                {showEmpty && (
                  <tr>
                    <td colSpan={8} className="empty-row">
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

// InFlightCard is one running session as a collapsible plan card
// (docs/WEB-REDESIGN.md phase 3, design/sessions.html). Collapsed, its
// trigger answers what the session is doing (the in_progress item's
// activeForm) and how far in it is (the completed ratio); expanded, it shows
// the whole plan and the last few tool calls. A session that never wrote a
// plan shows no plan section at all, neither collapsed nor expanded.
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
  const calls = sess.recent_tool_calls ?? [];

  return (
    <Card className="run-card">
      <Collapsible open={open} onOpenChange={onOpenChange}>
        <CollapsibleTrigger asChild>
          <button type="button" className={cn("run-summary", open && "run-summary-open")}>
            <span className="run-line1">
              <span className={cn("caret", open && "caret-open")}>▸</span>
              <Badge variant={badge.variant}>{badge.label}</Badge>
              <span className="run-id">{sess.id}</span>
              <span className="run-meta">
                {sess.model} · {sess.effort}
                {sess.job_type && <> · {sess.job_type}</>}
              </span>
              <span className="run-stats">
                <span>
                  <b>{formatElapsed(sess, now)}</b> elapsed
                </span>
                <span>
                  <b>{sess.sub_turns}</b> sub-turns
                </span>
                <span title={costTitle(sess)}>{formatCost(sess.usage.cost_usd)}</span>
              </span>
            </span>
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
        </CollapsibleTrigger>
        <CollapsibleContent>
          <div className={cn("run-body", plan.length === 0 && "run-body-noplan")}>
            {plan.length > 0 && (
              <div className="run-plan">
                <div className="panel-label">Plan</div>
                <PlanList todos={plan} />
              </div>
            )}
            <div className="run-side">
              <div className="panel-label">Last calls</div>
              <RecentCalls calls={calls} />
              <div className="run-actions">
                <Button variant="outline" size="sm" onClick={() => onOpen(sess.id)}>
                  Open transcript
                </Button>
                <StopControl sessionId={sess.id} running={true} />
              </div>
            </div>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}

// RecentCalls is the card's activity panel: the last few tool calls as one
// line each — time, tool tag, and the target the call worked on
// (design/sessions.html's "Last five calls").
function RecentCalls({ calls }: { calls: RecentToolCall[] }) {
  if (calls.length === 0) {
    return <div className="activity-empty dim">No tool calls yet.</div>;
  }
  return (
    <div className="activity">
      {calls.map((c, i) => (
        <div className="row" key={i}>
          <span className="t">{formatCallTime(c.created_at)}</span>
          <span className="tool-tag">{c.name}</span>
          <span className="target truncate">{toolCallTarget(c.name, c.arguments)}</span>
        </div>
      ))}
    </div>
  );
}

// FinishedRow is one finished session in the dense table. The Session cell
// gains a one-line subtitle: the plan ratio and the model's own summary
// (docs/WEB-REDESIGN.md phase 3), so scanning the list does not require
// opening each transcript.
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
          <span className="sess-id">{sess.id}</span>
          {subtitle && <span className="sess-sub truncate">{subtitle}</span>}
        </div>
      </td>
      <td>
        {sess.model} <span className="dim">({sess.effort})</span>
        {sess.job_type && <span className="dim"> · {sess.job_type}</span>}
      </td>
      <td>{formatElapsed(sess, now)}</td>
      <td>{sess.sub_turns}</td>
      <td className="dim" title={hitRateTitle(sess.usage)}>
        {formatHitRate(sess.usage)}
      </td>
      <td title={costTitle(sess)}>{formatCost(sess.usage.cost_usd)}</td>
      <td className="dim">{sess.request_id ?? "—"}</td>
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
