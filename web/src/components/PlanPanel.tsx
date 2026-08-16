import type { Todo } from "../api/types";
import { PlanList } from "./PlanList";

// PlanPanel is pinned beside the transcript, driven by the plan the fold
// keeps from the TaskCreate/TaskUpdate calls (docs/TOOLS.md "TaskCreate,
// TaskGet, TaskList, TaskUpdate": "A web UI can pin it as a live plan panel
// beside the transcript, which is one of the clearer wins the browser buys
// us"). It renders nothing until the model writes a first plan, and nothing
// again once the list is cleared — TranscriptStore already treats an empty
// todos array as "no plan", not "hide the last one" (see FoldState.ingest).
// The list itself is PlanList, shared with the in-flight card so the two
// render the same glyphs.
export function PlanPanel({ todos }: { todos: Todo[] }) {
  if (todos.length === 0) return null;
  return (
    <aside className="sticky top-2 flex-none basis-[240px] rounded-[calc(var(--radius)-4px)] border border-border bg-card p-2 text-[0.85rem] max-panel:static max-panel:w-full">
      <h2 className="mb-2 text-xs text-muted-foreground uppercase">Plan</h2>
      <PlanList todos={todos} />
    </aside>
  );
}
