import { memo } from "react";
import type { Block } from "../../api/fold";
import { Markdown } from "../../render/Markdown";
import { ReasoningPanel } from "./ReasoningPanel";

// AssistantBlock is frozen the moment turn_finished lands: reasoning and
// content are complete text, parsed and highlighted exactly once by
// ReasoningPanel/Markdown, then wrapped in React.memo so this component
// never runs its render body again for the rest of the session
// (docs/DESIGN.md §5.2, "399 of 400 blocks are inert"). It is the boxed,
// labelled rendering used for loose blocks; inside a sub-turn card
// (SubTurnCard) the same content renders bare via AssistantBody, where the
// card header already carries the sub-turn number (design/transcript.html).
export const AssistantBlock = memo(function AssistantBlock({ block }: { block: Extract<Block, { type: "assistant" }> }) {
  return (
    <section className="block block-assistant">
      <div className="block-label">sub-turn {block.subTurn}</div>
      <AssistantBody block={block} />
    </section>
  );
});

// AssistantBody is the assistant block's content without the boxed wrapper
// or the sub-turn label — how a frozen assistant renders inside its sub-turn
// card. Not memoised: it is only ever reached through a memoised parent
// (AssistantBlock or SubTurnCard), so the memo sits one level up.
export function AssistantBody({ block }: { block: Extract<Block, { type: "assistant" }> }) {
  return (
    <>
      {block.reasoning && (
        <ReasoningPanel text={block.reasoning} defaultOpen={false} elapsedMs={block.reasoningElapsedMs} tokens={block.reasoningTokens} />
      )}
      {block.content && <Markdown text={block.content} />}
      {block.toolCalls.map((call) => (
        <div className="tool-call" key={call.id}>
          <code>
            {call.name}({call.arguments})
          </code>
        </div>
      ))}
    </>
  );
}
