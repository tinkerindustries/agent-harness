import { describe, expect, it } from "vitest";
import type { ToolCallPayload } from "../../api/types";
import { parseToolArgs, toolDetail } from "./toolArgs";

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
