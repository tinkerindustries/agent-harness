import type { Todo } from "../api/types";

// PlanPanel is pinned beside the transcript, driven by the latest TodoWrite
// call (docs/TOOLS.md "TodoWrite": "A web UI can pin it as a live plan panel
// beside the transcript, which is one of the clearer wins the browser buys
// us"). It renders nothing until the model writes a first plan, and nothing
// again once the list is cleared — TranscriptStore already treats an empty
// todos array as "no plan", not "hide the last one" (see FoldState.ingest).
const STATUS_MARK: Record<Todo["status"], string> = {
  completed: "✓",
  in_progress: "…",
  pending: "○",
};

export function PlanPanel({ todos }: { todos: Todo[] }) {
  if (todos.length === 0) return null;
  return (
    <aside className="plan-panel">
      <h2>Plan</h2>
      <ul>
        {todos.map((t, i) => (
          <li key={i} className={`todo-item todo-${t.status}`}>
            <span className="todo-mark">{STATUS_MARK[t.status]}</span>
            <span className="todo-text">{t.status === "in_progress" ? t.activeForm : t.content}</span>
          </li>
        ))}
      </ul>
    </aside>
  );
}
