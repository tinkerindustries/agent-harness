import type { Block, LiveView } from "../../api/fold";
import type { TranscriptItem } from "../../api/groups";
import type { SessionState } from "../../api/types";
import { diffCounts, exitCode, formatCost } from "../blocks/toolArgs";

// turnHelpers is the pure, testable logic of the turn renderer (.turn):
// choosing a tool row's single most useful number, and splitting a long
// tool output into a head and a tail.
// TESTING.md's rule is "test the helpers, not the components" — there is no
// DOM harness and the components are JSX only, so every edge lives here
// instead.

// ToolResultLike is a group block that pairs with a tool call: the result or
// the denial. The fold attaches every result to the call that produced it,
// so the map the turn builds over a group's blocks is these two kinds only.
export type ToolResultLike = Extract<Block, { type: "tool_result" }> | Extract<Block, { type: "tool_denied" }>;

// ToolStatPart is one piece of a tool row's trailing figure (.tool
// .timing): a string, plus the diffstat marker for an edit's coloured
// +n/−n spans.
export interface ToolStatPart {
  text: string;
  // "add"/"del" marks a +n/−n figure of an edit's diffstat, which the turn
  // CSS colours (.diffstat .add/.del).
  cls?: "add" | "del";
}

// toolStat is the "single most useful number for that tool" rule: lines
// for a read, exit code for a command,
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

// The collapse policy for long tool output ("no scroll container inside
// the turn list"): below the threshold the whole output
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

// cachePercent is the turn meta line's cache-hit figure (.turnmeta: "0%
// cache", "90.7% cache"): one decimal, with a whole-number result
// rendered without the ".0" (0 and 100
// read as "0%" and "100%", not "0.0%" and "100.0%"). A zero total (no prompt
// tokens at all) renders as "0" — the caller decides whether to show the
// figure at all.
export function cachePercent(hitTokens: number, missTokens: number): string {
  const total = hitTokens + missTokens;
  if (total <= 0) return "0";
  const pct = (hitTokens / total) * 100;
  return Number.isInteger(pct) ? String(pct) : pct.toFixed(1);
}

// --- the watch page's status line ---

// watchStatusFigures is the watch page's status line's numbers: the
// sub-turn the line names — the live turn, or the last frozen one — and
// the run's totals from the row. Shared by the live footer and the
// finished run's nav slot, so the two places cannot drift.
export function watchStatusFigures(
  meta: SessionState,
  items: TranscriptItem[],
  live: LiveView,
): {
  subTurn: number | null;
  cacheHitTokens: number;
  cacheMissTokens: number;
  costUsd: number;
  completionTokens: number;
} {
  let subTurn: number | null = live.turn?.subTurn ?? null;
  if (subTurn === null) {
    for (let i = items.length - 1; i >= 0; i--) {
      const item = items[i];
      if (item.kind === "group") {
        subTurn = item.group.subTurn;
        break;
      }
    }
  }
  return {
    subTurn,
    cacheHitTokens: meta.usage.cache_hit_tokens,
    cacheMissTokens: meta.usage.cache_miss_tokens,
    costUsd: meta.usage.cost_usd,
    completionTokens: meta.usage.completion_tokens,
  };
}

// --- the chat page's steer messages and finished band ---

// pendingWaitLabel says what a pending steer is waiting on, from the
// live view ("waiting for the current tool call to finish"). The three
// cases are exactly what the stream can be doing at a
// sub-turn boundary: a tool call still running, a turn streaming, or the
// loop between turns. The run never pauses for the message either way.
export function pendingWaitLabel(hasToolRound: boolean, liveSubTurn: number | null): string {
  if (hasToolRound) return "waiting for the current tool call to finish";
  if (liveSubTurn !== null) return "waiting for this sub-turn to finish";
  return "waiting for the next sub-turn boundary";
}

// formatRunDuration renders a finished run's wall time the way the
// .box-done band states it ("16m 31s", "4m 12s") — seconds under a
// minute, minutes with the seconds, hours rounded to the minute. The
// seconds matter at these scales because a stop confirmation is about how
// far in the run is.
export function formatRunDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "0s";
  const totalSeconds = Math.round(ms / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

// finishedBandText is the .box-done band's sentence: the outcome in the
// badge, the duration and cost in the prose. A
// cancelled run says who stopped it and how far in — "during sub-turn N",
// or "before it started" when the stop landed before the first sub-turn; a
// run that ended any other way says how long it took, how many sub-turns it
// spanned, and what it cost. outcomeLabel is the statusBadge label ("DONE",
// "CANCELLED", ...) the badge next to the sentence already carries.
export function finishedBandText(
  status: string,
  durationMs: number,
  subTurns: number,
  costUsd: number,
  outcomeLabel: string,
): string {
  const duration = formatRunDuration(durationMs);
  if (status === "cancelled") {
    const at = subTurns > 0 ? `, during sub-turn ${subTurns}` : ", before it started";
    return `You stopped this run at ${duration}${at}.`;
  }
  const verb = status === "ok" ? "Finished" : outcomeLabel;
  return `${verb} in ${duration} over ${subTurns} sub-turn${subTurns === 1 ? "" : "s"} for $${formatCost(costUsd)}.`;
}
