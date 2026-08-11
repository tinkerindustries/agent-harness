import type { Todo } from "../api/types";

// The one todo-list renderer in the app, shared by the transcript's plan
// panel (PlanPanel) and the in-flight card's expanded plan
// (docs/WEB-REDESIGN.md phase 3). One component, one set of glyphs and
// status classes, so the two screens cannot drift apart. An in_progress
// item shows its activeForm ("Wiring six call sites…") rather than its
// static subject, exactly as the transcript renders it today. The item's
// description rides as a hover tooltip, so the panel stays compact while
// the longer explanation is still one hover away.
export const STATUS_MARK: Record<Todo["status"], string> = {
  completed: "✓",
  in_progress: "…",
  pending: "○",
};

export function PlanList({ todos }: { todos: Todo[] }) {
  return (
    <ul className="plan-list">
      {todos.map((t, i) => (
        <li key={i} className={`todo-item todo-${t.status}`} title={t.description}>
          <span className="todo-mark">{STATUS_MARK[t.status]}</span>
          <span className="todo-text">{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </li>
      ))}
    </ul>
  );
}
