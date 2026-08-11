import { describe, expect, it } from "vitest";
import type { DiffLine, ToolDeniedPayload, ToolResultPayload } from "../../api/types";
import { cachePercent, elideLines, toolStat, type ToolResultLike } from "./turnHelpers";

// The turn renderer's helpers are the one part of the redesign that has
// edges (TESTING.md: no DOM harness, so the logic that is not JSX gets the
// unit tests). toolStat pins the "single most useful number" rule of
// design/session-chat.html; elideLines pins the head/tail collapse of
// design/README.md's "no scroll container inside the turn list".

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
