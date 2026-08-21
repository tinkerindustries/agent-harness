import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, StopCircle } from "@phosphor-icons/react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptSnapshot } from "../api/transcriptStore";
import { TurnTranscript } from "./turns/TurnTranscript";
import { SteerMessage, type SteerBlock, type SteerWait } from "./turns/SteerMessage";
import { finishedBandText, formatRunDuration } from "./turns/turnHelpers";
import { controlToken, errorMessage, resumeSession, steerSession, stopSession, type RunAttachment } from "../api/operations";
import { attachmentCapsFromSettings, type AttachmentCaps } from "../api/attachments";
import { listSettings } from "../api/settings";
import { canResume, canSteer, isLive } from "../api/status";
import { isUserStarted } from "../api/provenance";
import { SessionIdContext, useNow } from "../hooks";
import { MSG_BODY_CLS, MSG_CLS, MSG_STATE_CLS, MSG_USER_CLS } from "./turns/SteerMessage";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { ChatComposer, type ComposerStatus, type FinishedBand } from "./ChatComposer";
import { ChatRail } from "./ChatRail";
import { DroppedStreamBanner } from "./DroppedStreamBanner";
import { outcome, type OutcomeSession } from "./statusBadge";
import { ClosingMessage, isCleanFinish } from "./blocks/MiscBlocks";
import { ScreenshotGallery } from "./blocks/ScreenshotGallery";
import { toolDetail } from "./blocks/toolArgs";
import { useNavRight } from "./TopNav";

interface Props {
  sessionId: string;
  // The metadata row the fork read before choosing this screen — null only
  // when the row's fetch failed and the fork kept this screen as the safe
  // default (SessionScreen): a person looking at their own run must never
  // lose the ability to steer it because a fetch failed.
  meta: SessionState | null;
  snapshot: TranscriptSnapshot;
  // The app's navigate, for the finished band's follow-up run (back to the
  // session list with the start form open).
  onNavigate: (path: string) => void;
  // Whether this session's stream has opened since the page loaded
  // (SessionScreen's per-session flag, for the dropped-stream banner).
  everOpen: boolean;
}

