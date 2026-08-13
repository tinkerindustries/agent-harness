import { memo } from "react";
import type { Block } from "../../api/fold";
import { Markdown } from "../../render/Markdown";
import { languageForPath } from "../../render/highlight";
import { CollapsibleOutput } from "./CollapsibleOutput";
import { DiffTable } from "./DiffTable";
import { ScreenshotGallery } from "./ScreenshotGallery";
import { TaskChildTranscript } from "./TaskChildTranscript";
import { parseToolArgs, screenshotPaths, toolDetail } from "./toolArgs";

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

// ToolResultBody is the "one shape per tool" rendering docs/TOOLS.md asks
// for: Edit gets a diff table, Bash and file reads get
// syntax-highlighted, collapsible text, WebFetch's prose gets markdown, and
// Task gets a link into the subagent's own transcript. Every other tool
// falls back to plain collapsible text. Exported so the sub-turn card's tool
// cards can reuse exactly the same bodies their standalone blocks render.
export function ToolResultBody({ block }: { block: ToolResultData }) {
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

    case "Screenshot":
      // The capture tool's own result is a line of metadata (what it wrote,
      // at what size). The image is the point, so it leads and the text
      // follows it.
      return (
        <>
          <ScreenshotGallery paths={screenshotPaths(block.call)} />
          <p className="block-text dim">{block.content}</p>
        </>
      );

    case "ReviewScreenshot":
      // Gemini's findings are a JSON list, so they get the same collapsible
      // verbatim rendering every other structured output gets — but above
      // them sit the images it was actually looking at, which is the only
      // way a human reading this can check the finding against the page.
      return (
        <>
          <ScreenshotGallery paths={screenshotPaths(block.call)} />
          <CollapsibleOutput text={block.content} language="json" />
        </>
      );

    case "TaskCreate":
    case "TaskUpdate":
      // The plan mutations' result (the rendered checklist / the patched
      // line) is exactly what the plan panel shows, so the transcript keeps
      // a dim one-liner rather than echoing the whole list.
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
    case "TaskGet":
    case "TaskList":
    default:
      // TaskGet/TaskList are reads whose output (one task or the checklist)
      // is worth reading in the transcript, so they fall through to the same
      // verbatim rendering every other tool's result gets.
      return <CollapsibleOutput text={block.content} />;
  }
}
