import type { LiveTurn, PendingTool } from "../../api/fold";
import { ReasoningPanel } from "./ReasoningPanel";
import { toolDetail } from "./toolArgs";

// LiveAssistantBlock and LivePendingToolBlock are the "streaming block
// renders as plain preformatted text" half of docs/DESIGN.md §5.3: no
// markdown parse, no highlighting, no diff computation while the data is
// still growing. Neither is wrapped in React.memo — they are expected to
// re-render on every flush, which is the point; AssistantBlock and
// ToolResultBlock take over once the corresponding event freezes the block,
// at which point these two stop being rendered at all.

export function LiveAssistantBlock({ turn }: { turn: LiveTurn }) {
  return (
    <section className="block block-assistant block-live">
      <div className="block-label">sub-turn {turn.subTurn} — streaming…</div>
      {turn.reasoning && <ReasoningPanel text={turn.reasoning} defaultOpen startedAt={turn.startedAt} />}
      {turn.content && <pre className="block-text live-pre">{turn.content}</pre>}
      {turn.toolCalls.map((call) => (
        <div className="tool-call" key={call.id}>
          <code>
            {call.name}({call.arguments})
          </code>
        </div>
      ))}
    </section>
  );
}

export function LivePendingToolBlock({ toolCallId, pending }: { toolCallId: string; pending: PendingTool }) {
  const detail = toolDetail(pending.call);
  return (
    <section className={`block block-tool-pending tool-${pending.call.name}`} data-tool-call-id={toolCallId}>
      <div className="block-label">
        {pending.call.name}
        {detail && <code className="tool-detail"> {detail}</code>} — running…
      </div>
      {pending.stdout && <pre className="block-pre live-pre">{pending.stdout}</pre>}
    </section>
  );
}
