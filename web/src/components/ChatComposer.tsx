import { useEffect, useRef, useState } from "react";
import { HourglassMedium } from "@phosphor-icons/react";
import { formatCost } from "./blocks/toolArgs";
import { useLabelFlip } from "../hooks";
import type { Outcome } from "./statusBadge";
import { cachePercent, formatRunDuration } from "./turns/turnHelpers";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";

// ChatComposer is the chat page's footer band (.footer): the queued line,
// the .box with the > prompt, the auto-growing textarea and the Send
// button, and the status line of live numbers under it. The band never
// moves — the page does not scroll, the conversation column does — so
// the composer is always where the eye last was.
//
// Its semantics are SteerControl's, kept exactly (docs/RUN-CONTROL.md "The
// frontend"): the POST is an acceptance, not a delivery — the 202 only means
// the text landed in the log, it reaches the model at the next sub-turn
// boundary, and the run does not pause for it. Nothing here polls or guesses
// at when the model saw it; the SSE stream the screen is already connected
// to delivers the steer block that flips the message pending → delivered.
// The composer therefore never implies a reply is owed: the queued line
// above the box says the run does not pause, and the pending message counts
// up rather than spinning.
//
// A null token hides the box exactly as SteerControl hid itself: run control
// not configured means an empty bearer would 503, so unavailable must look
// unavailable, not broken (docs/RUN-CONTROL.md "Authentication": a missing
// credential fails closed). The status line is not a control and stays.
//
// The band also carries the states around the run: the inline stop
// confirmation and the *stopping…* banner between the 202 and the
// terminal event, and — once the run is over — the .box-done
// band in the composer's place. The composer is removed rather than disabled:
// a greyed-out box invites the reader to hunt for the way to enable it, and
// the endpoint refuses a steer on a finished run anyway.
export interface ComposerStatus {
  // The sub-turn the status line names: the live turn, or the last frozen
  // one. Null before the first sub-turn.
  subTurn: number | null;
  cacheHitTokens: number;
  cacheMissTokens: number;
  costUsd: number;
  completionTokens: number;
}

export interface FinishedBand {
  label: string;
  variant: Outcome["variant"];
  // The sentence under the badge ("Finished in 16m 31s over 78 sub-turns
  // for $0.0838."), from turnHelpers.finishedBandText.
  text: string;
}

interface ChatComposerProps {
  // The run-control bearer; null hides the composer box (see above).
  token: string | null;
  // The run is steerable — the composer box and the stop controls render.
  // Once the run is over the .box-done band replaces them, and a run that is
  // live but not yet steerable (its workspace still being prepared) shows
  // neither box nor band, so the composer stays disabled while the workspace
  // is being built.
  running: boolean;
  // Steers accepted but not yet applied, for the queued line.
  pendingCount: number;
  // The steer write itself, owned by the screen: POSTs the text, records
  // the acceptance in the screen's ledger, and returns whether the 202
  // landed (false leaves the text in the box for the operator to see).
  send: (text: string) => Promise<boolean>;
  // The stop flow: the nav's Stop button (and esc esc) arm the inline
  // confirm strip; the strip's Stop run posts the stop; the banner shows
  // between the 202 and the terminal event.
  stop: {
    confirming: boolean;
    stopping: boolean;
    error: string | null;
    onRequestStop: () => void;
    onCancelStop: () => void;
    onConfirmStop: () => void;
  };
  // What the run is doing right now, for the confirm strip's sentence. Null
  // when there is no elapsed to state (the row never arrived).
  activity: { elapsedMs: number; detail: string } | null;
  // The status line's live numbers. Null before the session row arrives.
  status: ComposerStatus | null;
  // The facts the empty status line shows instead of live numbers
  // ("deepseek-v4-pro · effort high · permission full").
  facts: { model: string; effort: string; permission: string };
  // The run is over: the finished band replaces the composer. Null while
  // the run is live (or before the row arrives).
  finished: FinishedBand | null;
  // Starts a follow-up run from the finished band.
  onFollowUp: () => void;
}

