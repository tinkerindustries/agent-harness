import { describe, expect, it } from "vitest";
import type { DiffLine, ToolDeniedPayload, ToolResultPayload } from "../../api/types";
import type { SubTurnGroup } from "../../api/groups";
import {
  cachePercent,
  elideLines,
  finishedBandText,
  formatRunDuration,
  groupMatchesQuery,
  pendingWaitLabel,
  toolStat,
  type ToolResultLike,
} from "./turnHelpers";

// The turn renderer's helpers are the one part of the turn vocabulary that
// has edges (TESTING.md: no DOM harness, so the logic that is not JSX gets
// the unit tests). toolStat pins the "single most useful number" rule;
// elideLines pins the head/tail collapse of "no scroll container inside
// the turn list".

function ok(name: string, content: string, extra: Partial<ToolResultPayload> = {}): ToolResultLike {
  return { type: "tool_result", seq: 1, tool_call_id: "c1", name, content, ...extra };
}

function denied(name: string, rule: string, content: string): ToolResultLike {
  const payload: ToolDeniedPayload = { tool_call_id: "c1", name, rule, content };
  return { type: "tool_denied", seq: 1, ...payload };
}

const diff = (kinds: DiffLine["kind"][]): DiffLine[] => kinds.map((kind) => ({ kind, text: "x" }));

describe("toolStat", () => {
  it("shows an edit's diff as coloured +n −n parts", () => {
    expect(toolStat(ok("Edit", "", { diff: diff(["add", "add", "add", "remove", "remove"]) }))).toEqual([
      { text: "+3", cls: "add" },
      { text: "−2", cls: "del" },
    ]);
  });

  it("suppresses a zero side of an edit's diff rather than printing +0 or −0", () => {
    expect(toolStat(ok("Write", "", { diff: diff(["add", "add"]) }))).toEqual([{ text: "+2", cls: "add" }]);
    expect(toolStat(ok("Edit", "", { diff: diff(["remove"]) }))).toEqual([{ text: "−1", cls: "del" }]);
  });

  it("leaves an edit with no diff statless", () => {
    expect(toolStat(ok("Edit", "ok"))).toEqual([]);
  });

  it("shows the exit code of a failed command, read from the harness's trailer", () => {
    expect(toolStat(ok("Bash", "line\n\n[exit code 127]"))).toEqual([{ text: "exit 127" }]);
  });

  it("reads a successful command as exit 0", () => {
    expect(toolStat(ok("Bash", "(no output)"))).toEqual([{ text: "exit 0" }]);
  });

  it("reads a Bash failure without the trailer (timeout, wedged pipe) as failed", () => {
    expect(toolStat(ok("Bash", "command timed out", { is_error: true }))).toEqual([{ text: "failed" }]);
  });

  it("counts a read's lines, pluralising one line", () => {
    expect(toolStat(ok("Read", "a\nb\nc"))).toEqual([{ text: "3 lines" }]);
    expect(toolStat(ok("Read", "solo"))).toEqual([{ text: "1 line" }]);
    expect(toolStat(ok("Read", ""))).toEqual([{ text: "1 line" }]);
  });

  it("gives a failed read no count", () => {
    expect(toolStat(ok("Read", "no such file", { is_error: true }))).toEqual([]);
  });

  it("leaves tools with no useful single figure statless, and a denial too", () => {
    expect(toolStat(ok("Grep", "hits"))).toEqual([]);
    expect(toolStat(ok("TaskCreate", "plan updated"))).toEqual([]);
    expect(toolStat(denied("Bash", "readonly", "denied"))).toEqual([]);
  });
});

describe("elideLines", () => {
  it("renders short output whole — nothing to elide", () => {
    expect(elideLines("")).toBeNull();
    expect(elideLines("a\nb\nc")).toBeNull();
  });

  it("leaves an output at exactly the threshold whole", () => {
    const text = Array.from({ length: 40 }, (_, i) => `line ${i + 1}`).join("\n");
    expect(elideLines(text)).toBeNull();
  });

  it("splits one line past the threshold into head and tail, with the hidden count", () => {
    const text = Array.from({ length: 41 }, (_, i) => `line ${i + 1}`).join("\n");
    expect(elideLines(text)).toEqual({
      head: Array.from({ length: 20 }, (_, i) => `line ${i + 1}`).join("\n"),
      tail: Array.from({ length: 10 }, (_, i) => `line ${i + 32}`).join("\n"),
      hidden: 11,
    });
  });

  it("reconstructs the original text from head, the hidden lines, and tail", () => {
    const text = Array.from({ length: 60 }, (_, i) => `line ${i + 1}`).join("\n");
    const split = elideLines(text);
    expect(split).not.toBeNull();
    expect([split!.head, split!.tail].join("\n").split("\n").length).toBe(30);
    expect(split!.hidden).toBe(30);
    // head and tail are both exact slices of the original, in order.
    expect(text.startsWith(split!.head)).toBe(true);
    expect(text.endsWith(split!.tail)).toBe(true);
  });
});

describe("cachePercent", () => {
  it("formats a whole-number percentage without the decimal", () => {
    expect(cachePercent(0, 100)).toBe("0");
    expect(cachePercent(100, 0)).toBe("100");
  });

  it("formats a fractional percentage to one decimal", () => {
    expect(cachePercent(907, 93)).toBe("90.7");
    expect(cachePercent(50, 50)).toBe("50");
  });

  it("renders a zero token total as 0", () => {
    expect(cachePercent(0, 0)).toBe("0");
  });
});