// The interactive session page: a run a person started, inside the
// full-height app shell the two session pages share. This is the
// page a person talks to: the composer footer that never moves, sent
// messages rendered as .msg-user in the
// conversation, the plan rail on the right, the states around the run
// (empty before the first message, the finished band after), the inline stop
// confirmation, and the nav slot the design's header draws. The watch page
// is a separate screen; this screen owns none of its rail, chips, or timeline.
export function SessionChatScreen({ sessionId, meta, snapshot, onNavigate, everOpen }: Props) {
  const now = useNow(1000);
  // The composer is steerability, not liveness: the run is steerable until
  // the row says otherwise — and when the row never arrived, the safe
  // default is steerable, so the composer stays usable. A "creating" row is
  // live but has no loop to read a steer yet, so canSteer (not isLive) is
  // what gates the stop controls and the queued line: they stay off while the
  // workspace is being built.
  const running = meta === null || canSteer(meta.status);
  // The other half: the run is over and this session can be continued
  // (docs/RUN-CONTROL.md "Continuing"). The box renders for either, and which
  // of the two is true is what a typed message becomes — a steer or a resume.
  // Between them they cover every status but "creating" and "compacted".
  const resumable = meta !== null && canResume(meta.status);

  // The run-control bearer, fetched once per page load (controlToken caches
  // its promise) and shared by the steer POST and the stop POST. A null
  // token hides the composer box and the stop button exactly as it hid
  // SteerControl and StopControl: run control not configured must look
  // unavailable, not broken.
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

  // The caps a pasted image is checked against, read once per page load from
  // the settings registry — the same two the start form reads and the steer
  // and resume endpoints enforce, so an operator who changes
  // tools.attachments_max_count or tools.attachments_max_bytes sees the
  // composer change with them. A failed fetch leaves them null, which the
  // composer renders as "images cannot be attached" while the box keeps
  // taking text: the caps are the images' precondition, not the message's.
  const [attachmentCaps, setAttachmentCaps] = useState<AttachmentCaps | null>(null);
  useEffect(() => {
    let cancelled = false;
    listSettings()
      .then((entries) => {
        if (!cancelled) setAttachmentCaps(attachmentCapsFromSettings(entries));
      })
      .catch(() => {
        // Deliberately silent: nothing on this page is broken by it, and the
        // composer already says what a person can and cannot do without it.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // --- the sent-message ledger (three states)
  // Two of the three states live in the fold's steer blocks; the ledger
  // completes them with what the fold cannot know. sentAt holds the local
  // time of each 202 this page accepted, so a pending message can count up
  // from when it was actually sent; failed holds the sends whose POST itself
  // failed — nothing landed in the log, so no block will ever appear, and
  // the chat screen holds them until retried or dismissed.
  const [sentAt, setSentAt] = useState<ReadonlyMap<number, number>>(new Map());
  interface FailedSend {
    id: number;
    text: string;
    // The images that went with it, kept so Retry re-sends the whole message
    // rather than the words of it — a screenshot the person pasted is half of
    // what they said, and a retry that quietly dropped it would send
    // something they never wrote.
    attachments: RunAttachment[];
    error: string;
  }
  const [failed, setFailed] = useState<FailedSend[]>([]);
  const failedId = useRef(0);

  // postMessage is the one write behind the composer's send and the failed
  // message's Retry, and the single place the page decides which verb a typed
  // message is. A live run reads it at its next sub-turn boundary (a steer);
  // a finished one is continued by it (a resume). The decision is made here,
  // at the moment of sending, rather than by the composer, so the box below
  // is one control with one callback.
  //
  // Neither acceptance is a delivery, and each says so in its own way. A
  // steer's 202 carries the seq its event landed at, which goes into the
  // ledger so the pending block can count up from when this browser sent it.
  // A resume's does not: the continuation is not waiting on a boundary, it is
  // waiting for a worker to claim it, and it appears in the transcript as a
  // continuation block when the resumed run starts. Nothing here polls or
  // guesses at either.
  const postMessage = useCallback(
    async (text: string, attachments: RunAttachment[]): Promise<{ ok: boolean; error: string | null }> => {
      if (token === null) return { ok: false, error: "run control is not configured" };
      try {
        if (!running && resumable) {
          await resumeSession(sessionId, token, text, attachments);
          return { ok: true, error: null };
        }
        const res = await steerSession(sessionId, token, text, attachments);
        setSentAt((prev) => {
          const m = new Map(prev);
          m.set(res.seq, Date.now());
          return m;
        });
        return { ok: true, error: null };
      } catch (err) {
        return { ok: false, error: errorMessage(err) };
      }
    },
    [sessionId, token, running, resumable],
  );

  const send = useCallback(
    async (text: string, attachments: RunAttachment[]): Promise<boolean> => {
      const res = await postMessage(text, attachments);
      if (!res.ok) {
        setFailed((prev) => [...prev, { id: failedId.current++, text, attachments, error: res.error ?? "send failed" }]);
        return false;
      }
      return true;
    },
    [postMessage],
  );

  const retryFailed = useCallback(
    async (failure: FailedSend) => {
      // A retry is the same POST, not a fresh send: only its own entry
      // disappears on success, and a second refusal keeps the entry rather
      // than stacking a duplicate failed message.
      const res = await postMessage(failure.text, failure.attachments);
      if (res.ok) setFailed((prev) => prev.filter((f) => f.id !== failure.id));
    },
    [postMessage],
  );

  const dismissFailed = useCallback((id: number) => {
    setFailed((prev) => prev.filter((f) => f.id !== id));
  }, []);

  // --- the stop flow ---
  // Stop is behind a confirmation because a run you cannot get back is hard
  // to reverse; the strip is inline in the footer, not a modal over the
  // transcript. Between the 202 and the terminal event the banner says
  // *stopping…*: the loop finishes its current tool call first, and the
  // stream delivers the terminal state. A session that stops being running
  // resets the armed state — the terminal event has arrived, the run is
  // stopped rather than stopping.
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
      // The reason is carried verbatim into the cancelled result; the
      // browser has no free-text field for it, so it says where the stop
      // came from, exactly as StopControl does.
      await stopSession(sessionId, token, "stopped from the browser");
      setConfirmingStop(false);
      // stopping stays true: the run is still ending, and the stream will
      // deliver the terminal state.
    } catch (err) {
      setStopping(false);
      setConfirmingStop(false);
      setStopError(errorMessage(err));
    }
  }, [sessionId, token, stopping]);

  // --- derived view over the snapshot ---
  // The sub-turn the footer names: the live turn, or the last frozen one.
  const status: ComposerStatus | null = useMemo(() => {
    if (!meta) return null;
    let subTurn: number | null = snapshot.live.turn?.subTurn ?? null;
    if (subTurn === null) {
      for (let i = snapshot.items.length - 1; i >= 0; i--) {
        const item = snapshot.items[i];
        if (item.kind === "group") {
          subTurn = item.group.subTurn;
          break;
        }
      }
    }
    return {
      subTurn,
      cacheHitTokens: meta.usage.cache_hit_tokens,
      cacheMissTokens: meta.usage.cache_miss_tokens,
      costUsd: meta.usage.cost_usd,
      completionTokens: meta.usage.completion_tokens,
    };
  }, [meta, snapshot.live.turn?.subTurn, snapshot.items]);

  // The delivered sub-turn per steer block lives on the block itself
  // (fold.ts: steer_applied stamps appliedSubTurn when the block flips to
  // delivered), so the chat screen carries nothing derived from transcript
  // order — the number the producer stamped is the boundary the message
  // became a user message at, which counting groups cannot recover when the
  // steer was sent mid-turn.

  // What pending steers are waiting on, as primitives (turnHelpers
  // pendingWaitLabel): reference-stable across live-only deltas, so the
  // memoised turn list below keeps bailing out at token rate.
  const wait = useMemo<SteerWait>(
    () => ({ hasToolRound: snapshot.live.pendingTools.size > 0, liveSubTurn: snapshot.live.turn?.subTurn ?? null }),
    [snapshot.live.pendingTools.size, snapshot.live.turn?.subTurn],
  );

  const pendingCount = useMemo(
    () =>
      snapshot.items.filter(
        (i) => i.kind === "block" && i.block.type === "steer" && i.block.state === "pending",
      ).length,
    [snapshot.items],
  );

  // renderSteer is reference-stable across live-only deltas — it changes
  // only when the ledger or the wait primitives do — so the memoised
  // TurnList bails out on every token (web/CLAUDE.md: preserve the
  // group-granularity memoisation). runEnded joins the deps: a steer still
  // pending when the run ends will never be applied, and the message's
  // state line must stop claiming it is waiting once the run has ended —
  // otherwise a steer that never got applied renders as pending forever.
  const renderSteer = useCallback(
    (block: SteerBlock) => (
      <SteerMessage key={block.seq} block={block} sentAt={sentAt.get(block.seq)} wait={wait} runEnded={!running} />
    ),
    [sentAt, wait, running],
  );

  // A message somebody typed to continue this session, rendered the way a
  // sent message is rendered everywhere on this page: the .msg-user shell,
  // no state line. There is nothing for a state line to say — a continuation
  // is not waiting on a sub-turn boundary the way a steer is; the run it
  // opened is already underway by the time the block exists.
  const renderContinuation = useCallback(
    (block: Extract<Block, { type: "continuation" }>) => (
      <div className={cn(MSG_CLS, MSG_USER_CLS)} key={`${block.seq}-continuation`}>
        <div className={MSG_BODY_CLS}>{block.text}</div>
        {/* The images that rode the message, through the same gallery every
            other workspace image on this page uses — a continuation's
            attachments are written into the workspace before the message is
            appended, so by the time this block exists the files are there. */}
        <ScreenshotGallery paths={block.attachments} />
      </div>
    ),
    [],
  );

  // A run ending is a turn boundary on this page, not an event in its own
  // right. When the outcome is clean, the model's closing words render as
  // the model's closing words — no banner, no outcome label, no cost line —
  // because that text is the reply, and the chrome around it is what makes a
  // conversation read as a series of jobs. The numbers it carried are all
  // still on the page: the status line under the composer, the nav's badge,
  // and the session list.
  //
  // Anything but a clean outcome keeps the full card. That is the moment the
  // reason matters — a run that gave up, hit its sub-turn ceiling or was
  // stopped has something to say beyond its last sentence, and quietly
  // eliding it would leave the reader wondering why the model stopped
  // mid-thought.
  const renderRunFinished = useCallback((block: Extract<Block, { type: "run_finished" }>) => {
    if (!isCleanFinish(block)) return null;
    const text = block.text || block.summary;
    if (!text) return null;
    return <ClosingMessage key={`${block.seq}-run_finished`} text={text} />;
  }, []);

  // What the run is doing right now, for the confirm strip's sentence
  // ("It is 4m 12s in, mid `scripts/test.sh`").
  const activity = useMemo(() => {
    if (!meta || !running) return null;
    let detail = "between sub-turns";
    if (snapshot.live.pendingTools.size > 0) {
      const calls = [...snapshot.live.pendingTools.values()].map((p) => p.call);
      const last = calls[calls.length - 1];
      const target = last ? toolDetail(last) : "";
      detail = `mid ${target || last?.name || "a tool call"}`;
    } else if (snapshot.live.turn) {
      detail = `mid sub-turn ${snapshot.live.turn.subTurn}`;
    }
    return { elapsedMs: now - Date.parse(meta.created_at), detail };
  }, [meta, running, snapshot.live.pendingTools, snapshot.live.turn, now]);

  // The finished band ("the run is over"): the composer is removed
  // rather than disabled, and the outcome, duration and
  // cost take its place. The duration is the row's own finished_at −
  // created_at — the wall time the run actually ran, not a guess.
  // The band is gated on liveness, not on the composer's canSteer: a
  // "creating" run is not steerable yet, but it is not over either, so it
  // must show neither the box nor the band while the workspace is being
  // built.
  const finished: FinishedBand | null = useMemo(() => {
    if (!meta || isLive(meta.status)) return null;
    // A session that can be continued keeps its composer, so there is no slot
    // for the band and no need for its follow-up button: the way on is to
    // type. What is left here is a session that can be continued by nothing —
    // one retired by compaction (docs/RUN-CONTROL.md "Continuing").
    if (canResume(meta.status)) return null;
    const o = outcome(headerOutcomeSession(meta, snapshot.blocks));
    const durationMs =
      meta.finished_at && meta.created_at ? Date.parse(meta.finished_at) - Date.parse(meta.created_at) : 0;
    return {
      label: o.label,
      variant: o.variant,
      text: finishedBandText(meta.status, durationMs, meta.sub_turns, meta.usage.cost_usd, o.label),
    };
  }, [meta, snapshot.blocks]);

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

  // The empty state ("before the first message"): POST /api/runs creates
  // a run with no prompt, so a claimed session with
  // zero sub-turns is normal, not an error.
  const hasTurns = snapshot.counts.total > 0 || snapshot.live.turn !== null;
  const showEmpty = running && !hasTurns;

  // The nav's right slot for this screen: the RUNNING badge with its
  // pulse dot, the elapsed time, the
  // permission-mode badge — a safety fact, stated next to the stop button —
  // and the Stop control, which arms the inline confirm strip in the footer.
  // The connection badge stays: it is the only thing on this screen that
  // says the stream is (or is not) still telling the page what happens next.
  const navElapsed = meta && running ? formatRunDuration(now - Date.parse(meta.created_at)) : null;
  useNavRight(
    <>
      {running && (
        <>
          <Badge variant="running">
            <span className="h-1.5 w-1.5 flex-none rounded-full bg-current dot-pulse" aria-hidden />
            RUNNING
          </Badge>
          {navElapsed && <span>{navElapsed}</span>}
          {meta && (
            <Badge
              variant="outline"
              className="border-[var(--status-gaveup)] font-semibold text-[var(--status-gaveup)]"
              title="This run can reach the host docker socket when full"
            >
              {meta.permission_mode}
            </Badge>
          )}
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
      <Badge
        variant="outline"
        className={cn(
          "text-muted-foreground",
          snapshot.connection === "open" && "border-[var(--status-done)] text-[var(--status-done)]",
        )}
      >
        {snapshot.connection}
      </Badge>
    </>,
  );

  // --- follow the tail (.jump) ---
  // The stream opens pinned to the newest turn and stays pinned while new
  // turns arrive; scrolling up stops the following and shows the jump pill;
  // clicking it (or scrolling back to the bottom by hand) resumes it.
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
  const jumpToTail = () => {
    followingRef.current = true;
    setFollowing(true);
    streamRef.current?.scrollTo({ top: streamRef.current.scrollHeight, behavior: "smooth" });
  };

  const followUp = useCallback(() => {
    // A follow-up run starts from the same form every run starts from: the
    // session list's start panel. The workspace is freshly prepared for the
    // run — the start API has no workspace-reuse — so the form is where the
    // operator picks the repos again.
    onNavigate("/#start");
  }, [onNavigate]);

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
      <div className="grid min-h-0 flex-1 grid-cols-[minmax(0,1fr)_288px] max-watch:grid-cols-[minmax(0,1fr)]">
        <main className="stream relative pt-5 px-6 [scrollbar-gutter:stable]" ref={streamRef} onScroll={onStreamScroll}>
          <div className="mx-auto max-w-[800px] pb-6">
            {showEmpty && (
              <div className="px-5 pt-10 pb-7 text-center text-muted-foreground">
                <h3 className="m-0 mb-1.5 text-base font-semibold text-foreground">Waiting for your first message</h3>
                <p className="mx-auto mb-0 max-w-[46ch] text-sm leading-[1.6]">
                  The workspace is prepared and a worker has claimed this run. Nothing has been sent to the model yet
                  — your first message is the task.
                </p>
              </div>
            )}
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
                renderSteer={renderSteer}
                renderContinuation={renderContinuation}
                renderRunFinished={renderRunFinished}
              />
            </SessionIdContext.Provider>
            {failed.map((f) => (
              <div className={cn(MSG_CLS, MSG_USER_CLS, "border-l-[var(--status-failed)]")} key={f.id}>
                <div className={MSG_BODY_CLS}>{f.text}</div>
                <div className={cn(MSG_STATE_CLS, "text-[var(--status-failed)]")}>
                  not sent
                  {/* The images are still held against this entry, so Retry
                      re-sends the whole message. Saying so is the difference
                      between a retry somebody trusts and one they redo by
                      hand. */}
                  {f.attachments.length > 0 &&
                    ` · ${f.attachments.length} image${f.attachments.length === 1 ? "" : "s"} still attached`}
                  {" — "}
                  {f.error}
                  <Button variant="ghost" size="sm" className="h-[22px] px-2 text-xs" onClick={() => void retryFailed(f)}>
                    Retry
                  </Button>
                  <button
                    type="button"
                    className="ml-auto cursor-pointer rounded-[3px] border-0 bg-transparent px-1 py-0.5 text-xs leading-none text-muted-foreground hover:bg-accent hover:text-foreground max-phone:inline-flex max-phone:min-h-11 max-phone:min-w-11 max-phone:items-center max-phone:justify-center"
                    onClick={() => dismissFailed(f.id)}
                    aria-label="Dismiss"
                  >
                    ✕
                  </button>
                </div>
              </div>
            ))}
          </div>
          {/* The jump pill is for a live run; a finished session's stream has
              no tail left to jump to, and the pill offered to follow a run
              that had already ended. Same guard the watch page carries. */}
          {running && !following && (
            <div className="pointer-events-none sticky bottom-2.5 flex h-0 items-end justify-center">
              <button
                type="button"
                className="pointer-events-auto inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-full border border-border bg-card px-3 text-xs text-foreground [box-shadow:var(--shadow-float)] max-phone:h-11 max-phone:px-4"
                onClick={jumpToTail}
              >
                <ArrowDown className="h-[13px] w-[13px] flex-none" aria-hidden />
                Jump to live
              </button>
            </div>
          )}
        </main>
        {meta && (
          <ChatRail
            todos={snapshot.todos}
            facts={{
              model: meta.model,
              effort: meta.effort,
              startedBy: chatStartedBy(meta),
              workspace: meta.workspace,
              requestId: meta.request_id,
            }}
          />
        )}
      </div>
      <ChatComposer
        token={token}
        running={running}
        resumable={resumable}
        pendingCount={pendingCount}
        send={send}
        attachmentCaps={attachmentCaps}
        stop={{ confirming: confirmingStop, stopping, error: stopError, onRequestStop: toggleStopConfirm, onCancelStop: cancelStop, onConfirmStop: () => void confirmStop() }}
        activity={activity}
        status={status}
        facts={
          meta
            ? { model: meta.model, effort: meta.effort, permission: meta.permission_mode }
            : { model: "", effort: "", permission: "" }
        }
        finished={finished}
        onFollowUp={followUp}
      />
    </>
  );
}

// chatStartedBy is the rail's "started by" value ("you"): the bare
// identity, without the "started by " prefix the one-line
// provenance label carries — the fact row already says what the field is.
function chatStartedBy(meta: SessionState): string {
  if (isUserStarted(meta)) return meta.parent_agent_id || "you";
  if (meta.parent_agent_type) return meta.parent_agent_id ? `${meta.parent_agent_type} (${meta.parent_agent_id})` : meta.parent_agent_type;
  return "—";
}

// headerOutcomeSession is the outcome() input for the finished band: the
// metadata row plus the run_finished block's reason, when the fold has one.
// The metadata endpoint carries complete_status but not run_finished's
// reason, so STOPPED (answered in prose, no Complete call) is only
// distinguishable on this screen, straight from the folded event log.
function headerOutcomeSession(meta: SessionState, blocks: Block[]): OutcomeSession {
  return { status: meta.status, complete_status: meta.complete_status, reason: lastRunFinishedReason(blocks) };
}

function lastRunFinishedReason(blocks: Block[]): string | undefined {
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i];
    if (b.type === "run_finished") return b.reason;
  }
  return undefined;
}
