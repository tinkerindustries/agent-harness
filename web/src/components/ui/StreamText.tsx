import { cn } from "@/lib/utils";

// Model output as it lands: each committed chunk fades and settles in (320ms,
// out-quart), so a reader can see where the text grew, with a blinking block
// cursor at the tail while the turn is live.
//
// READ THIS BEFORE WIRING IT (docs/DESIGN.md §5.3, web/CLAUDE.md):
//
// Streaming text deliberately lives OUTSIDE React state — deltas append to a
// mutable buffer and a requestAnimationFrame loop flushes it, so React sees at
// most one update per frame. If this component renders the live buffer, the tail
// chunk re-animates on every flush: 60 animations a second on the one element a
// reader is looking at.
//
// So: chunks must be committed, immutable values — the same ones the memoised
// FrozenBlock / SubTurnCard render, keyed by block id and never re-rendered. The
// live pair (LiveTurn.liveContent) stays plain preformatted text with the
// existing .live .say treatment; it does not come through here. Append to the
// array; never re-key or reorder existing entries, or the whole block replays.
export function StreamText({
  chunks,
  live = false,
  className,
}: {
  /** Committed chunks in order. Append only. */
  chunks: readonly string[];
  /** Show the block cursor at the tail while the turn is still streaming. */
  live?: boolean;
  className?: string;
}) {
  return (
    <span data-slot="stream-text" className={cn("whitespace-pre-wrap", className)}>
      {chunks.map((chunk, i) => (
        // eslint-disable-next-line react/no-array-index-key -- append-only: an
        // index is stable here and a content key would replay on a repeated chunk.
        <span key={i} className="anim-stream-in inline">
          {chunk}
        </span>
      ))}
      {live && (
        <span
          aria-hidden="true"
          className="dot-pulse ml-0.5 inline-block h-[14px] w-[7px] align-[-2px]"
          style={{ background: "var(--status-running)" }}
        />
      )}
    </span>
  );
}
