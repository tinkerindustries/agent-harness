import { describe, expect, it } from "vitest";
import type { DiffLine, ToolCallPayload } from "../../api/types";
import { parseSkillCatalogue } from "../../api/skillCatalogue";
import {
  childStat,
  diffStat,
  formatCost,
  parseToolArgs,
  screenshotPaths,
  screenshotUrl,
  skillOpened,
  toolDetail,
  toolGlyph,
  toolHeader,
} from "./toolArgs";

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

  it("drops the workspace root from a path, so a row leads with what differs", () => {
    const p = "/workspaces/sess-c3d7233a8a4eaba7e64d2a5ef4ee1890/agent-harness/internal/tools/descriptor.go";
    expect(toolDetail(call("Read", { file_path: p }))).toBe("agent-harness/internal/tools/descriptor.go");
    expect(toolDetail(call("Edit", { file_path: p }))).toBe("agent-harness/internal/tools/descriptor.go");
    expect(toolDetail(call("List", { path: "/workspaces/sess-abc/agent-harness/internal" }))).toBe(
      "agent-harness/internal",
    );
  });

  it("names the workspace root rather than rendering it as an empty target", () => {
    expect(toolDetail(call("List", { path: "/workspaces/sess-abc" }))).toBe("workspace root");
    expect(toolDetail(call("List", { path: "/workspaces/sess-abc/" }))).toBe("workspace root");
  });

  it("leaves paths outside a workspace, and Bash commands, exactly as they are", () => {
    expect(toolDetail(call("Read", { file_path: "/etc/hosts" }))).toBe("/etc/hosts");
    expect(toolDetail(call("Read", { file_path: "relative/path.go" }))).toBe("relative/path.go");
    expect(toolDetail(call("Bash", { command: "cd /workspaces/sess-abc/repo && git status" }))).toBe(
      "cd /workspaces/sess-abc/repo && git status",
    );
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
    expect(toolDetail(call("TaskList", {}))).toBe("");
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

describe("toolGlyph", () => {
  it("maps the design table's letters and families", () => {
    expect(toolGlyph("Edit")).toEqual({ letter: "E", family: "write" });
    expect(toolGlyph("Write")).toEqual({ letter: "W", family: "write" });
    expect(toolGlyph("Bash")).toEqual({ letter: "B", family: "shell" });
    expect(toolGlyph("Read")).toEqual({ letter: "R", family: "other" });
    expect(toolGlyph("Grep")).toEqual({ letter: "G", family: "other" });
    expect(toolGlyph("Task")).toEqual({ letter: "T", family: "other" });
  });

  it("gives all four plan tools the same plan glyph, so the rail never falls back to clashing first letters", () => {
    // TaskGet's first letter would collide with Grep's G and TaskList's with
    // List's L; the shared P keeps the plan family recognizable.
    for (const name of ["TaskCreate", "TaskGet", "TaskList", "TaskUpdate"]) {
      expect(toolGlyph(name)).toEqual({ letter: "P", family: "other" });
    }
  });

  it("falls back to the tool's first letter, neutral family, for tools the table does not name", () => {
    expect(toolGlyph("Glob")).toEqual({ letter: "G", family: "other" });
    expect(toolGlyph("Complete")).toEqual({ letter: "C", family: "other" });
    expect(toolGlyph("")).toEqual({ letter: "?", family: "other" });
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

describe("screenshotPaths", () => {
  it("reads Glance's image_paths array", () => {
    const c = call("Glance", { image_paths: ["scratch/a.png", "scratch/b.webp"], query: "?" });
    expect(screenshotPaths(c)).toEqual(["scratch/a.png", "scratch/b.webp"]);
  });

  it("reads Screenshot's single path", () => {
    expect(screenshotPaths(call("Screenshot", { url: "http://x", path: "scratch/home.png" }))).toEqual(["scratch/home.png"]);
  });

  it("reads Ground's and Detect's single image_path", () => {
    expect(screenshotPaths(call("Ground", { image_path: "scratch/a.png", target: "the submit button" }))).toEqual([
      "scratch/a.png",
    ]);
    expect(screenshotPaths(call("Detect", { image_path: "scratch/a.png", category: "buttons" }))).toEqual(["scratch/a.png"]);
  });

  it("reads the image Transcribe was pointed at", () => {
    const c = call("Transcribe", { image_path: "scratch/page.png" });
    expect(screenshotPaths(c)).toEqual(["scratch/page.png"]);
  });

  it("reads Crop's own output rather than the image it read", () => {
    const c = call("Crop", { image_path: "scratch/a.png", region: "0,0,10,10", output: "scratch/cropped.png" });
    expect(screenshotPaths(c)).toEqual(["scratch/cropped.png"]);
  });

  it("falls back to execCrop's own default output name when output is omitted", () => {
    const c = call("Crop", { image_path: "scratch/a.png", region: "0,0,10,10" });
    expect(screenshotPaths(c)).toEqual(["scratch/a.crop.png"]);
  });

  it("keeps the given order, because the first image is the high-resolution one", () => {
    const c = call("Glance", { image_paths: ["z.png", "a.png"] });
    expect(screenshotPaths(c)).toEqual(["z.png", "a.png"]);
  });

  it("drops duplicates so one file is not fetched and rendered twice", () => {
    const c = call("Glance", { image_paths: ["a.png", "a.png"] });
    expect(screenshotPaths(c)).toEqual(["a.png"]);
  });

  // Arguments arrive mid-stream, so a half-assembled array is a normal
  // intermediate state and must not throw or render junk.
  it("survives partial and malformed arguments", () => {
    expect(screenshotPaths(undefined)).toEqual([]);
    expect(screenshotPaths({ index: 0, id: "c1", name: "Glance", arguments: '{"image_paths":["a.p' })).toEqual([]);
    expect(screenshotPaths(call("Glance", {}))).toEqual([]);
    expect(screenshotPaths(call("Glance", { image_paths: "not-an-array.png" }))).toEqual([]);
    expect(screenshotPaths(call("Glance", { image_paths: [1, null, "", "  ", "ok.png"] }))).toEqual(["ok.png"]);
    expect(screenshotPaths(call("Ground", {}))).toEqual([]);
    expect(screenshotPaths(call("Crop", {}))).toEqual([]);
    expect(screenshotPaths(call("Transcribe", {}))).toEqual([]);
  });

  // The endpoint serves three types and refuses the rest, so asking for
  // anything else would only ever produce a broken image.
  it("keeps only the extensions the endpoint serves", () => {
    const c = call("Glance", { image_paths: ["a.png", "b.JPEG", "c.jpg", "d.webp", "e.gif", "f.txt", "g"] });
    expect(screenshotPaths(c)).toEqual(["a.png", "b.JPEG", "c.jpg", "d.webp"]);
  });
});

describe("screenshotUrl", () => {
  it("puts the path in the query string, encoded", () => {
    expect(screenshotUrl("sess-1", "scratch/home page.png")).toBe(
      "/api/sessions/sess-1/screenshot?path=scratch%2Fhome%20page.png",
    );
  });

  it("encodes an absolute path, which is the form the model usually passes", () => {
    expect(screenshotUrl("sess-1", "/workspaces/sess-1/scratch/a.png")).toBe(
      "/api/sessions/sess-1/screenshot?path=%2Fworkspaces%2Fsess-1%2Fscratch%2Fa.png",
    );
  });
});

describe("toolDetail for the screenshot and vision tools", () => {
  it("shows the URL a Screenshot call captured", () => {
    expect(toolDetail(call("Screenshot", { url: "http://127.0.0.1:5173/", path: "scratch/a.png" }))).toBe("http://127.0.0.1:5173/");
  });

  it("names the first image and counts the rest, so the header stays one line", () => {
    expect(toolDetail(call("Glance", { image_paths: ["/workspaces/sess-1/scratch/a.png"] }))).toBe("scratch/a.png");
    expect(toolDetail(call("Glance", { image_paths: ["scratch/a.png", "b.png", "c.png"] }))).toBe("scratch/a.png +2");
    expect(toolDetail(call("Glance", {}))).toBe("");
  });

  it("shows the target for Ground and the category for Detect", () => {
    expect(toolDetail(call("Ground", { image_path: "scratch/a.png", target: "the submit button" }))).toBe(
      "the submit button",
    );
    expect(toolDetail(call("Detect", { image_path: "scratch/a.png", category: "buttons" }))).toBe("buttons");
    expect(toolDetail(call("Detect", {}))).toBe("");
  });

  it("shows the image Transcribe read, trimmed against the workspace", () => {
    expect(toolDetail(call("Transcribe", { image_path: "/workspaces/sess-1/scratch/page.png" }))).toBe(
      "scratch/page.png",
    );
  });

  it("shows the file Crop wrote, trimmed the same way a Read/Write path is", () => {
    const p = "/workspaces/sess-1/scratch/a.png";
    expect(toolDetail(call("Crop", { image_path: p, region: "0,0,10,10", output: "scratch/cropped.png" }))).toBe(
      "scratch/cropped.png",
    );
    expect(toolDetail(call("Crop", { image_path: p, region: "0,0,10,10" }))).toBe("scratch/a.crop.png");
  });
});

describe("toolGlyph for the screenshot and vision tools", () => {
  it("gives each vision tool a letter that cannot be confused with Read's, Grep's, Glob's or Complete's", () => {
    expect(toolGlyph("Screenshot").letter).toBe("S");
    expect(toolGlyph("Glance").letter).toBe("V");
    expect(toolGlyph("Ground").letter).toBe("X");
    expect(toolGlyph("Detect").letter).toBe("D");
    expect(toolGlyph("Crop").letter).toBe("K");
    // Transcribe cannot keep its own first letter either: T is Task's.
    expect(toolGlyph("Transcribe").letter).toBe("O");
    expect(toolGlyph("Task").letter).toBe("T");
    expect(toolGlyph("Read").letter).toBe("R");
  });
});

describe("skillOpened", () => {
  const catalogue = parseSkillCatalogue(
    "- test-runner (repo/.claude/skills/test-runner/SKILL.md): Run the suite.\n",
  );
  const inWorkspace = "/workspaces/sess-abc/repo/.claude/skills/test-runner/SKILL.md";

  it("names the skill a Read of a catalogued SKILL.md opens", () => {
    expect(skillOpened(call("Read", { file_path: inWorkspace }), catalogue)).toBe("test-runner");
  });

  it("matches a path the model wrote relative to the workspace root", () => {
    expect(skillOpened(call("Read", { file_path: "repo/.claude/skills/test-runner/SKILL.md" }), catalogue)).toBe(
      "test-runner",
    );
  });

  it("stays silent for a SKILL.md the catalogue did not offer", () => {
    // The case that decides this is worth a catalogue lookup at all: a run
    // whose work is skills reads SKILL.md files it was never offered, and
    // badging those would call editing a skill pack a skill load.
    const other = "/workspaces/sess-abc/repo/assets/skill-packs/unity/probe/SKILL.md";
    expect(skillOpened(call("Read", { file_path: other }), catalogue)).toBe("");
  });

  it("only counts Read", () => {
    expect(skillOpened(call("Edit", { file_path: inWorkspace }), catalogue)).toBe("");
    expect(skillOpened(call("Grep", { pattern: "SKILL.md" }), catalogue)).toBe("");
  });

  it("stays silent with no catalogue and no call", () => {
    expect(skillOpened(call("Read", { file_path: inWorkspace }), new Map())).toBe("");
    expect(skillOpened(undefined, catalogue)).toBe("");
  });
});
