import type { ToolCallPayload } from "../../api/types";

// parseToolArgs safely decodes a tool call's arguments for display purposes
// only. A malformed or still-assembling payload (arguments can arrive mid-
// stream) just yields an empty object rather than throwing — nothing here
// feeds back into the loop, so a parse failure only means a block renders
// with a missing detail, not a broken transcript.
export function parseToolArgs(call: ToolCallPayload | undefined): Record<string, unknown> {
  if (!call) return {};
  try {
    const parsed = JSON.parse(call.arguments) as unknown;
    if (parsed && typeof parsed === "object") return parsed as Record<string, unknown>;
  } catch {
    // ignore
  }
  return {};
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

// toolDetail is the one-line descriptor shown next to a tool's name in its
// block label — the same idea as internal/tools/descriptor.go's
// descriptorFor, kept independent since the browser only needs it for
// display, not for permission matching.
export function toolDetail(call: ToolCallPayload | undefined): string {
  if (!call) return "";
  const args = parseToolArgs(call);
  switch (call.name) {
    case "Bash":
      return str(args.command);
    case "Read":
    case "Write":
    case "List":
      return str(args.file_path) || str(args.path);
    case "Edit":
      return str(args.file_path);
    case "Glob":
    case "Grep":
      return str(args.pattern);
    case "Task":
      return str(args.description);
    case "WebFetch":
      return str(args.url);
    default:
      return "";
  }
}
