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

export function PlanList({ todos }: { todos: Todo[] }) {
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

  return (
    // "plan-list"/"todo-item"/"todo-${status}"/"todo-mark" survive as
    // literal classes even though every property below is now a direct
    // Tailwind utility: SessionListScreen.tsx's InFlightCard wraps this
    // same list in a "run-plan" div and layers a `.run-plan .plan-list
    // .todo-*` scoped override in styles.css on top, out of scope here.
    // That block is unlayered CSS and always wins over these (layered)
    // utilities wherever the two set the same property, so it still drives
    // the in-flight card's compact rendering unchanged; these utilities are
    // what actually paints the plain PlanPanel context, where no such
    // override exists.
    <ul className="plan-list m-0 flex list-none flex-col gap-1 p-0">
      {todos.map((t, i) => (
        <li
          key={i}
          className={cn(
            `todo-item todo-${t.status} flex items-baseline gap-1.5`,
            t.status === "completed" && "text-muted-foreground line-through",
          )}
          title={t.description}
        >
          <span
            className={cn(
              "todo-mark w-[1em] flex-none",
              t.status === "in_progress" ? "text-[var(--status-running)]" : "text-muted-foreground",
              justDone.has(i) && "anim-mark-done",
            )}
          >
            {STATUS_MARK[t.status]}
          </span>
          <span className="todo-text">{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </li>
      ))}
    </ul>
  );
}
