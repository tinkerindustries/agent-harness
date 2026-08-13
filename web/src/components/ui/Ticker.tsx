import * as React from "react";

import { cn } from "@/lib/utils";

// A figure that changes while you watch it: the stat strip's four numbers, the
// in-flight card's elapsed, the session footer's cost and cache rate. The old
// value rolls up and out as the new one rolls up into place, in one 220ms
// gesture, so a number that moved is noticeable without the row twitching.
//
// Not for the finished table: those figures are final and a roll there is noise.
//
// The previous value is held for one 260ms timeout — slightly longer than the
// animation — so the outgoing and incoming spans never overlap after the
// gesture ends. Both are absolutely positioned inside an overflow-hidden box, so
// the roll cannot change the row's height.
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
  const [shown, setShown] = React.useState(value);
  const [prev, setPrev] = React.useState<string | number | null>(null);

  // The timeout is held in a ref rather than cleared by this effect's own
  // cleanup. Setting `shown` re-runs the effect (shown is a dependency), and a
  // cleanup here would cancel the very timeout the previous run had just
  // scheduled — the outgoing value would then never be dropped. It survived
  // review because .anim-roll-out ends at opacity 0 with `both`, so a stale
  // span is invisible; under prefers-reduced-motion the animation is off, the
  // span paints at full opacity, and every live figure renders with its
  // outgoing value overprinted on top of it.
  const timer = React.useRef(0);

  React.useEffect(() => {
    if (value === shown) return;
    setPrev(shown);
    setShown(value);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setPrev(null), 260);
  }, [value, shown]);

  React.useEffect(() => () => window.clearTimeout(timer.current), []);

  return (
    <span data-slot="ticker" className={cn("inline-flex items-baseline tabular-nums", className)}>
      {prefix}
      <span className="relative inline-block overflow-hidden leading-[1.2]">
        <span key={String(shown)} className="anim-roll-in inline-block">
          {shown}
        </span>
        {prev !== null && (
          <span aria-hidden="true" className="anim-roll-out absolute left-0 top-0 inline-block">
            {prev}
          </span>
        )}
      </span>
      {suffix}
    </span>
  );
}
