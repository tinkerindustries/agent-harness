import type { LiveTurn, PendingTool } from "../../api/fold";
import { ReasoningPanel } from "./ReasoningPanel";
import { toolDetail } from "./toolArgs";
import { BLOCK_CLS, BLOCK_LABEL_CLS, BLOCK_PRE_CLS, BLOCK_TEXT_CLS, TOOL_CALL_CLS, TOOL_DETAIL_CLS } from "./blockStyles";
import { cn } from "@/lib/utils";

// LiveAssistantBlock and LivePendingToolBlock are the "streaming block
// renders as plain preformatted text" half of docs/DESIGN.md §5.3: no
// markdown parse, no highlighting, no diff computation while the data is
// still growing. Neither is wrapped in React.memo — they are expected to
// re-render on every flush, which is the point; AssistantBlock and
// ToolResultBlock take over once the corresponding event freezes the block,
// at which point these two stop being rendered at all.

export function LiveAssistantBlock({ turn }: { turn: LiveTurn }) {
  return (
    <section className={cn(BLOCK_CLS, "border-border bg-card border-dashed", "block-assistant")}>
      <div className={BLOCK_LABEL_CLS}>sub-turn {turn.subTurn} — streaming…</div>
      {turn.reasoning && <ReasoningPanel text={turn.reasoning} defaultOpen startedAt={turn.startedAt} />}
      {turn.content && <pre className={cn(BLOCK_TEXT_CLS, "text-[0.85rem] text-foreground")}>{turn.content}</pre>}
      {turn.toolCalls.map((call) => (
        <div className={TOOL_CALL_CLS} key={call.id}>
          <code>
            {call.name}
            {toolDetail(call) && <span className={TOOL_DETAIL_CLS}> → {toolDetail(call)}</span>}
          </code>
        </div>
      ))}
    </section>
  );
}

export function LivePendingToolBlock({ toolCallId, pending }: { toolCallId: string; pending: PendingTool }) {
  const detail = toolDetail(pending.call);
  return (
    <section
      className={cn(BLOCK_CLS, "border-border bg-card border-dashed text-muted-foreground", `tool-${pending.call.name}`)}
      data-tool-call-id={toolCallId}
    >
      <div className={BLOCK_LABEL_CLS}>
        {pending.call.name}
        {detail && <code className={TOOL_DETAIL_CLS}> {detail}</code>} — running…
      </div>
      {pending.stdout && <pre className={cn(BLOCK_PRE_CLS, "text-foreground")}>{pending.stdout}</pre>}
    </section>
  );
}
