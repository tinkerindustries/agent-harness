// The alignment behind Ticker's roll: which characters of a figure actually
// moved, so only those animate. Split out of the component because it is the
// part with a decision in it, and the components have no DOM harness
// (../../../TESTING.md).

/** One character of the figure, paired with the character it replaced. */
export type TickerSlot = {
  /**
   * Distance from the end of the string, not the index from the start. A
   * figure that grows a digit on the left — "9" to "10", "9:59" to "10:00" —
   * would otherwise renumber every position to its right, and React would
   * remount and re-animate characters that never moved. Counting from the end
   * keeps the key of an untouched position stable.
   */
  key: number;
  char: string;
  /** The outgoing character, or null when this position is unchanged or new. */
  prev: string | null;
};

/**
 * Pair each character of `shown` with the one it replaced, aligning the two
 * strings on their right-hand end — the alignment a number wants, where a
 * carried digit pushes the older ones left rather than shifting their meaning.
 *
 * A position whose character is unchanged reports `prev: null` and does not
 * animate. So does a position with no counterpart at all, which is a figure
 * that grew: the new leading character rolls in from nothing. The mirror case,
 * a figure that shrank, drops its extra leading characters without a roll —
 * they have no position left to leave from.
 */
export function tickerSlots(shown: string, prev: string | null): TickerSlot[] {
  const chars = Array.from(shown);
  const before = prev === null ? null : Array.from(prev);
  // Positive when the figure grew: how far each position sits from its
  // counterpart in the outgoing string, counted from the left.
  const offset = before === null ? 0 : chars.length - before.length;

  return chars.map((char, i) => {
    const key = chars.length - 1 - i;
    const j = i - offset;
    const was = before === null || j < 0 || j >= before.length ? null : before[j];
    return {
      key,
      char: printable(char),
      prev: was === null || was === char ? null : printable(was),
    };
  });
}

// Each character is rendered in its own inline-block box, and a box holding
// nothing but an ordinary space collapses to no width at all — a figure
// formatted with one in it would lose it. No formatter feeding a Ticker uses a
// space today; this is what stops the first one that does from silently
// closing up.
function printable(char: string): string {
  return char === " " ? "\u00a0" : char;
}
