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

  React.useEffect(() => {
    if (value === shown) return;
    setPrev(shown);
    setShown(value);
    const t = window.setTimeout(() => setPrev(null), 260);
    return () => window.clearTimeout(t);
  }, [value, shown]);

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
