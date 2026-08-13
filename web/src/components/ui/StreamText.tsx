import { cn } from "@/lib/utils";

// Model output as it lands: each chunk fades and settles in (--dur-reveal,
// out-quart), so a reader can see where the text grew.
//
// READ THIS BEFORE FEEDING IT ANYTHING NEW (docs/DESIGN.md §5.3, web/CLAUDE.md):
//
// The one and only safe input is an APPEND-ONLY array of immutable strings —
// today LiveTurn.liveContentChunks, one entry per `live` SSE frame. Append to
// it; never rewrite, re-key, or reorder an entry that is already in it.
//
// The reason is the whole design of the streaming path. Text deliberately
// lives outside React state: deltas land in a mutable buffer and a
// requestAnimationFrame loop flushes it, so React sees at most one update per
// frame. This component is rendered from inside that loop, so it re-renders
// ~60 times a second. What keeps it from animating 60 times a second is that
// an unchanged entry keeps its position and its DOM node, and a CSS animation
// only runs when its element mounts. Mutate the tail entry in place instead of
// pushing a new one and every flush replays the animation on the element a
// reader is actually looking at.
//
// Note what this is NOT fed: the committed content_delta events. Those are
// written in one batch with the turn_finished that freezes this component
// away (internal/session/turn.go), so they would arrive as one lump, animate
// a whole turn's prose in a single frame, and then be replaced by the frozen
// block's markdown. The frames are the only thing that streams.
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
