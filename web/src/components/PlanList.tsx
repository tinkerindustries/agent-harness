import { useEffect, useRef, useState } from "react";
import type { Todo } from "../api/types";
import { cn } from "@/lib/utils";

// The one todo-list renderer in the app, shared by the transcript's plan
// panel (PlanPanel) and the in-flight card's expanded plan. One component,
// one set of glyphs and
// status classes, so the two screens cannot drift apart. An in_progress
// item shows its activeForm ("Wiring six call sites…") rather than its
// static subject, exactly as the transcript renders it today. The item's
// description rides as a hover tooltip, so the panel stays compact while
// the longer explanation is still one hover away.
//
// The marks stay text: ✓ ▸ ○ are vocabulary — they encode plan state, and no
// icon set has a word for them (accordion.tsx carries the standing decision).
export const STATUS_MARK: Record<Todo["status"], string> = {
  completed: "✓",
  in_progress: "…",
  pending: "○",
};

// A margin past the end of --dur-swap, the mark-done gesture. Clearing the
// class before the animation finishes cuts the pop off mid-flight, so this
// has to stay the longer of the two — see the motion layer in styles.css.
const MARK_HOLD_MS = 340;

export function PlanList({ todos, compact = false }: { todos: Todo[]; compact?: boolean }) {
  // .anim-mark-done fires on the item whose status changed, and on no other:
  // the previous statuses are compared by index, so one item completing does
  // not pop the whole plan. The set clears a margin past the end of the
  // gesture so a later completion in the same list animates again.
  //
  // The timeout is held in a ref rather than cleared by this effect's own
  // cleanup — the same shape, and for the same reason, as the hold in
  // components/ui/Ticker.tsx. A plan updates repeatedly while a run works and
  // every update re-runs this effect, so a cleanup here would cancel the clear
  // that the run which just fired the animation had scheduled, and the early
  // return below would never schedule a replacement. The set would then hold
  // its indices for the life of the component, and the item that completed
  // could never pop again.
  const prevStatuses = useRef<Todo["status"][]>(todos.map((t) => t.status));
  const [justDone, setJustDone] = useState<ReadonlySet<number>>(() => new Set());
  const timer = useRef(0);

  useEffect(() => {
    const before = prevStatuses.current;
    prevStatuses.current = todos.map((t) => t.status);
    const changed = new Set<number>();
    todos.forEach((t, i) => {
      if (t.status === "completed" && before[i] !== undefined && before[i] !== "completed") changed.add(i);
    });
    if (changed.size === 0) return;
    setJustDone(changed);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setJustDone(new Set()), MARK_HOLD_MS);
  }, [todos]);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  // compact is the in-flight card's own rendering (SessionListScreen.tsx),
  // denser than the transcript's plan panel and with its own status
  // vocabulary: a grid row instead of a flex row, a completed item stays
  // struck through in the plan panel but not here (its ✓ still turns
  // --status-done), and an in_progress item goes semibold rather than
  // changing the row's own color.
  return (
    <ul className={cn("m-0 list-none p-0", compact ? "flex flex-col gap-px" : "flex flex-col gap-1")}>
      {todos.map((t, i) => (
        <li
          key={i}
          className={cn(
            compact
              ? "grid grid-cols-[16px_minmax(0,1fr)] items-baseline gap-2 border-l-2 border-transparent py-[3px] pr-0 pl-2 text-sm"
              : "flex items-baseline gap-1.5",
            !compact && t.status === "completed" && "text-muted-foreground line-through",
            compact && t.status === "completed" && "text-muted-foreground",
            compact && t.status === "in_progress" && "font-medium",
            compact && t.status === "pending" && "text-foreground",
          )}
          title={t.description}
        >
          <span
            className={cn(
              "w-[1em] flex-none",
              compact && "text-center leading-[1.5]",
              t.status === "in_progress"
                ? "text-[var(--status-running)]"
                : compact && t.status === "completed"
                  ? "text-[var(--status-done)]"
                  : "text-muted-foreground",
              justDone.has(i) && "anim-mark-done",
            )}
          >
            {STATUS_MARK[t.status]}
          </span>
          <span>{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </li>
      ))}
    </ul>
  );
}
