import type { Todo } from "../api/types";
import { ElidedPath } from "./ui/ElidedPath";

// ChatRail is the chat page's right-hand rail (.rail.rail-right): the
// plan with its progress bar and the in-progress item highlighted, then
// the session facts. The watch page has its own, separate rail;
// this one is the chat page's only rail, and it deliberately replaces the
// filter chips, the timeline rail, and the plan panel — the design drops all
// three from the interactive page and lets the plan carry the navigation.
export function ChatRail({
  todos,
  facts,
}: {
  todos: Todo[];
  facts: {
    model: string;
    effort: string;
    startedBy: string;
    workspace: string;
    requestId?: string;
  };
}) {
  return (
    <aside className="rail rail-right">
      <div className="railsec">
        <h3>Plan</h3>
        {todos.length === 0 ? (
          <p className="rail-note">No plan — this run never wrote one.</p>
        ) : (
          <PlanProgress todos={todos} />
        )}
      </div>
      <div className="railsec">
        <h3>Session</h3>
        <div className="facts">
          <div>
            <span>model</span>
            <span className="v">{facts.model}</span>
          </div>
          <div>
            <span>effort</span>
            <span className="v">{facts.effort}</span>
          </div>
          <div>
            <span>started by</span>
            <span className="v">{facts.startedBy}</span>
          </div>
          <div>
            <span>workspace</span>
            <ElidedPath className="v" path={facts.workspace} keepSession />
          </div>
          <div>
            <span>request</span>
            <span className="v">{facts.requestId ?? "—"}</span>
          </div>
        </div>
      </div>
    </aside>
  );
}

// PlanProgress is the plan section: the progress bar with the completed
// ratio, then one .planrow per item — the done items
// dimmed with a ✓, the in-progress item highlighted with its activeForm, the
// rest pending. The fold's own plan (snapshot.todos) is the only source;
// nothing here re-parses a TaskCreate call.
function PlanProgress({ todos }: { todos: Todo[] }) {
  const done = todos.filter((t) => t.status === "completed").length;
  const pct = Math.round((done / todos.length) * 100);
  return (
    <>
      <div className="planhead">
        <span className="progress" aria-hidden>
          <span style={{ width: `${pct}%` }} />
        </span>
        <span className="count">
          {done}/{todos.length}
        </span>
      </div>
      {todos.map((t) => (
        <div
          key={t.taskId}
          className={`planrow${t.status === "completed" ? " plan-done" : t.status === "in_progress" ? " plan-now" : ""}`}
        >
          <span className="mark" aria-hidden>
            {t.status === "completed" ? "✓" : t.status === "in_progress" ? "▸" : "○"}
          </span>
          <span>{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </div>
      ))}
    </>
  );
}
