import { useEffect, useRef, useState } from "react";
import { HourglassMedium, X } from "@phosphor-icons/react";
import { formatCost } from "./blocks/toolArgs";
import { formatFileSize, pasteStamp, readPastedImages, type AttachmentCaps, type ChosenAttachment } from "../api/attachments";
import type { RunAttachment } from "../api/operations";
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
// confirmation and the *stopping…* banner between the 202 and the terminal
// event.
//
// The box outlives the run. A message typed into it is a steer while the loop
// is alive and a resume once it is over (docs/RUN-CONTROL.md "Continuing"),
// which is what makes this page a conversation rather than a single run with
// a text field on it — the screen owns that routing and hands it down as one
// `send`. So the two live states differ only in what surrounds the box: a
// running session gets the stop controls, the queued line and the esc esc
// hint, and a finished one gets none of them, because there is nothing to
// stop and nothing waiting on a sub-turn boundary.
//
// Images arrive by paste, and only by paste (docs/RUN-CONTROL.md, "Images in
// the composer"). There is no file input here the way there is on the start
// form: the thing people actually do mid-conversation is screenshot something
// and hit paste, and a second control for the rarer case would cost the band
// height it does not have. A paste carrying images stages them above the box
// as thumbnails and rides the next send, whichever verb that send turns out
// to be — the images are part of the message, not a separate act, so there is
// nothing to upload and nothing to wait for before typing.
//
// The done band is what is left for a session that cannot be continued at all
// — one retired by compaction, whose continuation is its child. There the
// composer is removed rather than disabled, for the reason it always was: a
// greyed-out box invites the reader to hunt for the way to enable it.
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
  // The run is steerable — the stop controls, the queued line and the esc esc
  // hint render alongside the box. A run that is live but not yet steerable
  // (its workspace still being prepared) shows neither box nor band, so the
  // composer stays disabled while the workspace is being built.
  running: boolean;
  // The run is over and this session can be continued from where it stopped
  // (api/status.ts canResume). The box stays, and what it sends is a resume.
  resumable: boolean;
  // Steers accepted but not yet applied, for the queued line.
  pendingCount: number;
  // The write itself, owned by the screen: POSTs the text and its images as a
  // steer or a resume depending on the run's state, records the acceptance in
  // the screen's ledger, and returns whether the 202 landed (false leaves the
  // text and the images in the box for the operator to see).
  send: (text: string, attachments: RunAttachment[]) => Promise<boolean>;
  // The caps a pasted image is checked against, from GET /api/settings — the
  // same two the start form reads and the endpoint enforces. Null means they
  // have not arrived (or their fetch failed), and a paste then says so
  // rather than staging bytes the server may refuse: the box still takes
  // text, because text never needed them.
  attachmentCaps: AttachmentCaps | null;
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
  // The run is over and cannot be continued: the finished band replaces the
  // composer. Null while the run is live, while it is resumable, and before
  // the row arrives.
  finished: FinishedBand | null;
  // Starts a follow-up run from the finished band.
  onFollowUp: () => void;
}

