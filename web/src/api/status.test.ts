import { describe, expect, it } from "vitest";
import { canResume, canSteer, isLive } from "./status";

// The session-status vocabulary: every place that means "live" or "steerable"
// goes through these two helpers (status.ts), so the list, the screens and
// the operations logic cannot drift apart about what a status means.
describe("isLive", () => {
  it("counts a creating session as in-flight", () => {
    expect(isLive("creating")).toBe(true);
    expect(isLive("running")).toBe(true);
  });

  it("does not count a terminal session as live", () => {
    for (const status of ["ok", "failed", "timeout", "max_turns", "cancelled", "compacted"]) {
      expect(isLive(status)).toBe(false);
    }
  });
});

describe("canSteer", () => {
  it("is running only: a creating session has no loop to read a steer", () => {
    expect(canSteer("running")).toBe(true);
    expect(canSteer("creating")).toBe(false);
    for (const status of ["ok", "failed", "timeout", "max_turns", "cancelled", "compacted"]) {
      expect(canSteer(status)).toBe(false);
    }
  });
});

describe("canResume", () => {
  it("covers every terminal status, not just a clean finish", () => {
    for (const status of ["ok", "failed", "timeout", "max_turns", "cancelled"]) {
      expect(canResume(status)).toBe(true);
    }
  });

  it("refuses a live session, which wants a steer instead", () => {
    expect(canResume("running")).toBe(false);
    expect(canResume("creating")).toBe(false);
  });

  it("refuses a compacted session, whose continuation is its child", () => {
    expect(canResume("compacted")).toBe(false);
  });

  it("is canSteer's other half: between them they cover all but creating and compacted", () => {
    for (const status of ["running", "ok", "failed", "timeout", "max_turns", "cancelled"]) {
      expect(canSteer(status) || canResume(status)).toBe(true);
    }
    for (const status of ["creating", "compacted"]) {
      expect(canSteer(status) || canResume(status)).toBe(false);
    }
  });
});
