import { describe, expect, it } from "vitest";
import { elidePath, shortenWorkspacePaths, splitWorkspacePath } from "./workspacePath";

// The two roots that reach the browser: the host path parity produces today,
// and the container-only path sessions recorded before it. Both split, which
// is the point of anchoring on the session directory instead of the root.
const HOST = "/Users/geoff/Repos/deepseek-harness/workspaces/sess-abc123";
const NEUTRAL = "/Users/Shared/harness-workspaces/sess-abc123";
const LEGACY = "/workspaces/sess-abc123";

describe("splitWorkspacePath", () => {
  it("splits a host path into root, session and rest", () => {
    expect(splitWorkspacePath(`${HOST}/agent-harness/internal/fold/fold.go`)).toEqual({
      root: "/Users/geoff/Repos/deepseek-harness/workspaces/",
      session: "sess-abc123",
      rest: "agent-harness/internal/fold/fold.go",
    });
  });

  it("splits a relocated root the same way — the root is not hard-coded", () => {
    expect(splitWorkspacePath(`${NEUTRAL}/repo/main.go`)).toMatchObject({
      root: "/Users/Shared/harness-workspaces/",
      session: "sess-abc123",
    });
  });

  it("still splits the pre-parity container path", () => {
    expect(splitWorkspacePath(`${LEGACY}/repo/main.go`)).toEqual({
      root: "/workspaces/",
      session: "sess-abc123",
      rest: "repo/main.go",
    });
  });

  it("reports an empty rest for the workspace directory itself", () => {
    expect(splitWorkspacePath(HOST)).toMatchObject({ session: "sess-abc123", rest: "" });
  });

  it("leaves a path with no session directory alone", () => {
    expect(splitWorkspacePath("/etc/hosts")).toBeNull();
    expect(splitWorkspacePath("internal/fold/fold.go")).toBeNull();
    expect(splitWorkspacePath("")).toBeNull();
  });

  it("concatenates back to the original", () => {
    const full = `${HOST}/repo/main.go`;
    const s = splitWorkspacePath(full)!;
    expect(s.root + s.session + "/" + s.rest).toBe(full);
  });
});

describe("shortenWorkspacePaths", () => {
  it("drops the root from the opening message's workspace line", () => {
    expect(shortenWorkspacePaths(`Workspace: ${HOST}`)).toBe("Workspace: …/sess-abc123");
  });

  it("keeps the session directory and anything under it", () => {
    expect(shortenWorkspacePaths(`Workspace: ${NEUTRAL}/repo`)).toBe("Workspace: …/sess-abc123/repo");
  });

  it("leaves text with no workspace path untouched", () => {
    expect(shortenWorkspacePaths("871 words · internal/fold/fold.go")).toBe("871 words · internal/fold/fold.go");
  });

  it("does not swallow the delimiter of a path in a code span", () => {
    expect(shortenWorkspacePaths(`\`${LEGACY}/repo\``)).toBe("`…/sess-abc123/repo`");
  });
});

describe("elidePath", () => {
  it("hides root and session directory for a tool target", () => {
    const s = splitWorkspacePath(`${HOST}/agent-harness/internal/fold/fold.go`)!;
    expect(elidePath(s, false)).toEqual({
      hidden: "/Users/geoff/Repos/deepseek-harness/workspaces/sess-abc123/",
      visible: "agent-harness/internal/fold/fold.go",
    });
  });

  it("keeps the session directory for the workspace fact", () => {
    const s = splitWorkspacePath(`${HOST}/agent-harness`)!;
    expect(elidePath(s, true)).toEqual({
      hidden: "/Users/geoff/Repos/deepseek-harness/workspaces/",
      visible: "sess-abc123/agent-harness",
    });
  });

  it("names the workspace directory rather than eliding it to nothing", () => {
    const s = splitWorkspacePath(HOST)!;
    expect(elidePath(s, false).visible).toBe("workspace root");
  });

  it("puts the whole path back together across both modes", () => {
    const full = `${NEUTRAL}/repo/main.go`;
    const s = splitWorkspacePath(full)!;
    for (const keepSession of [true, false]) {
      const { hidden, visible } = elidePath(s, keepSession);
      expect(hidden + visible).toBe(full);
    }
  });
});
