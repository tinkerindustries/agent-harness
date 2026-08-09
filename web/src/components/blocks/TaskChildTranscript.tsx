import { Suspense, lazy, useState } from "react";

// A Task call's subagent runs in its own session, entirely off the parent's
// message array (docs/TOOLS.md "Task"); the browser reflects that by
// rendering it as a collapsed child transcript rather than inlining it into
// the parent's block list (PLAN.md phase 5). Collapsed by default, so a long
// session with many Task calls does not open an SSE connection — and pay
// TaskChildBody's module weight — for every one of them; only expanding
// pays that cost. The lazy import also breaks what would otherwise be a
// static cycle: TaskChildBody renders BlockList, which dispatches through
// ToolResultBlock, which is what renders this component.
const TaskChildBody = lazy(() => import("./TaskChildBody"));

export function TaskChildTranscript({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="task-child">
      <button className="collapse-toggle" onClick={() => setOpen((o) => !o)}>
        {open ? "Hide" : "Show"} subagent transcript <code className="tool-detail">{sessionId}</code>
      </button>
      {open && (
        <Suspense fallback={<p className="dim">Loading…</p>}>
          <TaskChildBody sessionId={sessionId} />
        </Suspense>
      )}
    </div>
  );
}