export function ChatComposer({
  token,
  running,
  pendingCount,
  send,
  stop,
  activity,
  status,
  facts,
  finished,
  onFollowUp,
}: ChatComposerProps) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const taRef = useRef<HTMLTextAreaElement>(null);

  // A session that stops being running clears the input, exactly as
  // SteerControl did: a steer for a finished run would sit in the log
  // forever, unapplied and unexplained, and the endpoint would refuse it
  // anyway.
  useEffect(() => {
    if (!running) {
      setText("");
      setSending(false);
    }
  }, [running]);

  // The textarea grows with its content (the design's auto-grow) and
  // shrinks again when a send clears it.
  useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = `${Math.min(ta.scrollHeight, 180)}px`;
  }, [text]);

  const sendNow = async () => {
    const t = text.trim();
    if (sending || t === "") return;
    setSending(true);
    const accepted = await send(t);
    setSending(false);
    if (accepted) setText("");
  };

  // esc esc is the stop shortcut the status line advertises: two Escapes
  // within half a second arm the confirm strip, exactly as the nav's Stop
  // button does. The hint is only drawn while the composer is live.
  const stopRef = useRef(stop);
  stopRef.current = stop;
  useEffect(() => {
    if (!running) return;
    let lastEsc = 0;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      const now = Date.now();
      if (now - lastEsc < 600) {
        lastEsc = 0;
        stopRef.current.onRequestStop();
      } else {
        lastEsc = now;
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [running]);

  // The outcome badge's flip gate: "RUNNING" until the band arrives, then the
  // outcome's own label, so .anim-badge-in fires on the change and never on
  // first paint (hooks.ts useLabelFlip).
  const outcomeFlip = useLabelFlip(finished?.label ?? "RUNNING");

  // Before the session row arrives AND run control is unconfigured the band
  // would be empty — no box, no numbers, no facts — so it stays off the page
  // rather than drawing a bare strip above nothing.
  if (finished === null && token === null && status === null && facts.model === "") return null;

  return (
    <div className="footer">
      <div className="footer-inner">
        {finished ? (
          <div className="box-done">
            {/* The one badge on this page that genuinely flips in place: the
                composer stays mounted and the band replaces it when the run
                ends, so the outcome arrives rather than having always been
                there. The gate keeps a run that was already finished when the
                page opened from flipping on load. */}
            <Badge key={finished.label} variant={finished.variant} className={outcomeFlip}>
              {finished.label}
            </Badge>
            <span>{finished.text}</span>
            <span className="spacer" />
            {token !== null && (
              <Button variant="outline" size="sm" onClick={onFollowUp}>
                Start a follow-up run here
              </Button>
            )}
          </div>
        ) : (
          <>
            {stop.confirming && (
              <div className="confirm">
                <b>Stop this run?</b>
                <span className="muted">
                  {activity
                    ? `It is ${formatRunDuration(activity.elapsedMs)} in, ${activity.detail}. The work it has done stays in the workspace.`
                    : "The work it has done stays in the workspace."}
                </span>
                <span className="spacer" />
                <Button variant="outline" size="sm" onClick={stop.onCancelStop} disabled={stop.stopping}>
                  Keep running
                </Button>
                <Button variant="destructive" size="sm" onClick={stop.onConfirmStop} disabled={stop.stopping}>
                  {stop.stopping ? "Stopping…" : "Stop run"}
                </Button>
              </div>
            )}
            {stop.stopping && (
              <div className="banner">
                {/* The hourglass takes the pulse the dot carried: waiting is
                    what the banner is about, and the mark now says so. */}
                <HourglassMedium className="dot-pulse" style={{ color: "var(--status-gaveup)" }} aria-hidden />
                <span>
                  <b>Stopping…</b> waiting for the current tool call to return. The run ends at the next boundary.
                </span>
              </div>
            )}
            {stop.error && <span className="field-error">{stop.error}</span>}
            {pendingCount > 0 && (
              <div className="queued">
                <span className="dot dot-pulse" aria-hidden />
                {pendingCount} message{pendingCount === 1 ? "" : "s"} waiting — it reaches the model at the next
                sub-turn boundary. The run does not pause.
              </div>
            )}
            {token !== null && running && (
              <div className="box">
                <span className="prompt" aria-hidden>
                  &gt;
                </span>
                <textarea
                  ref={taRef}
                  value={text}
                  rows={1}
                  placeholder={status && status.subTurn === null ? "Describe the task…" : "Send a message to the run…"}
                  onChange={(e) => setText(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && !e.shiftKey) {
                      e.preventDefault();
                      void sendNow();
                    }
                  }}
                  disabled={sending}
                  aria-label="Message the running session"
                  spellCheck={false}
                />
                <button type="button" className="send" onClick={() => void sendNow()} disabled={sending || text.trim() === ""}>
                  Send
                </button>
              </div>
            )}
            <StatusLine status={status} facts={facts} running={running} />
          </>
        )}
      </div>
    </div>
  );
}

// StatusLine is the footer's bottom row: the live numbers — sub-turn,
// cache hit, cost, output tokens — then the keyboard hints on the
// right. Before the first message there are no numbers yet, so
// the line states the session facts instead, the way the design's empty
// state draws it. The hints are drawn only while the composer is live: they
// describe the composer and the stop shortcut, and a finished run's band
// needs neither.
function StatusLine({
  status,
  facts,
  running,
}: {
  status: ComposerStatus | null;
  facts: { model: string; effort: string; permission: string };
  running: boolean;
}) {
  // Before the session row arrives there is nothing true to state — neither
  // live numbers nor the facts — so the line stays empty rather than
  // printing dashes.
  if (!status && facts.model === "") return null;
  return (
    <div className="statusline">
      {status && status.subTurn !== null ? (
        <>
          <span>sub-turn {status.subTurn}</span>
          <span className="sep">·</span>
          <span>
            {cachePercent(status.cacheHitTokens, status.cacheMissTokens)}% cache
          </span>
          <span className="sep">·</span>
          <span title="Price table captured by the server's pricing config">${formatCost(status.costUsd)}</span>
          <span className="sep">·</span>
          <span>{status.completionTokens.toLocaleString("en-US")} out</span>
        </>
      ) : (
        <>
          <span>{facts.model}</span>
          <span className="sep">·</span>
          <span>effort {facts.effort}</span>
          <span className="sep">·</span>
          <span>permission {facts.permission}</span>
        </>
      )}
      <span className="spacer" />
      {running && (
        <>
          <span>
            <kbd>⏎</kbd> send
          </span>
          <span>
            <kbd>⇧⏎</kbd> newline
          </span>
          <span>
            <kbd>esc</kbd>
            <kbd>esc</kbd> stop run
          </span>
        </>
      )}
    </div>
  );
}
