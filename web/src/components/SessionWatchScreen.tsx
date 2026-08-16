import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, StopCircle } from "@phosphor-icons/react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptSnapshot } from "../api/transcriptStore";
import { TurnTranscript } from "./turns/TurnTranscript";
import { WatchRail } from "./WatchRail";
import { WatchFooter } from "./WatchFooter";
import { ResultPanel, type RunFinishedBlock } from "./ResultPanel";
import { DroppedStreamBanner } from "./DroppedStreamBanner";
import { controlToken, errorMessage, stopSession } from "../api/operations";
import { isLive } from "../api/status";
import { getSessionEval, type EvalMembership } from "../api/evals";
import { startedBy } from "../api/provenance";
import { SessionIdContext, useLabelFlip, useNow } from "../hooks";
import { MSG_BODY_CLS, MSG_CLS, MSG_STATE_CLS, MSG_USER_CLS } from "./turns/SteerMessage";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { outcome } from "./statusBadge";
import { cachePercent, formatRunDuration, watchStatusFigures } from "./turns/turnHelpers";
import { formatCost } from "./blocks/toolArgs";
import { useNavRight } from "./TopNav";

interface Props {
  sessionId: string;
  // The metadata row the fork read before choosing this screen — always
  // non-null here: a null row keeps the chat screen, the safe default
  // (SessionScreen), so this page renders only for a real agent-launched
  // run with provenance to show.
  meta: SessionState;
  snapshot: TranscriptSnapshot;
  // The app's navigate, for the provenance strip's "Session row →" link
  // back to the session list.
  onNavigate: (path: string) => void;
  // Whether this session's stream has opened since the page loaded
  // (SessionScreen's per-session flag, for the dropped-stream banner).
  everOpen: boolean;
}

