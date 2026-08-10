import { describe, expect, it } from "vitest";
import type { DiffLine, ToolCallPayload } from "../../api/types";
import { childStat, diffStat, formatCost, parseToolArgs, toolDetail, toolHeader } from "./toolArgs";

function call(name: string, args: unknown): ToolCallPayload {
  return { index: 0, id: "c1", name, arguments: JSON.stringify(args) };
}

describe("parseToolArgs", () => {
  it("returns an empty object for an undefined call", () => {
    expect(parseToolArgs(undefined)).toEqual({});
  });

  it("returns an empty object for malformed JSON rather than throwing", () => {
    const malformed: ToolCallPayload = { index: 0, id: "c1", name: "Bash", arguments: "{not json" };
    expect(() => parseToolArgs(malformed)).not.toThrow();
    expect(parseToolArgs(malformed)).toEqual({});
  });

  it("parses well-formed arguments", () => {
    expect(parseToolArgs(call("Bash", { command: "ls" }))).toEqual({ command: "ls" });
  });
});

describe("toolDetail", () => {
  it("shows the command for Bash", () => {
    expect(toolDetail(call("Bash", { command: "npm test" }))).toBe("npm test");
  });

  it("prefers file_path, falling back to path, for Read/Write/List", () => {
    expect(toolDetail(call("Read", { file_path: "a.go" }))).toBe("a.go");
    expect(toolDetail(call("List", { path: "src/" }))).toBe("src/");
  });

  it("shows the pattern for Glob and Grep", () => {
    expect(toolDetail(call("Grep", { pattern: "TODO" }))).toBe("TODO");
    expect(toolDetail(call("Glob", { pattern: "**/*.go" }))).toBe("**/*.go");
  });

  it("shows the description for Task and the url for WebFetch", () => {
    expect(toolDetail(call("Task", { description: "look something up" }))).toBe("look something up");
    expect(toolDetail(call("WebFetch", { url: "https://example.com" }))).toBe("https://example.com");
  });

  it("returns an empty string for a tool with no special-cased detail, or no call at all", () => {
    expect(toolDetail(call("TodoWrite", { todos: [] }))).toBe("");
    expect(toolDetail(undefined)).toBe("");
  });
});

describe("diffStat", () => {
  const line = (kind: DiffLine["kind"]): DiffLine => ({ kind, text: "x" });

  it("counts additions and removals as +n −n", () => {
    expect(diffStat([line("add"), line("add"), line("remove")])).toBe("+2 −1");
  });

  it("suppresses a zero side rather than printing −0 or +0", () => {
    expect(diffStat([line("add"), line("add")])).toBe("+2");
    expect(diffStat([line("remove"), line("remove"), line("remove")])).toBe("−3");
    expect(diffStat([line("context")])).toBe("");
  });

  it("returns an empty stat for an empty or missing diff", () => {
    expect(diffStat(undefined)).toBe("");
    expect(diffStat([])).toBe("");
  });
});

describe("formatCost", () => {
  it("trims a fixed-six-decimal cost to its significant digits", () => {
    expect(formatCost(0.00035)).toBe("0.00035");
    expect(formatCost(0.00035)).not.toBe("0.000350");
    expect(formatCost(1)).toBe("1");
  });

  it("renders a non-positive cost as 0", () => {
    expect(formatCost(0)).toBe("0");
    expect(formatCost(-1)).toBe("0");
  });
});

describe("childStat", () => {
  it("shows the child session's sub-turn count and cost", () => {
    expect(childStat(11, 0.0043)).toBe("child · 11 sub-turns · $0.0043");
    expect(childStat(1, 0.0043)).toBe("child · 1 sub-turn · $0.0043");
  });

  it("suppresses a zero sub-turn count rather than printing 0 sub-turns", () => {
    expect(childStat(0, 0.0043)).toBe("child · $0.0043");
    expect(childStat(0, 0)).toBe("child");
  });
});

describe("toolHeader", () => {
  it("builds the Edit header from the call and its diff", () => {
    const diff: DiffLine[] = [
      { kind: "remove", text: "a", old_line: 1 },
      { kind: "add", text: "b", new_line: 1 },
      { kind: "add", text: "c", new_line: 2 },
    ];
    expect(toolHeader(call("Edit", { file_path: "internal/httplog/recorder.go" }), { diff })).toEqual({
      name: "Edit",
      target: "internal/httplog/recorder.go",
      stat: "+2 −1",
    });
  });

  it("shows the path for Write and the pattern for Grep", () => {
    expect(toolHeader(call("Write", { file_path: "web/src/api/groups.ts" })).target).toBe("web/src/api/groups.ts");
    expect(toolHeader(call("Grep", { pattern: "churn_point" })).target).toBe("churn_point");
  });

  it("shows the command for Bash and the description for Task", () => {
    expect(toolHeader(call("Bash", { command: "go build ./..." })).target).toBe("go build ./...");
    expect(toolHeader(call("Task", { description: "survey the docs" })).target).toBe("survey the docs");
  });

  it("appends the child stat to a Task header when the child figures are known", () => {
    const header = toolHeader(call("Task", { description: "survey the docs" }), { child: { subTurns: 11, costUsd: 0.0043 } });
    expect(header.stat).toBe("child · 11 sub-turns · $0.0043");
    expect(header.name).toBe("Task");
  });

  it("returns empty parts for no call at all", () => {
    expect(toolHeader(undefined)).toEqual({ name: "", target: "", stat: "" });
  });
});
