import type { Block } from "../../api/fold";
import { formatDuration } from "../../api/operations";
import { useNow } from "../../hooks";
import { pendingWaitLabel } from "./turnHelpers";
import { cn } from "@/lib/utils";

// The sent-message shell (.msg .msg-user): a mono body behind the
// running-colour left rule, and a state line under it. Shared by every
// variant below and by SessionWatchScreen.tsx's launcher-instruction
// rendering, which draws the same shape for the run's opening instruction.
export const MSG_CLS = "mb-[22px] ml-10 max-nav:ml-0";
export const MSG_USER_CLS = "border-l-2 border-[var(--status-running)] py-px pl-[13px]";
export const MSG_BODY_CLS = "font-mono text-sm leading-[1.55] whitespace-pre-wrap";
export const MSG_STATE_CLS = "flex items-center gap-1.5 mt-[5px] text-micro text-muted-foreground";

// SteerMessage is a sent operator message on the chat page (.msg-user,
// three states): the text in mono behind the running-colour left rule,
// and a state line under it. It replaces the old steer block card on
// the interactive page
// — a message is a message, not a block — while the watch page keeps the
// block rendering.
//
// Two of the three states come from the fold's steer block:
//
// - pending — steer_message is in the log, the matching steer_applied has
//   not arrived. The line counts up from when this browser's POST was
//   accepted and says what the message is waiting on, because a steer that
//   sits pending is the operator's signal that the run is wedged
//   (docs/RUN-CONTROL.md "Two event kinds, not one") — the count-up, not an
//   indeterminate spinner, is the point. The sent time is the 202's local
//   timestamp, passed in by the chat screen's ledger; a steer sent by
//   another client (or before a reload) has no local time and the count is
//   omitted rather than fabricated.
// - delivered — the matching steer_applied arrived; the line names the
//   sub-turn it landed in, straight from the block's own appliedSubTurn —
//   the producer-stamped boundary the message became a user message at
//   (docs/RUN-CONTROL.md "Two event kinds, not one"), never derived from
//   transcript order. A delivered block folded before that field existed
//   renders without naming a sub-turn: the fallback is a plain "delivered",
//   so an old log still renders.
//
// The third state — not sent, a POST that failed so nothing is in the log —
// is composer-local and never becomes a block; the chat screen renders those
// from its own failed-send ledger (see SessionChatScreen), not through this
// component.
export type SteerBlock = Extract<Block, { type: "steer" }>;

export interface SteerWait {
  // Whether a tool call is still running (live.pendingTools non-empty) —
  // the boundary a pending steer waits on is the end of the tool round.
  hasToolRound: boolean;
  // The sub-turn currently streaming, or null between turns.
  liveSubTurn: number | null;
}

interface Props {
  block: SteerBlock;
  // When this browser's own POST accepted the steer: the local time of the
  // 202, so the pending count-up has a start. Absent for a steer this page
  // did not send.
  sentAt?: number;
  // What the pending message is waiting on, from the live view.
  wait: SteerWait;
  // The run is over. A still-pending steer will never be applied — the loop
  // delivers at the next sub-turn boundary, and there is no next boundary —
  // so the pending vocabulary ("waiting for the current tool call to
  // finish") would claim a boundary is coming that is not, and a forever
  // pulsing dot would read as a wedged run: a steer sent while the final
  // sub-turn's request was in flight sits pending on a finished run
  // otherwise. The state line then says the one true thing — the message
  // was accepted, and the run ended before it reached the model.
  runEnded: boolean;
}

// SteerMessage re-renders once a second while a pending steer's count is
// showing. It is deliberately NOT memoised — the ticking is the whole point
// — but its parent (the memoised TurnList) bails out on every live-only
// delta, so the tick never re-renders the conversation, only this message.
export function SteerMessage({ block, sentAt, wait, runEnded }: Props) {
  const now = useNow(1000);

  if (block.state === "delivered") {
    return (
      <div className={cn(MSG_CLS, MSG_USER_CLS)}>
        <div className={MSG_BODY_CLS}>{block.text}</div>
        <div className={MSG_STATE_CLS}>
          delivered{block.appliedSubTurn != null ? ` · sub-turn ${block.appliedSubTurn}` : ""}
        </div>
      </div>
    );
  }

  const age = sentAt !== undefined ? `sent ${formatDuration(Math.max(0, now - sentAt))} ago` : null;
  if (runEnded) {
    return (
      <div className={cn(MSG_CLS, MSG_USER_CLS)}>
        <div className={MSG_BODY_CLS}>{block.text}</div>
        <div className={cn(MSG_STATE_CLS, "text-[var(--status-gaveup)]")}>
          {age ? `${age} · ` : ""}not delivered — the run ended before this message reached the model
        </div>
      </div>
    );
  }
  return (
    <div className={cn(MSG_CLS, MSG_USER_CLS)}>
      <div className={MSG_BODY_CLS}>{block.text}</div>
      <div className={cn(MSG_STATE_CLS, "text-[var(--status-gaveup)]")}>
        <span className="h-1.5 w-1.5 flex-none rounded-full bg-current dot-pulse" aria-hidden />
        pending{age ? ` · ${age}` : ""} · {pendingWaitLabel(wait.hasToolRound, wait.liveSubTurn)}
      </div>
    </div>
  );
}
