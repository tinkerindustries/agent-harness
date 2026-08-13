import * as React from "react";

import { cn } from "@/lib/utils";
import { tickerSlots } from "./tickerSlots";

// A figure that changes while you watch it: the stat strip's four numbers, the
// in-flight card's elapsed, the session footer's cost and cache rate. The
// characters that moved roll up and out as their replacements roll up into
// place, in one --dur-roll gesture, so a number that moved is noticeable
// without the row twitching.
//
// Per character, not per figure. Elapsed ticks once a second and cost changes
// a digit at a time; rolling the whole string on every change made a
// one-digit move look like a reshuffle, and left the eye nowhere to rest. The
// characters are aligned on their right-hand end and keyed by distance from
// that end (tickerSlots.ts), so an unchanged position keeps its DOM node,
// never remounts, and never re-runs the animation — which is the whole
// mechanism: a CSS animation only fires when its element mounts.
//
// Not for the finished table: those figures are final and a roll there is noise.
//
// The previous value is held for one HOLD_MS timeout — slightly longer than
// the animation — so the outgoing and incoming characters never overlap after
// the gesture ends. The outgoing character is absolutely positioned inside an
// overflow-hidden box, so the roll cannot change the row's height.
const HOLD_MS = 400;

export function Ticker({
  value,
  prefix,
  suffix,
  className,
}: {
  /** Already formatted: "$0.0507", "99.1%", "6:45". */
  value: string | number;
  prefix?: React.ReactNode;
  suffix?: React.ReactNode;
  className?: string;
}) {
  const text = String(value);
  const [shown, setShown] = React.useState(text);
  const [prev, setPrev] = React.useState<string | null>(null);

  // The timeout is held in a ref rather than cleared by this effect's own
  // cleanup. Setting `shown` re-runs the effect (shown is a dependency), and a
  // cleanup here would cancel the very timeout the previous run had just
  // scheduled — the outgoing value would then never be dropped. That is safe
  // because .anim-roll-out ends at opacity 0 with `both`, and because the
  // prefers-reduced-motion rule that turns the animation off pins the same
  // opacity by hand — without it a stale outgoing character would paint at
  // full strength over the one that replaced it.
  const timer = React.useRef(0);

  React.useEffect(() => {
    if (text === shown) return;
    setPrev(shown);
    setShown(text);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setPrev(null), HOLD_MS);
  }, [text, shown]);

  React.useEffect(() => () => window.clearTimeout(timer.current), []);

  const slots = tickerSlots(shown, prev);

  return (
    <span data-slot="ticker" className={cn("inline-flex items-baseline tabular-nums", className)}>
      {prefix}
      {/* One character per box reads as a spelling-out to a screen reader, so
          the figure is announced once as itself and the boxes are hidden. */}
      <span className="sr-only">{shown}</span>
      <span aria-hidden="true" className="inline-flex items-baseline">
        {slots.map((slot) => (
          <span key={slot.key} className="relative inline-block overflow-hidden leading-[1.2]">
            {/* Keyed by the character itself: a changed position remounts and
                so re-runs the roll, an unchanged one does neither. */}
            <span key={slot.char} className="anim-roll-in inline-block">
              {slot.char}
            </span>
            {slot.prev !== null && (
              <span className="anim-roll-out absolute left-0 top-0 inline-block">{slot.prev}</span>
            )}
          </span>
        ))}
      </span>
      {suffix}
    </span>
  );
}
