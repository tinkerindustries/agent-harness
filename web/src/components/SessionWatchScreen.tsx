import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { SessionState } from "../api/types";
import type { Block } from "../api/fold";
import type { TranscriptFilter } from "../api/groups";
import type { TranscriptSnapshot } from "../api/transcriptStore";
import { TurnTranscript } from "./turns/TurnTranscript";
import { WatchRail } from "./WatchRail";
import { WatchFooter } from "./WatchFooter";
import { ResultPanel, type RunFinishedBlock } from "./ResultPanel";
import { DroppedStreamBanner } from "./DroppedStreamBanner";
import { controlToken, errorMessage, stopSession } from "../api/operations";
import { startedBy } from "../api/provenance";
import { useNow } from "../hooks";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { formatRunDuration } from "./turns/turnHelpers";
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

// The read-only session page (design/session-watch.html): a run another
// agent started, inside the full-height app shell the two session pages
// share. You may stop it; you may not talk to it — its instructions come
// from the agent that launched it, and the page shows no way to send a
// message, not even a disabled one (a greyed-out input invites you to look
// for the way to enable it). Phase 4 is this page: the provenance strip
// under the nav, the navigator rail on the left (find, chips, the plan as
// phases with one tick per sub-turn), the live footer answering what the
// run is doing right now, the result the parent gets back at the end of the
// stream, and the dropped-stream banner both pages share.
export function SessionWatchScreen({ sessionId, meta, snapshot, onNavigate, everOpen }: Props) {
  const now = useNow(1000);
  const running = meta.status === "running";

  // The one-line provenance label ("started by claude-code (sess-1)") and
  // the bare identity the sentence below names. provenance.ts owns the
  // label's shape (web/src/api/provenance.ts startedBy), including the
  // legacy fallback; the strip never re-formats the fields itself.
  const startedByLabel = startedBy(meta);
  const who = meta.parent_agent_type || meta.parent_agent_id || "the launching agent";

  // The filter starts at All and the find box blank; both are plain values,
  // so the memoised turn list compares them by value and still bails out on
  // every live-only delta — toggling a chip or typing is the deliberate
  // re-render.
  const [filter, setFilter] = useState<TranscriptFilter>("all");
  const [query, setQuery] = useState("");

  // --- the stop flow (design/session-states.html "stopping") ---
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

  // --- follow the tail (design/session-watch.html's .follow) ---
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

  // --- the result (design/session-states.html "the result the parent gets
  // back") ---
  // The run_finished block renders as the result panel at the end of the
  // stream, through the same seam the chat page's sent messages use: the
  // watch page is the page the caller's payload comes back to, so the block
  // card gives way to the payload itself. Reference-stable across live-only
  // deltas so the memoised turn list keeps bailing out.
  const renderRunFinished = useCallback(
    (block: RunFinishedBlock) => <ResultPanel key={`${block.seq}-run_finished`} block={block} parentAgent={who} />,
    [who],
  );

  // --- the launching agent's instruction (design/session-watch.html's
  // .msg-user) ---
  // The instruction that started this run renders as its own message at the
  // top of the stream, attributed to the launcher: the same block shape the
  // chat page gives an operator's sent message, with the "from <agent>"
  // attribution in the state line (the fold's instruction block carries the
  // text; the launch is always the first user message, so it is delivered
  // at sub-turn 1). Reference-stable across live-only deltas, like the
  // result panel above.
  const renderInstruction = useCallback(
    (block: Extract<Block, { type: "instruction" }>) => (
      <div className="msg msg-user" key={`${block.seq}-instruction`}>
        <div className="body">{block.text}</div>
        <div className="state">
          from {who} · delivered · sub-turn 1
        </div>
      </div>
    ),
    [who],
  );

  // The plan-mini band (design/session-watch.html's narrow-width fallback):
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

  // The nav's right slot (design/session-watch.html's header): the RUNNING
  // badge with its pulse dot, the elapsed time, the permission-mode badge —
  // a safety fact, stated next to the stop button — and the Stop control,
  // which arms the inline confirm strip in the footer. The connection badge
  // stays: it is the only thing on this screen that says the stream is (or
  // is not) still telling the page what happens next.
  const navElapsed = running ? formatRunDuration(now - Date.parse(meta.created_at)) : null;
  useNavRight(
    <>
      {running && (
        <>
          <Badge variant="running">
            <span className="dot dot-pulse" aria-hidden />
            RUNNING
          </Badge>
          {navElapsed && <span>{navElapsed}</span>}
          <Badge variant="outline" className="badge-perm" title="This run can reach the host docker socket when full">
            {meta.permission_mode}
          </Badge>
          {token !== null && (
            <Button
              variant="outline"
              size="sm"
              onClick={toggleStopConfirm}
              disabled={stopping}
              aria-expanded={confirmingStop}
            >
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

  return (
    <>
      <DroppedStreamBanner connection={snapshot.connection} everOpen={everOpen} />
      {/* The provenance strip (design/session-watch.html's .prov): who
          started this run, the originating request and job, and the one
          sentence that says what a spectator may do — a quiet strip, not an
          alert, because this is the normal state for these sessions. */}
      <div className="prov">
        <Badge variant="outline">WATCHING</Badge>
        {startedByLabel && (
          <>
            <span className="sep">·</span>
            <span className="who">{startedByLabel}</span>
          </>
        )}
        {meta.request_id && (
          <>
            <span className="sep">·</span>
            <span>
              request <code>{meta.request_id}</code>
            </span>
          </>
        )}
        {meta.job_type && (
          <>
            <span className="sep">·</span>
            <span>
              job <code>{meta.job_type}</code>
            </span>
          </>
        )}
        <span className="spacer" />
        <span>
          This run takes its instructions from {who}. You can stop it, but not message it.
        </span>
        <a
          href="/"
          onClick={(e) => {
            e.preventDefault();
            onNavigate("/");
          }}
        >
          Session row →
        </a>
      </div>
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
      <div className="work work-watch">
        <WatchRail
          items={snapshot.items}
          live={snapshot.live}
          todos={snapshot.todos}
          counts={snapshot.counts}
          filter={filter}
          onFilterChange={setFilter}
          query={query}
          onQueryChange={setQuery}
          meta={meta}
          getToolCall={snapshot.getToolCall}
        />
        <main className="stream" ref={streamRef} onScroll={onStreamScroll}>
          <div className="stream-inner">
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
              filter={filter}
              getToolCall={snapshot.getToolCall}
              renderInstruction={renderInstruction}
              renderRunFinished={renderRunFinished}
              textQuery={query}
            />
          </div>
          {!following && (
            <div className="jumpwrap">
              <button type="button" className="jump" onClick={toggleFollow}>
                ↓ Jump to live
              </button>
            </div>
          )}
        </main>
      </div>
      <WatchFooter
        running={running}
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
    </>
  );
}
