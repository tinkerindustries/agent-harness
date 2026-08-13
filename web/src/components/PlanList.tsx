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

export function PlanList({ todos }: { todos: Todo[] }) {
  // .anim-mark-done fires on the item whose status changed, and on no other:
  // the previous statuses are compared by index, so one item completing does
  // not pop the whole plan. The set clears after the 220ms gesture (260ms, the
  // same margin Ticker holds its outgoing value for) so a later completion in
  // the same list animates again.
  const prevStatuses = useRef<Todo["status"][]>(todos.map((t) => t.status));
  const [justDone, setJustDone] = useState<ReadonlySet<number>>(() => new Set());

  useEffect(() => {
    const before = prevStatuses.current;
    prevStatuses.current = todos.map((t) => t.status);
    const changed = new Set<number>();
    todos.forEach((t, i) => {
      if (t.status === "completed" && before[i] !== undefined && before[i] !== "completed") changed.add(i);
    });
    if (changed.size === 0) return;
    setJustDone(changed);
    const id = window.setTimeout(() => setJustDone(new Set()), 260);
    return () => window.clearTimeout(id);
  }, [todos]);

  return (
    <ul className="plan-list">
      {todos.map((t, i) => (
        <li key={i} className={`todo-item todo-${t.status}`} title={t.description}>
          <span className={cn("todo-mark", justDone.has(i) && "anim-mark-done")}>{STATUS_MARK[t.status]}</span>
          <span className="todo-text">{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </li>
      ))}
    </ul>
  );
}