export function ChatComposer({
  token,
  running,
  resumable,
  pendingCount,
  send,
  attachmentCaps,
  stop,
  activity,
  status,
  facts,
  finished,
  onFollowUp,
}: ChatComposerProps) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  // The images staged for the next send, and the one refusal a paste can
  // produce (over the count cap, over the per-image cap, or the caps never
  // arrived). The error clears on the next paste rather than on a timer —
  // it is about the paste that just happened, and it stops being about
  // anything the moment another one does.
  const [staged, setStaged] = useState<ChosenAttachment[]>([]);
  const [pasteError, setPasteError] = useState<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);

  // Text survives the run ending, because the box does: a half-typed message
  // becomes the resume that continues the session, and clearing it would
  // throw away what somebody was in the middle of writing at exactly the
  // moment the model stopped. It is cleared only when the box itself goes
  // away — a session that can be neither steered nor continued.
  const usable = running || resumable;
  useEffect(() => {
    if (!usable) {
      setText("");
      setStaged([]);
      setPasteError(null);
      setSending(false);
    }
  }, [usable]);

  // The textarea grows with its content (the design's auto-grow) and
  // shrinks again when a send clears it.
  useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = `${Math.min(ta.scrollHeight, 180)}px`;
  }, [text]);

  // A message is sendable with words, with images, or with both: a pasted
  // screenshot and nothing else is a complete thing to say, and the endpoints
  // accept it on the same terms.
  const empty = text.trim() === "" && staged.length === 0;
  const sendNow = async () => {
    if (sending || empty) return;
    setSending(true);
    const accepted = await send(
      text.trim(),
      staged.map((c) => c.attachment),
    );
    setSending(false);
    if (accepted) {
      setText("");
      setStaged([]);
      setPasteError(null);
    }
  };

  // A paste is intercepted only when it actually carries images; a paste of
  // text is left entirely alone, so the box behaves exactly as it did for
  // everyone who never pastes a picture into it. preventDefault is called
  // only on the image branch, and only after that check, because a clipboard
  // holding both a screenshot and its alt text should still drop the text in.
  const onPaste = async (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(e.clipboardData?.files ?? []);
    if (!files.some((f) => f.type.startsWith("image/"))) return;
    e.preventDefault();
    if (!attachmentCaps) {
      setPasteError("Attachment limits are unavailable, so images cannot be attached to this message.");
      return;
    }
    const result = await readPastedImages(files, attachmentCaps, staged, pasteStamp(new Date()));
    if (!result.ok) {
      setPasteError(result.error);
      return;
    }
    setPasteError(null);
    setStaged((prev) => [...prev, ...result.chosen]);
  };

  const removeStaged = (name: string) => {
    setStaged((prev) => prev.filter((c) => c.attachment.name !== name));
    setPasteError(null);
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
    // "footer" stays a literal class only for the grid-collapse and padding
    // overrides in styles.css (max-width: 1120px/720px) — every other
    // footer/footer-inner property is a direct Tailwind utility.
    <div className="footer grid grid-cols-[minmax(0,1fr)_288px] border-t border-border bg-background px-6 pt-2.5 pb-[9px]">
      <div className="col-start-1 mx-auto w-full max-w-[800px]">
        {finished ? (
          <div className="flex flex-wrap items-center gap-3 rounded-lg border border-dashed border-border bg-muted px-3.5 py-3 text-sm text-muted-foreground">
            {/* The one badge on this page that genuinely flips in place: the
                composer stays mounted and the band replaces it when the run
                ends, so the outcome arrives rather than having always been
                there. The gate keeps a run that was already finished when the
                page opened from flipping on load. */}
            <Badge key={finished.label} variant={finished.variant} className={outcomeFlip}>
              {finished.label}
            </Badge>
            <span>{finished.text}</span>
            <span className="flex-1" />
            {token !== null && (
              <Button variant="outline" size="sm" onClick={onFollowUp}>
                Start a follow-up run here
              </Button>
            )}
          </div>
        ) : (
          <>
            {stop.confirming && (
              <div className="flex flex-wrap items-center gap-2.5 rounded-[calc(var(--radius)-2px)] border border-[var(--status-failed)] bg-[var(--status-failed-bg)] px-3 py-2.5 text-sm">
                <b>Stop this run?</b>
                <span className="text-muted-foreground">
                  {activity
                    ? `It is ${formatRunDuration(activity.elapsedMs)} in, ${activity.detail}. The work it has done stays in the workspace.`
                    : "The work it has done stays in the workspace."}
                </span>
                <span className="flex-1" />
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
            {stop.error && <span className="text-xs font-mono text-[var(--status-failed)] basis-full">{stop.error}</span>}
            {pendingCount > 0 && (
              <div className="mb-1.5 flex items-center gap-1.5 text-micro text-[var(--status-gaveup)]">
                <span className="h-1.5 w-1.5 flex-none rounded-full bg-current dot-pulse" aria-hidden />
                {pendingCount} message{pendingCount === 1 ? "" : "s"} waiting — it reaches the model at the next
                sub-turn boundary. The run does not pause.
              </div>
            )}
            {token !== null && usable && (staged.length > 0 || pasteError !== null) && (
              <StagedImages staged={staged} error={pasteError} onRemove={removeStaged} disabled={sending} />
            )}
            {token !== null && usable && (
              <div className="flex items-end gap-2 rounded-lg border border-input bg-card py-2 pr-2 pl-2.5 focus-within:border-ring focus-within:[box-shadow:0_0_0_3px_hsl(217_91%_48%/0.09)] max-phone:pl-3">
                <span className="flex-none self-start font-mono font-semibold leading-[1.55] text-[var(--status-running)]" aria-hidden>
                  &gt;
                </span>
                <textarea
                  ref={taRef}
                  className="min-h-[38px] flex-1 resize-none border-0 bg-transparent p-0 font-mono text-sm leading-[1.55] text-foreground outline-none placeholder:text-muted-foreground max-phone:min-h-11"
                  value={text}
                  rows={1}
                  placeholder={
                    status && status.subTurn === null
                      ? "Describe the task…"
                      : running
                        ? "Send a message to the run…"
                        : "Continue this session…"
                  }
                  onChange={(e) => setText(e.target.value)}
                  onPaste={(e) => void onPaste(e)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && !e.shiftKey) {
                      e.preventDefault();
                      void sendNow();
                    }
                  }}
                  disabled={sending}
                  aria-label={running ? "Message the running session" : "Continue this session"}
                  spellCheck={false}
                />
                <button
                  type="button"
                  className="h-[26px] flex-none cursor-pointer rounded-[calc(var(--radius)-3px)] border border-border bg-secondary px-2.5 font-[inherit] text-xs text-secondary-foreground hover:bg-accent max-phone:h-11 max-phone:px-4"
                  onClick={() => void sendNow()}
                  disabled={sending || empty}
                >
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

// StagedImages is the strip of pasted images waiting on the next send, and
// the one refusal a paste can produce. The thumbnails are drawn from the
// base64 the payload already carries rather than an object URL, so there is
// no revoke to get wrong and the tile survives a re-render for free — the
// bytes are in memory either way, and each is capped at a few megabytes by
// the same limit the endpoint enforces.
//
// Each tile is its own remove button rather than carrying one: the tile is
// small, the only thing anybody wants to do to a staged image is take it back
// out, and a hit target that is the whole thumbnail is the one that works on
// a phone.
function StagedImages({
  staged,
  error,
  onRemove,
  disabled,
}: {
  staged: ChosenAttachment[];
  error: string | null;
  onRemove: (name: string) => void;
  disabled: boolean;
}) {
  return (
    <div className="mb-1.5">
      {staged.length > 0 && (
        <ul className="flex flex-wrap gap-2 m-0 p-0 list-none">
          {staged.map(({ attachment, size }) => (
            <li key={attachment.name}>
              <button
                type="button"
                className="group relative block cursor-pointer overflow-hidden rounded-[calc(var(--radius)-3px)] border border-border bg-card p-0 disabled:cursor-default"
                onClick={() => onRemove(attachment.name)}
                disabled={disabled}
                title={`${attachment.name} · ${formatFileSize(size)} — click to remove`}
                aria-label={`Remove ${attachment.name} from this message`}
              >
                <img
                  src={`data:${attachment.mime_type};base64,${attachment.data}`}
                  alt=""
                  className="block h-14 w-14 object-cover"
                />
                <span className="absolute inset-0 flex items-center justify-center bg-background/70 opacity-0 transition-opacity group-hover:opacity-100 group-focus-visible:opacity-100">
                  <X aria-hidden />
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {error && <p className="mt-1 mb-0 text-xs text-[var(--status-failed)]">{error}</p>}
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
    <div className="mt-[7px] flex flex-wrap items-center gap-2 max-phone:gap-y-1 text-[0.6875rem] text-muted-foreground tabular-nums">
      {status && status.subTurn !== null ? (
        <>
          <span>sub-turn {status.subTurn}</span>
          <span className="text-[color-mix(in_srgb,var(--muted-foreground)_50%,transparent)]">·</span>
          <span>
            {cachePercent(status.cacheHitTokens, status.cacheMissTokens)}% cache
          </span>
          <span className="text-[color-mix(in_srgb,var(--muted-foreground)_50%,transparent)]">·</span>
          <span title="Price table captured by the server's pricing config">${formatCost(status.costUsd)}</span>
          <span className="text-[color-mix(in_srgb,var(--muted-foreground)_50%,transparent)]">·</span>
          <span>{status.completionTokens.toLocaleString("en-US")} out</span>
        </>
      ) : (
        <>
          <span>{facts.model}</span>
          <span className="text-[color-mix(in_srgb,var(--muted-foreground)_50%,transparent)]">·</span>
          <span>effort {facts.effort}</span>
          <span className="text-[color-mix(in_srgb,var(--muted-foreground)_50%,transparent)]">·</span>
          <span>permission {facts.permission}</span>
        </>
      )}
      <span className="flex-1" />
      {running && (
        <span className="contents max-phone:hidden">
          <span>
            <kbd>⏎</kbd> send
          </span>
          <span>
            <kbd>⇧⏎</kbd> newline
          </span>
          <span>
            <kbd>⌘V</kbd> attach image
          </span>
          <span>
            <kbd>esc</kbd>
            <kbd>esc</kbd> stop run
          </span>
        </span>
      )}
    </div>
  );
}
