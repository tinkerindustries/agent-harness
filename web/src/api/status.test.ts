import { describe, expect, it } from "vitest";
import { canSteer, isLive } from "./status";

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
