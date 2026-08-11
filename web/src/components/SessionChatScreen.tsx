import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptSnapshot } from "../api/transcriptStore";
import { TurnTranscript } from "./turns/TurnTranscript";
import { SteerMessage, type SteerBlock, type SteerWait } from "./turns/SteerMessage";
import { finishedBandText, formatRunDuration, steerDeliveredSubTurn } from "./turns/turnHelpers";
import { controlToken, errorMessage, steerSession, stopSession } from "../api/operations";
import { isUserStarted } from "../api/provenance";
import { useNow } from "../hooks";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { ChatComposer, type ComposerStatus, type FinishedBand } from "./ChatComposer";
import { ChatRail } from "./ChatRail";
import { outcome, type OutcomeSession } from "./statusBadge";
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
}

// The interactive session page (design/session-chat.html): a run a person
// started, inside the full-height app shell the two session pages share
// (design/session.css). Phase 3 is the page a person talks to: the composer
// footer that never moves, sent messages rendered as .msg-user in the
// conversation, the plan rail on the right, the states around the run
// (empty before the first message, the finished band after), the inline stop
// confirmation, and the nav slot the design's header draws. The watch page
// is phase 4's; this screen owns none of its rail, chips, or timeline.
export function SessionChatScreen({ sessionId, meta, snapshot, onNavigate }: Props) {
  const now = useNow(1000);
  // The run is live until the row says otherwise — and when the row never
  // arrived, the safe default is live, so the composer stays steerable.
  const running = meta === null || meta.status === "running";

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

  // --- the sent-message ledger (design/session-states.html's three states)
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
    error: string;
  }
  const [failed, setFailed] = useState<FailedSend[]>([]);
  const failedId = useRef(0);

  const send = useCallback(
    async (text: string): Promise<boolean> => {
      if (token === null) return false;
      try {
        // The 202 is the acceptance, not the delivery: the text appears in
        // the transcript as a pending steer block via the SSE stream the
        // moment the steer_message event lands, and flips to delivered when
        // the loop applies it. Nothing here polls or guesses at that.
        const res = await steerSession(sessionId, token, text);
        setSentAt((prev) => {
          const m = new Map(prev);
          m.set(res.seq, Date.now());
          return m;
        });
        return true;
      } catch (err) {
        setFailed((prev) => [...prev, { id: failedId.current++, text, error: errorMessage(err) }]);
        return false;
      }
    },
    [sessionId, token],
  );

  const retryFailed = useCallback(
    async (id: number, text: string) => {
      if (await send(text)) setFailed((prev) => prev.filter((f) => f.id !== id));
    },
    [send],
  );

  const dismissFailed = useCallback((id: number) => {
    setFailed((prev) => prev.filter((f) => f.id !== id));
  }, []);

  // --- the stop flow (design/session-states.html "stopping") ---
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

  // The delivered sub-turn per steer block, derived from transcript order
  // (turnHelpers.steerDeliveredSubTurn) — computed once per items change and
  // memoised on primitives so the token-rate hot path never recomputes it.
  const deliveredSubTurns = useMemo(() => {
    const liveSubTurn = snapshot.live.turn?.subTurn ?? null;
    const m = new Map<number, number | null>();
    for (const item of snapshot.items) {
      if (item.kind === "block" && item.block.type === "steer" && item.block.state === "delivered") {
        m.set(item.block.seq, steerDeliveredSubTurn(item.block.seq, snapshot.items, liveSubTurn));
      }
    }
    return m;
  }, [snapshot.items, snapshot.live.turn?.subTurn]);

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
  // only when the ledger, the delivered map, or the wait primitives do — so
  // the memoised TurnList bails out on every token (web/CLAUDE.md: preserve
  // the group-granularity memoisation).
  const renderSteer = useCallback(
    (block: SteerBlock) => (
      <SteerMessage
        key={block.seq}
        block={block}
        sentAt={sentAt.get(block.seq)}
        deliveredSubTurn={deliveredSubTurns.get(block.seq) ?? null}
        wait={wait}
      />
    ),
    [sentAt, deliveredSubTurns, wait],
  );

  // What the run is doing right now, for the confirm strip's sentence
  // (design/session-states.html: "It is 4m 12s in, mid `scripts/test.sh`").
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

  // The finished band (design/session-states.html "the run is over"): the
  // composer is removed rather than disabled, and the outcome, duration and
  // cost take its place. The duration is the row's own finished_at −
  // created_at — the wall time the run actually ran, not a guess.
  const finished: FinishedBand | null = useMemo(() => {
    if (!meta || running) return null;
    const o = outcome(headerOutcomeSession(meta, snapshot.blocks));
    const durationMs =
      meta.finished_at && meta.created_at ? Date.parse(meta.finished_at) - Date.parse(meta.created_at) : 0;
    return {
      label: o.label,
      variant: o.variant,
      text: finishedBandText(meta.status, durationMs, meta.sub_turns, meta.usage.cost_usd, o.label),
    };
  }, [meta, running, snapshot.blocks]);

  // The plan-mini band (design/session-chat.html's narrow-width fallback):
  // which plan item is running, in one line above the stream, shown only
  // when the rail itself is hidden at narrow widths.
  const planMini = useMemo(() => {
    if (snapshot.todos.length === 0) return null;
    const inProgress = snapshot.todos.find((t) => t.status === "in_progress");
    const label = inProgress ? inProgress.activeForm : snapshot.todos.find((t) => t.status !== "completed")?.subject;
    if (!label) return null;
    const done = snapshot.todos.filter((t) => t.status === "completed").length;
    return { label, done, total: snapshot.todos.length };
  }, [snapshot.todos]);

  // The empty state (design/session-states.html "before the first message"):
  // POST /api/runs creates a run with no prompt, so a claimed session with
  // zero sub-turns is normal, not an error.
  const hasTurns = snapshot.counts.total > 0 || snapshot.live.turn !== null;
  const showEmpty = running && !hasTurns;

  // The nav's right slot for this screen (design/session-chat.html's
  // header): the RUNNING badge with its pulse dot, the elapsed time, the
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
            <span className="dot dot-pulse" aria-hidden />
            RUNNING
          </Badge>
          {navElapsed && <span>{navElapsed}</span>}
          {meta && (
            <Badge variant="outline" className="badge-perm" title="This run can reach the host docker socket when full">
              {meta.permission_mode}
            </Badge>
          )}
          {token !== null && (
            <Button variant="outline" size="sm" onClick={toggleStopConfirm} disabled={stopping} aria-expanded={confirmingStop}>
              {stopping ? "Stopping…" : "Stop"}
            </Button>
          )}
        </>
      )}
      <Badge variant="outline" className={`connection-badge connection-${snapshot.connection}`}>
        {snapshot.connection}
      </Badge>
    </>,
  );

  // --- follow the tail (design/session-chat.html's .jump) ---
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
      {planMini && (
        <div className="plan-mini">
          <span className="mark" aria-hidden>
            ▸
          </span>
          <span className="truncate">{planMini.label}</span>
          <span className="count">
            {planMini.done}/{planMini.total}
          </span>
        </div>
      )}
      <div className="work work-chat">
        <main className="stream" ref={streamRef} onScroll={onStreamScroll}>
          <div className="stream-inner">
            {showEmpty && (
              <div className="empty">
                <h3>Waiting for your first message</h3>
                <p>
                  The workspace is prepared and a worker has claimed this run. Nothing has been sent to the model yet
                  — your first message is the task.
                </p>
              </div>
            )}
            {snapshot.churnPoint && (
              <div className="notice churn-banner">
                <b>
                  Cache churn at sub-turn {snapshot.churnPoint.subTurn}: {snapshot.churnPoint.excessTokens.toLocaleString("en-US")} tokens
                  re-sent above the expected miss.
                </b>{" "}
                The prefix moved — see docs/CACHE.md. <a href={`#sub-turn-${snapshot.churnPoint.subTurn}`}>Jump to it →</a>
              </div>
            )}
            <TurnTranscript
              items={snapshot.items}
              live={snapshot.live}
              filter="all"
              getToolCall={snapshot.getToolCall}
              renderSteer={renderSteer}
            />
            {failed.map((f) => (
              <div className="msg msg-user msg-user-failed" key={f.id}>
                <div className="body">{f.text}</div>
                <div className="state state-failed">
                  not sent — {f.error}
                  <Button variant="ghost" size="sm" className="retry" onClick={() => void retryFailed(f.id, f.text)}>
                    Retry
                  </Button>
                  <button type="button" className="dismiss" onClick={() => dismissFailed(f.id)} aria-label="Dismiss">
                    ✕
                  </button>
                </div>
              </div>
            ))}
          </div>
          {!following && (
            <div className="jumpwrap">
              <button type="button" className="jump" onClick={jumpToTail}>
                ↓ Jump to live
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
        pendingCount={pendingCount}
        send={send}
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

// chatStartedBy is the rail's "started by" value (design/session-chat.html's
// "you"): the bare identity, without the "started by " prefix the one-line
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
// distinguishable on this screen, straight from the folded event log
// (design/components.html).
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
