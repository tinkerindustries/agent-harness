import type { VariantProps } from "class-variance-authority";
import { badgeVariants } from "./ui/badge";

// The session outcome vocabulary: one badge label and colour per meaning.
// The store already distinguishes every row in the table below.
// complete_status is the status
// argument the model gave Complete ("done" or "gave_up"), empty when it
// never called the tool; reason is run_finished's reason ("complete",
// "no_tool_calls", "max_sub_turns"), which only the transcript stream
// carries — the session list never sees it.
type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

const STATUS_VARIANT: Record<string, BadgeVariant> = {
  running: "running",
  ok: "done",
  failed: "failed",
  timeout: "stopped",
  max_turns: "gaveup",
  cancelled: "stopped",
  compacted: "outline",
};

export function statusVariant(status: string): BadgeVariant {
  return STATUS_VARIANT[status] ?? "outline";
}

export interface OutcomeSession {
  status: string;
  complete_status?: string;
  reason?: string;
}

export interface Outcome {
  label: string;
  variant: BadgeVariant;
}

// outcome maps one session row onto the badge it should carry. Both the
// session list and the transcript header call it — one function, so the two
// screens cannot drift apart. The mapping:
// status ok splits on complete_status (done / gave_up) and, where the
// transcript's run_finished block is available, on reason (no_tool_calls →
// STOPPED). A status ok with neither signal is the pre-migration fallback:
// the plain terminal status renders as DONE, the one word the list already
// uses for an ok run, rather than a guess. Every other store
// status maps straight from the table.
export function outcome(session: OutcomeSession): Outcome {
  if (session.status === "ok") {
    if (session.complete_status === "done") return { label: "DONE", variant: "done" };
    if (session.complete_status === "gave_up") return { label: "GAVE UP", variant: "gaveup" };
    if (session.reason === "no_tool_calls") return { label: "STOPPED", variant: "stopped" };
    return { label: "DONE", variant: statusVariant("ok") };
  }
  switch (session.status) {
    case "running":
      return { label: "RUNNING", variant: "running" };
    case "max_turns":
      return { label: "MAX TURNS", variant: "gaveup" };
    case "failed":
      return { label: "FAILED", variant: "failed" };
    case "timeout":
      return { label: "TIMEOUT", variant: "stopped" };
    case "cancelled":
      return { label: "CANCELLED", variant: "stopped" };
    case "compacted":
      return { label: "COMPACTED", variant: "outline" };
    default:
      // An unknown status renders as itself on the neutral outline badge —
      // the pre-shadcn behaviour — rather than being guessed at.
      return { label: session.status, variant: "outline" };
  }
}

// watchBadge is the provenance strip's spectator badge (.prov): WATCHING
// while the run is live, and FINISHED once it is over. The strip's own
// sentence beside the badge says a finished run "could not be messaged"
// — so the badge must not keep claiming the operator is watching a run
// that has ended.
export function watchBadge(running: boolean): Outcome {
  return running ? { label: "WATCHING", variant: "outline" } : { label: "FINISHED", variant: "outline" };
}
