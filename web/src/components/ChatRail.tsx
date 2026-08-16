import type { Todo } from "../api/types";
import { ElidedPath } from "./ui/ElidedPath";
import { cn } from "@/lib/utils";

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
    // "rail" stays a literal class: .app .rail's overflow-y:auto reaches in
    // from a body-level class SessionScreen computes outside React. Every
    // other rail/facts property below is now a direct Tailwind
    // utility, shared with WatchRail.tsx's identical chrome.
    <aside className="rail border-l border-border px-4 pt-4 pb-6">
      <div className="[&+&]:mt-5 [&+&]:border-t [&+&]:border-border [&+&]:pt-4">
        <h3 className="m-0 mb-2.5 text-micro font-semibold tracking-[0.06em] text-muted-foreground uppercase">Plan</h3>
        {todos.length === 0 ? (
          <p className="m-0 text-xs text-muted-foreground">No plan — this run never wrote one.</p>
        ) : (
          <PlanProgress todos={todos} />
        )}
      </div>
      <div className="[&+&]:mt-5 [&+&]:border-t [&+&]:border-border [&+&]:pt-4">
        <h3 className="m-0 mb-2.5 text-micro font-semibold tracking-[0.06em] text-muted-foreground uppercase">Session</h3>
        <div className="[display:block] text-xs text-muted-foreground">
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>model</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{facts.model}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>effort</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{facts.effort}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>started by</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{facts.startedBy}</span>
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>workspace</span>
            <ElidedPath
              className="v min-w-0 truncate text-right font-mono text-micro text-foreground"
              path={facts.workspace}
              keepSession
            />
          </div>
          <div className="flex justify-between gap-2.5 py-[3px]">
            <span>request</span>
            <span className="min-w-0 truncate text-right font-mono text-micro text-foreground">{facts.requestId ?? "—"}</span>
          </div>
        </div>
      </div>
    </aside>
  );
}

// PlanProgress is the plan section: the progress bar with the completed
// ratio, then one row per item — the done items
// dimmed with a ✓, the in-progress item highlighted with its activeForm, the
// rest pending. The fold's own plan (snapshot.todos) is the only source;
// nothing here re-parses a TaskCreate call.
function PlanProgress({ todos }: { todos: Todo[] }) {
  const done = todos.filter((t) => t.status === "completed").length;
  const pct = Math.round((done / todos.length) * 100);
  return (
    <>
      <div className="mb-2.5 flex items-center gap-2">
        <span className="h-1 flex-1 overflow-hidden rounded-full bg-muted" aria-hidden>
          <span className="[display:block] h-full bg-[var(--status-running)]" style={{ width: `${pct}%` }} />
        </span>
        <span className="text-micro text-muted-foreground tabular-nums">
          {done}/{todos.length}
        </span>
      </div>
      {todos.map((t) => (
        <div
          key={t.taskId}
          className={cn(
            "flex gap-2 py-1 text-sm leading-[1.45]",
            t.status === "completed" && "text-muted-foreground",
            t.status === "in_progress" &&
              "-mx-2 my-0.5 rounded-[calc(var(--radius)-3px)] bg-[var(--status-running-bg)] px-2 py-[5px] font-medium",
          )}
        >
          <span
            className={cn(
              "w-3 flex-none font-mono",
              t.status === "completed"
                ? "text-[var(--status-done)]"
                : t.status === "in_progress"
                  ? "text-[var(--status-running)]"
                  : "text-muted-foreground",
            )}
            aria-hidden
          >
            {t.status === "completed" ? "✓" : t.status === "in_progress" ? "▸" : "○"}
          </span>
          <span>{t.status === "in_progress" ? t.activeForm : t.subject}</span>
        </div>
      ))}
    </>
  );
}