describe("groupMatchesQuery", () => {
  // A group with an assistant block, an Edit call and a Bash result, the
  // shape the find box searches over.
  const group = (overrides: Partial<SubTurnGroup> = {}): SubTurnGroup => ({
    subTurn: 1,
    seq: 10,
    blocks: [
      {
        type: "assistant",
        seq: 10,
        subTurn: 1,
        reasoning: "the reasoning text",
        content: "the prose text",
        toolCalls: [
          { index: 0, id: "e1", name: "Edit", arguments: '{"file_path":"internal/mcp/launch.go"}' },
          { index: 1, id: "b1", name: "Bash", arguments: '{"command":"go vet ./..."}' },
        ],
        finishReason: "tool_calls",
      },
      { type: "tool_result", seq: 11, tool_call_id: "b1", name: "Bash", content: "build output text" },
    ],
    tags: { edits: 1, bash: 1, errors: 0, churn: false },
    phase: { id: 1, index: 1, label: "Do the thing" },
    ...overrides,
  });
  // getToolCall returns the same calls the fold keeps.
  const assistant = (): Extract<SubTurnGroup["blocks"][number], { type: "assistant" }> => {
    const a = group().blocks[0];
    if (a.type !== "assistant") throw new Error("test group must open with an assistant block");
    return a;
  };
  const getToolCall = (id: string) =>
    id === "e1" ? assistant().toolCalls[0] : id === "b1" ? assistant().toolCalls[1] : undefined;

  it("matches a blank query against everything", () => {
    expect(groupMatchesQuery(group(), "", getToolCall)).toBe(true);
    expect(groupMatchesQuery(group(), "   ", getToolCall)).toBe(true);
  });

  it("searches the assistant's prose and reasoning, case-insensitively", () => {
    expect(groupMatchesQuery(group(), "PROSE", getToolCall)).toBe(true);
    expect(groupMatchesQuery(group(), "reasoning", getToolCall)).toBe(true);
    expect(groupMatchesQuery(group(), "absent", getToolCall)).toBe(false);
  });

  it("searches the tool call's name and one-line target, not its arguments JSON", () => {
    // "launch.go" is the Edit's target; "file_path" exists only in the raw
    // arguments and must not match.
    expect(groupMatchesQuery(group(), "launch.go", getToolCall)).toBe(true);
    expect(groupMatchesQuery(group(), "Edit", getToolCall)).toBe(true);
    expect(groupMatchesQuery(group(), "file_path", getToolCall)).toBe(false);
  });

  it("searches the tool results, including a denial's rule", () => {
    expect(groupMatchesQuery(group(), "build output", getToolCall)).toBe(true);
    const denied: SubTurnGroup = {
      ...group(),
      blocks: [
        group().blocks[0],
        { type: "tool_denied", seq: 11, tool_call_id: "b1", name: "Bash", rule: "readonly", content: "denied" },
      ],
    };
    expect(groupMatchesQuery(denied, "readonly", getToolCall)).toBe(true);
  });
});

describe("pendingWaitLabel", () => {
  it("names the tool round when a tool call is running", () => {
    expect(pendingWaitLabel(true, null)).toBe("waiting for the current tool call to finish");
    expect(pendingWaitLabel(true, 7)).toBe("waiting for the current tool call to finish");
  });

  it("names the streaming turn when one is live", () => {
    expect(pendingWaitLabel(false, 7)).toBe("waiting for this sub-turn to finish");
  });

  it("names the boundary when the loop is between turns", () => {
    expect(pendingWaitLabel(false, null)).toBe("waiting for the next sub-turn boundary");
  });
});

describe("formatRunDuration", () => {
  it("renders seconds under a minute", () => {
    expect(formatRunDuration(0)).toBe("0s");
    expect(formatRunDuration(42_000)).toBe("42s");
    expect(formatRunDuration(9_940)).toBe("10s");
  });

  it("renders minutes with the seconds", () => {
    expect(formatRunDuration(991_000)).toBe("16m 31s");
    expect(formatRunDuration(252_000)).toBe("4m 12s");
  });

  it("renders hours rounded to the minute", () => {
    expect(formatRunDuration(3_660_000)).toBe("1h 1m");
    expect(formatRunDuration(3_631_000)).toBe("1h 0m");
  });

  it("clamps a negative duration to zero", () => {
    expect(formatRunDuration(-5)).toBe("0s");
  });
});

describe("finishedBandText", () => {
  it("states a finished run's duration, sub-turns, and cost", () => {
    expect(finishedBandText("ok", 991_000, 78, 0.0838, "DONE")).toBe(
      "Finished in 16m 31s over 78 sub-turns for $0.0838.",
    );
  });

  it("pluralises one sub-turn", () => {
    expect(finishedBandText("ok", 42_000, 1, 0.001, "DONE")).toBe("Finished in 42s over 1 sub-turn for $0.001.");
  });

  it("says a cancelled run was stopped by you, at the sub-turn it was in", () => {
    expect(finishedBandText("cancelled", 252_000, 13, 0.0042, "CANCELLED")).toBe(
      "You stopped this run at 4m 12s, during sub-turn 13.",
    );
  });

  it("says a cancelled run stopped before it started", () => {
    expect(finishedBandText("cancelled", 2_000, 0, 0, "CANCELLED")).toBe(
      "You stopped this run at 2s, before it started.",
    );
  });

  it("leads a non-ok, non-cancelled end with its outcome label", () => {
    expect(finishedBandText("failed", 60_000, 3, 0.01, "FAILED")).toBe(
      "FAILED in 1m 0s over 3 sub-turns for $0.01.",
    );
  });
});
