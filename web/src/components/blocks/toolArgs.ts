import type { DiffLine, ToolCallPayload } from "../../api/types";

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

// Every path a session touches lives under its own workspace root
// (/workspaces/<session id>/), so an absolute path spends its first fifty
// characters saying what the Session panel already says — the same prefix on
// every row of a 34-sub-turn run, pushing the part that differs off the end
// of the line. trimWorkspace drops it, leaving the path as the model would
// have written it in the repo. Display only: the tool row's expanded body
// and the arguments the run actually made are untouched, and a path outside
// a workspace (an absolute path elsewhere on the box) is left alone, because
// there the leading slash is the fact.
const WORKSPACE_ROOT = /^\/workspaces\/[^/]+(\/|$)/;

export function trimWorkspace(path: string): string {
  if (!WORKSPACE_ROOT.test(path)) return path;
  const rest = path.replace(WORKSPACE_ROOT, "");
  // The workspace root itself, named rather than left as an empty string.
  return rest === "" ? "workspace root" : rest;
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
      // A command is a literal the operator may want to run themselves, so it
      // keeps its absolute paths.
      return str(args.command);
    case "Read":
    case "Write":
    case "List":
      return trimWorkspace(str(args.file_path) || str(args.path));
    case "Edit":
      return trimWorkspace(str(args.file_path));
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

// exitCode extracts the Bash tool's "[exit code N]" trailer from an error
// result (internal/tools/bash.go appends it), for the failed-call badge on a
// tool header. Empty when the result is not a Bash failure or the trailer is
// absent (older sessions).
export function exitCode(name: string, content: string): string {
  if (name !== "Bash") return "";
  const m = content.match(/\[exit code (\d+)\]/);
  return m ? `exit ${m[1]}` : "";
}

// formatCost trims a fixed-six-decimal cost to its significant digits, so a
// figure reads $0.00035 rather than $0.000350 (shared by the sub-turn header
// and the Task tool header's child stat).
export function formatCost(cost: number): string {
  if (!Number.isFinite(cost) || cost <= 0) return "0";
  return cost.toFixed(6).replace(/\.?0+$/, "");
}

// diffCounts counts the added and removed lines of a computed diff — the raw
// figures behind the +n −n stat. Shared by diffStat (the string form, used
// by the old tool headers) and the turn renderer's coloured diffstat
// parts, so the counting lives in one place.
export function diffCounts(diff: DiffLine[] | undefined): { adds: number; removes: number } {
  let adds = 0;
  let removes = 0;
  for (const line of diff ?? []) {
    if (line.kind === "add") adds++;
    else if (line.kind === "remove") removes++;
  }
  return { adds, removes };
}

// diffStat is the +n −n figure an Edit/Write tool header carries, counted
// from the result's diff. A zero side is suppressed rather than printed as
// "+0" or "−0", and an empty diff yields an empty stat.
export function diffStat(diff: DiffLine[] | undefined): string {
  const { adds, removes } = diffCounts(diff);
  const parts: string[] = [];
  if (adds > 0) parts.push(`+${adds}`);
  if (removes > 0) parts.push(`−${removes}`);
  return parts.join(" ");
}

// childStat is the Task tool header's trailing figure: the child session's
// sub-turn count and cost. A zero sub-turn count is suppressed rather than
// printed as "0 sub-turns" — the count only appears when it means something.
export function childStat(subTurns: number, costUsd: number): string {
  const details: string[] = [];
  if (subTurns > 0) details.push(`${subTurns} sub-turn${subTurns === 1 ? "" : "s"}`);
  const cost = formatCost(costUsd);
  if (cost !== "0") details.push(`$${cost}`);
  return details.length > 0 ? `child · ${details.join(" · ")}` : "child";
}

// ToolHeader is a tool call's one-line summary (.tool > summary): the
// tool name, the target the call acts on rather than
// its raw arguments JSON, and the trailing stat — the +n −n from the diff
// for Edit/Write, the child session's figures for a Task.
export interface ToolHeader {
  name: string;
  target: string;
  stat: string;
}

// toolHeader builds the header from the call the fold keeps plus the
// result-side facts the call alone cannot know (the diff, the child session
// figures). diff and child are optional so the header renders sensibly
// before a result lands.
export function toolHeader(
  call: ToolCallPayload | undefined,
  extras?: { diff?: DiffLine[]; child?: { subTurns: number; costUsd: number } },
): ToolHeader {
  if (!call) return { name: "", target: "", stat: "" };
  let stat = "";
  if (call.name === "Edit" || call.name === "Write") stat = diffStat(extras?.diff);
  else if (call.name === "Task" && extras?.child) stat = childStat(extras.child.subTurns, extras.child.costUsd);
  return { name: call.name, target: toolDetail(call), stat };
}

// ToolGlyph is one timeline-rail glyph: a monospace letter per tool,
// coloured by family — writes green, shell blue, everything else
// neutral. "err" is never produced here; the
// rail overrides a glyph whose result failed with { letter: "!", family:
// "err" }.
export interface ToolGlyph {
  letter: string;
  family: "write" | "shell" | "other" | "err";
}

// The design table's letters for the tools it names; anything else falls
// back to its first letter (Glob → G, Complete → C, ...), neutral family.
// All four plan tools share one "P" glyph so the timeline rail keeps one
// recognizable plan marker instead of four first-letter glyphs that would
// clash with each other and with Grep's G (TaskGet → G, TaskList → L, ...).
const GLYPH_BY_NAME: Record<string, ToolGlyph> = {
  Edit: { letter: "E", family: "write" },
  Write: { letter: "W", family: "write" },
  Bash: { letter: "B", family: "shell" },
  Read: { letter: "R", family: "other" },
  Grep: { letter: "G", family: "other" },
  Task: { letter: "T", family: "other" },
  TaskCreate: { letter: "P", family: "other" },
  TaskGet: { letter: "P", family: "other" },
  TaskList: { letter: "P", family: "other" },
  TaskUpdate: { letter: "P", family: "other" },
};

export function toolGlyph(name: string): ToolGlyph {
  return GLYPH_BY_NAME[name] ?? { letter: name.charAt(0) || "?", family: "other" };
}
