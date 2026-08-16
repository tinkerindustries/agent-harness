import { Suspense, lazy, useState } from "react";
import { Button } from "../ui/button";
import { TOOL_DETAIL_CLS } from "./blockStyles";

// A Task call's subagent runs in its own session, entirely off the parent's
// message array (docs/TOOLS.md "Task"); the browser reflects that by
// rendering it as a collapsed child transcript rather than inlining it into
// the parent's block list. Collapsed by default, so a long
// session with many Task calls does not open an SSE connection — and pay
// TaskChildBody's module weight — for every one of them; only expanding
// pays that cost. The lazy import also breaks what would otherwise be a
// static cycle: TaskChildBody renders BlockList, which dispatches through
// ToolResultBlock, which is what renders this component.
const TaskChildBody = lazy(() => import("./TaskChildBody"));

export function TaskChildTranscript({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="mt-2 flex flex-col gap-2">
      <Button variant="outline" size="sm" className="text-[var(--status-running)]" onClick={() => setOpen((o) => !o)}>
        {open ? "Hide" : "Show"} subagent transcript <code className={TOOL_DETAIL_CLS}>{sessionId}</code>
      </Button>
      {open && (
        <Suspense fallback={<p className="text-muted-foreground">Loading…</p>}>
          <TaskChildBody sessionId={sessionId} />
        </Suspense>
      )}
    </div>
  );
}