// The read-only session page: a run another agent started, inside the
// full-height app shell the two session pages share while the run is
// live. You may stop it; you may not talk to it —
// its instructions come from the agent that launched it, and the page shows
// no way to send a message, not even a disabled one (a greyed-out input
// invites you to look for the way to enable it). A finished run drops the
// shell: one page scroll, the sticky rail, the footer's figures in the nav
// (SessionScreen decides the mode). This page is the navigator
// rail on the left (the plan as phases
// with one tick per sub-turn), the provenance strip leading
// the transcript column, the live footer answering
// what the run is doing right now, the result the parent gets back at the
// end of the stream, and the dropped-stream banner both pages share.
export function SessionWatchScreen({ sessionId, meta, snapshot, onNavigate, everOpen }: Props) {
  const now = useNow(1000);
  // The run is live until the row says otherwise: running, or still
  // creating while the worker prepares its workspace — a creating run has
  // no events yet, so "not running" would misread it as finished.
  const running = isLive(meta.status);

  // The one-line provenance label ("started by claude-code (sess-1)") and
  // the bare identity the sentence below names. provenance.ts owns the
  // label's shape (web/src/api/provenance.ts startedBy), including the
  // legacy fallback; the strip never re-formats the fields itself.
  const startedByLabel = startedBy(meta);
  const who = meta.parent_agent_type || meta.parent_agent_id || "the launching agent";

  // An eval session is an ordinary session that belongs to a comparison. The
  // membership is its own fetch rather than a field on the session row: it is
  // one extra request on a screen that already makes several, and it keeps an
  // eval lookup off the commit path that builds hub.SessionState. A 404 — an
  // ordinary session — renders nothing.
  const [membership, setMembership] = useState<EvalMembership | null>(null);
  useEffect(() => {
    let cancelled = false;
    getSessionEval(sessionId)
      .then((m) => {
        if (!cancelled) setMembership(m);
      })
      .catch(() => {
        if (!cancelled) setMembership(null);
      });
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  // --- the stop flow ---
  // The nav's Stop arms the same inline confirm strip the chat page uses —
  // in the footer band, not a modal over the transcript — and the banner
  // says *stopping…* between the 202 and the terminal event. A session that
  // stops being running resets the armed state.
  const [token, setToken] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    controlToken().then((t) => {
      if (!cancelled) setToken(t);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  const [confirmingStop, setConfirmingStop] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<string | null>(null);
  useEffect(() => {
    if (running) return;
    setConfirmingStop(false);
    setStopping(false);
    setStopError(null);
  }, [running]);

  const toggleStopConfirm = useCallback(() => setConfirmingStop((c) => !c), []);
  const cancelStop = useCallback(() => setConfirmingStop(false), []);
  const confirmStop = useCallback(async () => {
    if (stopping || token === null) return;
    setStopping(true);
    setStopError(null);
    try {
      await stopSession(sessionId, token, "stopped from the browser");
      setConfirmingStop(false);
    } catch (err) {
      setStopping(false);
      setConfirmingStop(false);
      setStopError(errorMessage(err));
    }
  }, [sessionId, token, stopping]);

  // --- follow the tail (.follow) ---
  // The stream opens pinned to the newest turn and stays pinned while new
  // turns arrive; scrolling up stops the following, the footer's toggle (and
  // the jump pill) resumes it. Same machinery as the chat page's jump pill.
  const streamRef = useRef<HTMLElement>(null);
  const followingRef = useRef(true);
  const [following, setFollowing] = useState(true);
  const streamTail = snapshot.items.length + (snapshot.live.turn?.content.length ?? 0) + snapshot.live.pendingTools.size;
  useEffect(() => {
    const el = streamRef.current;
    if (!el || !followingRef.current) return;
    el.scrollTop = el.scrollHeight;
  }, [streamTail]);
  const onStreamScroll = () => {
    const el = streamRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 8;
    if (followingRef.current !== atBottom) {
      followingRef.current = atBottom;
      setFollowing(atBottom);
    }
  };
  const toggleFollow = () => {
    if (!followingRef.current) {
      // Resuming follows the tail: pin and glide down.
      followingRef.current = true;
      setFollowing(true);
      streamRef.current?.scrollTo({ top: streamRef.current.scrollHeight, behavior: "smooth" });
    } else {
      followingRef.current = false;
      setFollowing(false);
    }
  };

  // --- the result ("the result the parent gets back") ---
  // The run_finished block renders as the result panel at the end of the
  // stream, through the same seam the chat page's sent messages use: the
  // watch page is the page the caller's payload comes back to, so the block
  // card gives way to the payload itself. Reference-stable across live-only
  // deltas so the memoised turn list keeps bailing out.
  const renderRunFinished = useCallback(
    (block: RunFinishedBlock) => <ResultPanel key={`${block.seq}-run_finished`} block={block} parentAgent={who} />,
    [who],
  );

  // --- the launching agent's instruction (.msg-user) ---
  // The instruction that started this run renders as its own message at the
  // top of the stream, attributed to the launcher: the same block shape the
  // chat page gives an operator's sent message, with the "from <agent>"
  // attribution in the state line (the fold's instruction block carries the
  // text; the launch is always the first user message, so it is delivered
  // at sub-turn 1). Reference-stable across live-only deltas, like the
  // result panel above.
  const renderInstruction = useCallback(
    (block: Extract<Block, { type: "instruction" }>) => (
      <div className={cn(MSG_CLS, MSG_USER_CLS)} key={`${block.seq}-instruction`}>
        <div className={MSG_BODY_CLS}>{block.text}</div>
        <div className={MSG_STATE_CLS}>from {who} · delivered · sub-turn 1</div>
      </div>
    ),
    [who],
  );

  // The plan-mini band (the narrow-width fallback): which plan item is
  // running, in one line above the stream, shown only
  // when the rail itself is hidden at narrow widths.
  const planMini = useMemo(() => {
    if (snapshot.todos.length === 0) return null;
    const inProgress = snapshot.todos.find((t) => t.status === "in_progress");
    const label = inProgress ? inProgress.activeForm : snapshot.todos.find((t) => t.status !== "completed")?.subject;
    if (!label) return null;
    const done = snapshot.todos.filter((t) => t.status === "completed").length;
    return { label, done, total: snapshot.todos.length };
  }, [snapshot.todos]);

  // The nav's right slot: the RUNNING badge with its pulse dot, the
  // elapsed time, the permission-mode badge — a safety fact, stated next
  // to the stop button — and the Stop control,
  // which arms the inline confirm strip in the footer. The connection badge
  // rides along while the run is live: it is the only thing on this screen
  // that says the stream is (or is not) still telling the page what happens
  // next. On a finished run the
  // footer is gone, so the slot carries what the footer's status line
  // carried — the outcome badge and the same five figures through the same
  // helpers, in the same order — and the run's wall time is frozen at
  // finished_at, as the footer's clock was.
  const navElapsed = running ? formatRunDuration(now - Date.parse(meta.created_at)) : null;
  const finishedNav = running
    ? null
    : {
        outcome: outcome(meta),
        status: watchStatusFigures(meta, snapshot.items, snapshot.live),
        elapsedMs: meta.finished_at ? Date.parse(meta.finished_at) - Date.parse(meta.created_at) : 0,
      };
  // The nav slot is where an outcome genuinely flips in place on this screen:
  // the RUNNING badge is replaced by the finished outcome while the page stays
  // open. The gate keeps a run that had already ended when the page loaded from
  // flipping in (hooks.ts useLabelFlip).
  const navOutcomeFlip = useLabelFlip(running ? "RUNNING" : finishedNav?.outcome.label ?? "RUNNING");
  useNavRight(
    <>
      {running && (
        <>
          <Badge key="running" variant="running" className={navOutcomeFlip}>
            <span className="h-1.5 w-1.5 flex-none rounded-full bg-current dot-pulse" aria-hidden />
            RUNNING
          </Badge>
          {navElapsed && <span>{navElapsed}</span>}
          <Badge
            variant="outline"
            className="border-[var(--status-gaveup)] font-semibold text-[var(--status-gaveup)]"
            title="This run can reach the host docker socket when full"
          >
            {meta.permission_mode}
          </Badge>
          {token !== null && (
            <Button
              variant="outline"
              size="sm"
              onClick={toggleStopConfirm}
              disabled={stopping}
              aria-expanded={confirmingStop}
              title={stopping ? "Stopping…" : "Stop"}
            >
              <StopCircle />
              <span className="max-nav:sr-only">{stopping ? "Stopping…" : "Stop"}</span>
            </Button>
          )}
        </>
      )}
      {finishedNav && (
        <>
          <Badge key={finishedNav.outcome.label} variant={finishedNav.outcome.variant} className={navOutcomeFlip}>
            {finishedNav.outcome.label}
          </Badge>
          <span className="statusline">
            {finishedNav.status.subTurn !== null ? (
              <>
                <span>sub-turn {finishedNav.status.subTurn}</span>
                <span className="sep">·</span>
                <span>{cachePercent(finishedNav.status.cacheHitTokens, finishedNav.status.cacheMissTokens)}% cache</span>
                <span className="sep">·</span>
                <span title="Price table captured by the server's pricing config">${formatCost(finishedNav.status.costUsd)}</span>
                <span className="sep">·</span>
                <span>{finishedNav.status.completionTokens.toLocaleString("en-US")} out</span>
                <span className="sep">·</span>
                <span>{formatRunDuration(finishedNav.elapsedMs)} elapsed</span>
              </>
            ) : (
              <>
                <span>{meta.model}</span>
                <span className="sep">·</span>
                <span>effort {meta.effort}</span>
              </>
            )}
          </span>
        </>
      )}
      {/* Only while the run is live. The badge answers "is the stream still
          telling this page what happens next", which is a question about a
          run that has more to say; on a finished run it reads "closed" for
          ever and sits next to the outcome badge competing with it. A stream
          that drops while it still mattered is the banner's job. */}
      {running && (
        <Badge
          variant="outline"
          className={cn(
            "text-muted-foreground",
            snapshot.connection === "open" && "border-[var(--status-done)] text-[var(--status-done)]",
          )}
        >
          {snapshot.connection}
        </Badge>
      )}
    </>,
  );

  return (
    <>
      <DroppedStreamBanner connection={snapshot.connection} everOpen={everOpen} />
      {planMini && (
        <div className="hidden max-watch:flex max-watch:items-center max-watch:gap-2.5 max-watch:border-b max-watch:border-border max-watch:bg-card max-watch:px-5 max-watch:py-[7px] max-watch:text-sm">
          <span className="flex-none font-mono text-[var(--status-running)]" aria-hidden>
            ▸
          </span>
          <span className="truncate">{planMini.label}</span>
          <span className="ml-auto flex-none text-micro text-muted-foreground tabular-nums">
            {planMini.done}/{planMini.total}
          </span>
        </div>
      )}
      <div
        className={cn(
          "grid min-h-0 flex-1 grid-cols-[244px_minmax(0,1fr)] max-watch:grid-cols-[minmax(0,1fr)]",
          !running && "work-watch-page",
        )}
      >
        <WatchRail
          items={snapshot.items}
          live={snapshot.live}
          todos={snapshot.todos}
          meta={meta}
          getToolCall={snapshot.getToolCall}
        />
        <main
          className="stream relative pt-5 pr-6 pl-10 [scrollbar-gutter:stable] max-watch:pl-6"
          ref={streamRef}
          onScroll={onStreamScroll}
        >
          <div className="mr-auto ml-0 max-w-[840px] pb-6 max-watch:ml-auto">
            {/* The run header leads the transcript column: what this run was
                called, who launched it, and the settings it ran under. It is
                unfilled and closed by a hairline, so the filled cards below
                it are the transcript rather than its frame.

                The run's own status is not repeated here. The nav carries the
                outcome badge and the run's figures, and a header that also
                said FINISHED stated one fact three times over — badge,
                sentence, nav. What a spectator may do is a live-run fact, so
                the note renders only while the run is going. */}
            <header className="mb-0">
              <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5">
                <h1 className="m-0 min-w-0 flex-[1_1_24ch] text-[1.0625rem] leading-[1.3] font-semibold text-foreground">
                  {meta.title || `Run ${sessionId.replace(/^sess-/, "").slice(0, 8)}`}
                </h1>
                {!running && (
                  <span className="text-xs whitespace-nowrap text-muted-foreground" title={new Date(meta.created_at).toString()}>
                    started {runWhen(meta.created_at)}
                  </span>
                )}
              </div>
              {meta.description && <p className="mt-1 mb-0 max-w-[78ch] text-sm leading-[1.45] text-muted-foreground">{meta.description}</p>}
              <div className="mt-2.5 flex min-w-0 flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted-foreground">
                {/* The launcher's own id is a claude-code session id: nothing
                    on this page or any other resolves it, so it rides on the
                    title rather than taking a third of the row. */}
                <Fact label="started by" value={who} title={startedByLabel ?? undefined} />
                {meta.job_type && <Fact label="job" value={meta.job_type} />}
                <Fact label="model" value={meta.model} />
                <Fact label="effort" value={meta.effort} />
                {/* A run that held the host docker socket says so on its own
                    face. The nav states it only while the run is live, which
                    left a finished full-permission run with nowhere that
                    said what it had been allowed to do. */}
                <Fact label="permission" value={meta.permission_mode} danger={meta.permission_mode === "full"} />
                {/* An eval puts the member's request id in parent_agent_id
                    (provenance.ts), so the strip printed the same
                    36-character id twice in a row — once inside "started by
                    eval (…)" and again as the request. Once is enough. */}
                {meta.request_id && meta.request_id !== meta.parent_agent_id && (
                  <Fact label="request" value={meta.request_id} mono />
                )}
                {membership && (
                  <>
                    <span className="inline-flex min-w-0 items-baseline gap-[5px]">
                      <span className="text-[0.625rem] tracking-[0.06em] text-muted-foreground uppercase">eval</span>
                      <a
                        className="text-[var(--status-running)] no-underline hover:underline max-phone:inline-flex max-phone:min-h-11 max-phone:items-center max-phone:px-1"
                        href={`/evals/${encodeURIComponent(membership.eval_run_id)}`}
                        onClick={(e) => {
                          e.preventDefault();
                          onNavigate(`/evals/${encodeURIComponent(membership.eval_run_id)}`);
                        }}
                      >
                        {membership.suite}
                      </a>
                    </span>
                    <Fact label="variant" value={membership.variant} />
                    <Fact label="task" value={membership.task_id} />
                    <Fact label="replicate" value={String(membership.replicate)} />
                  </>
                )}
              </div>
              {running && (
                <p className="mt-2.5 mb-0 text-xs text-muted-foreground">
                  {`This run takes its instructions from ${who}. You can stop it, but not message it.`}
                </p>
              )}
            </header>
            {snapshot.churnPoint && (
              <div className="mb-4 rounded-[calc(var(--radius)-2px)] border border-border border-l-[3px] border-l-[var(--status-gaveup)] bg-[var(--status-gaveup-bg)] px-3 py-2 text-sm text-foreground">
                <b>
                  Cache churn at sub-turn {snapshot.churnPoint.subTurn}: {snapshot.churnPoint.excessTokens.toLocaleString("en-US")} tokens
                  re-sent above the expected miss.
                </b>{" "}
                The prefix moved — see docs/CACHE.md.{" "}
                <a className="font-semibold text-[var(--status-gaveup)]" href={`#sub-turn-${snapshot.churnPoint.subTurn}`}>
                  Jump to it →
                </a>
              </div>
            )}
            <SessionIdContext.Provider value={sessionId}>
              <TurnTranscript
                replayed={snapshot.replayed}
                items={snapshot.items}
                live={snapshot.live}
                filter="all"
                getToolCall={snapshot.getToolCall}
                renderInstruction={renderInstruction}
                renderRunFinished={renderRunFinished}
              />
            </SessionIdContext.Provider>
          </div>
          {/* The jump pill is for a live run; a finished session's stream has
              no tail left to jump to. */}
          {running && !following && (
            <div className="pointer-events-none sticky bottom-2.5 flex h-0 items-end justify-center">
              <button
                type="button"
                className="pointer-events-auto inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-full border border-border bg-card px-3 text-xs text-foreground [box-shadow:var(--shadow-float)] max-phone:h-11 max-phone:px-4"
                onClick={toggleFollow}
              >
                <ArrowDown className="h-[13px] w-[13px] flex-none" aria-hidden />
                Jump to live
              </button>
            </div>
          )}
        </main>
      </div>
      {/* The footer is the live run's status bar — what it is doing right
          now, and the stop flow — and renders only while the run is live. A
          finished run is an ordinary page: no footer, the figures in the
          nav's right slot (SessionScreen drops the app shell for it). */}
      {running && (
        <WatchFooter
          meta={meta}
          items={snapshot.items}
          live={snapshot.live}
          following={following}
          onToggleFollow={toggleFollow}
          stop={{
            confirming: confirmingStop,
            stopping,
            error: stopError,
            onCancelStop: cancelStop,
            onConfirmStop: () => void confirmStop(),
          }}
          getToolCall={snapshot.getToolCall}
        />
      )}
    </>
  );
}

// runWhen is the run's wall-clock date, which no part of this page carried
// before: every figure on the screen was a duration, so a run from this
// morning and one from three weeks ago read identically.
function runWhen(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

// Fact is one labelled fact in the run header: a dim key and its value. Only
// an identifier takes the mono chip — a word like "implementation" or "max"
// is prose, and chipping it made six settings look like six ids.
function Fact({
  label,
  value,
  mono,
  danger,
  title,
}: {
  label: string;
  value: string;
  mono?: boolean;
  danger?: boolean;
  title?: string;
}) {
  return (
    <span className="inline-flex min-w-0 items-baseline gap-[5px]">
      <span className="text-[0.625rem] tracking-[0.06em] text-muted-foreground uppercase">{label}</span>
      {/* The chip clips a long id to keep the strip one line; the title is
          how the whole of it is still available. */}
      {mono ? (
        <code
          className="max-w-[16ch] truncate rounded-[calc(var(--radius)-4px)] border border-border bg-muted px-[5px] py-px text-micro text-foreground"
          title={title ?? value}
        >
          {value}
        </code>
      ) : (
        <span className={cn("truncate font-medium text-foreground", danger && "text-[var(--status-gaveup)]")} title={title ?? value}>
          {value}
        </span>
      )}
    </span>
  );
}
