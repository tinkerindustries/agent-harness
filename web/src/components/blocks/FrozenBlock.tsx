import type { Block } from "../../api/fold";
import { AssistantBlock } from "./AssistantBlock";
import { ErrorBlock, InstructionBlock, RunFinishedBlock, SteerBlock, UsageBlock } from "./MiscBlocks";
import { OpeningBlock } from "./OpeningBlock";
import { SkillsBlock } from "./SkillsBlock";
import { ToolDeniedBlock } from "./ToolDeniedBlock";
import { ToolResultBlock } from "./ToolResultBlock";

// FrozenBlock dispatches to the memoised, per-kind component for a
// completed block. Every branch below is wrapped in React.memo at its own
// definition, keyed by the caller on block.seq and type, so a block whose
// reference hasn't changed — every block except the one that just froze —
// skips its render body entirely on the next flush (docs/DESIGN.md §5.2).
// The key carries the type because session_started yields two blocks, the
// skills catalogue and the opening message, sharing one sequence number.
// This
// function itself is intentionally not memoised: it is cheap (one switch,
// no parsing), and memoising it would only add a comparison that always
// fails, since `block` is a new union member each time React calls it with
// a different array index during BlockList's map.
export function FrozenBlock({ block }: { block: Block }) {
  switch (block.type) {
    case "opening":
      return <OpeningBlock block={block} />;
    case "skills":
      return <SkillsBlock block={block} />;
    case "assistant":
      return <AssistantBlock block={block} />;
    case "tool_result":
      return <ToolResultBlock block={block} />;
    case "tool_denied":
      return <ToolDeniedBlock block={block} />;
    case "usage":
      return <UsageBlock block={block} />;
    case "run_finished":
      return <RunFinishedBlock block={block} />;
    case "error":
      return <ErrorBlock block={block} />;
    case "steer":
      return <SteerBlock block={block} />;
    case "instruction":
      return <InstructionBlock block={block} />;
  }
}
