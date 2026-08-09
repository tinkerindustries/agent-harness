import { memo } from "react";
import type { Block } from "../../api/fold";
import { Markdown } from "../../render/Markdown";
import { languageForPath } from "../../render/highlight";
import { CollapsibleOutput } from "./CollapsibleOutput";
import { DiffTable } from "./DiffTable";
import { TaskChildTranscript } from "./TaskChildTranscript";
import { parseToolArgs, toolDetail } from "./toolArgs";

type ToolResultData = Extract<Block, { type: "tool_result" }>;

// ToolResultBlock is the "one shape per tool" rendering docs/TOOLS.md asks
// for: Edit gets a diff table, Bash and file reads get
// syntax-highlighted, collapsible text, WebFetch's prose gets markdown, and
// Task gets a link into the subagent's own transcript. Every other tool
// falls back to plain collapsible text.
export const ToolResultBlock = memo(function ToolResultBlock({ block }: { block: ToolResultData }) {
  return (
    <section className={`block block-tool-result tool-${block.name}${block.is_error ? " block-tool-error" : ""}`}>
      <div className="block-label">
        {block.name}
        {toolDetail(block.call) && <code className="tool-detail"> {toolDetail(block.call)}</code>}
        {block.is_error && " (error)"}
        {block.truncated && " (truncated)"}
      </div>
      <ToolResultBody block={block} />
    </section>
  );
});

function ToolResultBody({ block }: { block: ToolResultData }) {
  switch (block.name) {
    case "Edit":
      return block.diff && block.diff.length > 0 ? <DiffTable diff={block.diff} /> : <CollapsibleOutput text={block.content} />;

    case "Read": {
      const args = parseToolArgs(block.call);
      return <CollapsibleOutput text={block.content} language={languageForPath(args.file_path as string | undefined)} />;
    }

    case "Bash":
      return <CollapsibleOutput text={block.content} language="bash" />;

    case "WebFetch":
      return <Markdown text={block.content} />;

    case "TodoWrite":
      return <p className="block-text dim">Plan updated — see the panel.</p>;

    case "Task":
      return (
        <>
          <p className="block-text">{block.content}</p>
          {block.child_session_id && <TaskChildTranscript sessionId={block.child_session_id} />}
        </>
      );

    case "Write":
    case "Glob":
    case "Grep":
    case "List":
    default:
      return <CollapsibleOutput text={block.content} />;
  }
}
