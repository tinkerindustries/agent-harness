import type { Block } from "../../api/fold";
import { diffCounts, exitCode } from "../blocks/toolArgs";

// turnHelpers is the pure, testable logic of the turn renderer
// (design/session-chat.html's .turn): choosing a tool row's single most
// useful number, and splitting a long tool output into a head and a tail.
// TESTING.md's rule is "test the helpers, not the components" — there is no
// DOM harness and the components are JSX only, so every edge lives here
// instead.

// ToolResultLike is a group block that pairs with a tool call: the result or
// the denial. The fold attaches every result to the call that produced it,
// so the map the turn builds over a group's blocks is these two kinds only.
export type ToolResultLike = Extract<Block, { type: "tool_result" }> | Extract<Block, { type: "tool_denied" }>;

// ToolStatPart is one piece of a tool row's trailing figure
// (design/session-chat.html's .tool .timing): a string, plus the diffstat
// marker for an edit's coloured +n/−n spans.
export interface ToolStatPart {
  text: string;
  // "add"/"del" marks a +n/−n figure of an edit's diffstat, which the turn
  // CSS colours (design/session-chat.html sub-turn 7; .diffstat .add/.del).
  cls?: "add" | "del";
}

// toolStat is the "single most useful number for that tool" rule
// (design/session-chat.html): lines for a read, exit code for a command,
// +n −n for an edit, nothing for tools with no useful single figure (a plan
// mutation, a fetch, a denial). It reads the result alone — the diff, the
// failure trailer, the output — never the call's arguments, and a zero side
// of an edit's diff is suppressed rather than printed as +0 or −0.
export function toolStat(result: ToolResultLike): ToolStatPart[] {
  if (result.type === "tool_denied") return [];
  switch (result.name) {
    case "Edit":
    case "Write": {
      const { adds, removes } = diffCounts(result.diff);
      const parts: ToolStatPart[] = [];
      if (adds > 0) parts.push({ text: `+${adds}`, cls: "add" });
      if (removes > 0) parts.push({ text: `−${removes}`, cls: "del" });
      return parts;
    }
    case "Bash": {
      // The harness appends "[exit code N]" to a failed command's output
      // (internal/tools/bash.go); a success exits 0 by construction. A Bash
      // failure without the trailer (timeout, wedged pipe) reads "failed".
      const code = exitCode(result.name, result.content);
      return [{ text: code || (result.is_error ? "failed" : "exit 0") }];
    }
    case "Read":
      // The line count of what came back, the one number that says how big
      // the read was. A failed read carries no useful count.
      if (result.is_error) return [];
      return [{ text: `${lineCount(result.content)} ${lineCount(result.content) === 1 ? "line" : "lines"}` }];
    default:
      return [];
  }
}

function lineCount(text: string): number {
  return text.split("\n").length;
}

// The collapse policy for long tool output (design/README.md: "no scroll
// container inside the turn list"): below the threshold the whole output
// renders; above it, head lines, an elided row, and tail lines, and
// expanding grows the page. The same numbers the old CollapsibleOutput used,
// so the policy does not change with the markup.
export const ELIDE_THRESHOLD = 40;
export const ELIDE_HEAD = 20;
export const ELIDE_TAIL = 10;

export interface ElidedSplit {
  head: string;
  tail: string;
  // The number of lines the elided row says are hidden.
  hidden: number;
}

// elideLines splits a long output into its head and tail halves, or returns
// null when the output is short enough to render whole. The split is by
// lines, so the head and tail each re-join to the exact text the renderer
// would have shown whole.
export function elideLines(text: string): ElidedSplit | null {
  const lines = text.split("\n");
  if (lines.length <= ELIDE_THRESHOLD) return null;
  return {
    head: lines.slice(0, ELIDE_HEAD).join("\n"),
    tail: lines.slice(-ELIDE_TAIL).join("\n"),
    hidden: lines.length - ELIDE_HEAD - ELIDE_TAIL,
  };
}

// cachePercent is the turn meta line's cache-hit figure
// (design/session-chat.html's .turnmeta: "0% cache", "90.7% cache"): one
// decimal, with a whole-number result rendered without the ".0" (0 and 100
// read as "0%" and "100%", not "0.0%" and "100.0%"). A zero total (no prompt
// tokens at all) renders as "0" — the caller decides whether to show the
// figure at all.
export function cachePercent(hitTokens: number, missTokens: number): string {
  const total = hitTokens + missTokens;
  if (total <= 0) return "0";
  const pct = (hitTokens / total) * 100;
  return Number.isInteger(pct) ? String(pct) : pct.toFixed(1);
}
