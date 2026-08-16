import { memo } from "react";
import type { Block } from "../../api/fold";
import { BLOCK_CLS, BLOCK_LABEL_CLS, BLOCK_TEXT_CLS } from "./blockStyles";
import { cn } from "@/lib/utils";

export const UsageBlock = memo(function UsageBlock({ block }: { block: Extract<Block, { type: "usage" }> }) {
  return (
    // .block-usage's own padding (4px 8px) replaces .block's 8px outright
    // rather than adding to it, so this is written straight through instead
    // of starting from BLOCK_CLS the way the other loose blocks do.
    <div className="rounded-[calc(var(--radius)-4px)] border border-border bg-card py-1 px-2 text-xs text-muted-foreground">
      {/* Named only when it is not the session's own model — a vision call
          bills separately and lands as a second usage row on the same
          sub-turn, and without the name the two read as one turn billed
          twice (docs/TOOLS.md, "What it costs, and who can see that"). */}
      {block.model && <span className="font-medium text-foreground">{block.model} · </span>}
      {/* A summed event says so. Transcribe bills a call per chunk of a tall
          image and commits one event for the lot (internal/tools/transcribe.go),
          so without this the row reads as one request with fifteen times the
          usual token count. Absent on every ordinary event, which is one call. */}
      {block.calls !== undefined && block.calls > 1 && <span>{block.calls} calls · </span>}
      prompt {block.prompt_tokens} (hit {block.prompt_cache_hit_tokens} / miss {block.prompt_cache_miss_tokens}), completion{" "}
      {block.completion_tokens}, cost ${block.cost_usd.toFixed(6)}
      {block.churn_point_index !== undefined && (
        <span className="text-[var(--status-gaveup)]"> — churn at message {block.churn_point_index}</span>
      )}
    </div>
  );
});

export const RunFinishedBlock = memo(function RunFinishedBlock({ block }: { block: Extract<Block, { type: "run_finished" }> }) {
  return (
    // "block" stays a literal class only so .turn-list > .block's margin
    // still finds this element when it renders as a session page's top-level
    // block (blockStyles.ts's own comment covers why that spacing can't
    // travel with the component itself).
    <section className={cn(BLOCK_CLS, "border-[var(--status-running)] bg-card", "block")}>
      <div className={BLOCK_LABEL_CLS}>run finished: {block.reason}</div>
      {block.summary && <p className={BLOCK_TEXT_CLS}>{block.summary}</p>}
      {block.text && <p className={BLOCK_TEXT_CLS}>{block.text}</p>}
    </section>
  );
});

export const ErrorBlock = memo(function ErrorBlock({ block }: { block: Extract<Block, { type: "error" }> }) {
  return (
    // "block" residual — see RunFinishedBlock's comment above.
    <section className={cn(BLOCK_CLS, "border-[var(--status-failed)] bg-card text-[var(--status-failed)]", "block")}>
      <div className={BLOCK_LABEL_CLS}>error</div>
      <p className={BLOCK_TEXT_CLS}>{block.message}</p>
    </section>
  );
});

// InstructionBlock renders the launching agent's instruction as a plain
// top-level block — the default, used where no page has a message-shaped
// rendering of it (the chat page and the perf harnesses). The watch page
// replaces it with its .msg-user rendering through TurnTranscript's
// renderInstruction ("from claude-code · delivered · sub-turn 1").
export const InstructionBlock = memo(function InstructionBlock({ block }: { block: Extract<Block, { type: "instruction" }> }) {
  return (
    // "block" residual — see RunFinishedBlock's comment above.
    <section className={cn(BLOCK_CLS, "border-border bg-card", "block")}>
      <div className={BLOCK_LABEL_CLS}>instruction</div>
      <p className={BLOCK_TEXT_CLS}>{block.text}</p>
    </section>
  );
});

// SteerBlock renders an operator steer (docs/RUN-CONTROL.md "The frontend"):
// an operator instruction the loop will fold into the model's next request.
// It is deliberately styled distinctly from the model's own turns — a
// top-level block, not a sub-turn card — and the two states are the point:
// *pending* (only the steer_message event exists) is the operator's signal
// that the run is wedged or still mid-tool-call, and *delivered* (the
// matching steer_applied arrived) is the run having actually picked it up.
//
// The two states pick one fully-resolved border/background/label string
// each rather than layering a shared base with a per-state override: both
// touch border-color, and two utilities that set the same CSS property on
// one element race each other in Tailwind's own output order, not the order
// they're listed in the className — the codebase's established way around
// that (Turn.tsx's ToolRow isError branch) is to never let two happen at
// once. BLOCK_CLS's frame (radius, border width, padding) still applies to
// both; only color/style/background differ.
export const SteerBlock = memo(function SteerBlock({ block }: { block: Extract<Block, { type: "steer" }> }) {
  const pending = block.state === "pending";
  return (
    <section
      className={cn(
        BLOCK_CLS,
        "border-l-[3px]",
        pending
          ? "border-dashed border-[var(--status-gaveup)] bg-[var(--status-gaveup-bg)]"
          : "border-border bg-[color-mix(in_srgb,var(--muted)_40%,transparent)]",
        // "block" residual — see RunFinishedBlock's comment above.
        "block",
      )}
    >
      <div className={cn("mb-1 text-xs uppercase", pending ? "font-semibold text-[var(--status-gaveup)]" : "text-[var(--status-done)]")}>
        {pending ? "steer · sent, not yet delivered" : "steer · delivered"}
      </div>
      <p className={BLOCK_TEXT_CLS}>{block.text}</p>
    </section>
  );
});
