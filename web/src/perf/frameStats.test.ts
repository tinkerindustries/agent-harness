import { describe, expect, it } from "vitest";
import { computeFrameStats } from "./frameStats";

describe("computeFrameStats", () => {
  it("returns all zeros for no samples", () => {
    expect(computeFrameStats([])).toEqual({
      frames: 0,
      meanMs: 0,
      p50Ms: 0,
      p95Ms: 0,
      p99Ms: 0,
      maxMs: 0,
      over16Count: 0,
      over33Count: 0,
    });
  });

  it("computes mean, max, and the frame-budget counts over a simple sample", () => {
    const stats = computeFrameStats([10, 20, 30, 40, 50]);
    expect(stats.frames).toBe(5);
    expect(stats.meanMs).toBe(30);
    expect(stats.maxMs).toBe(50);
    // 20 is not > 16.7? it is (20 > 16.7) -> 20,30,40,50 all exceed 16.7ms
    expect(stats.over16Count).toBe(4);
    // 40 and 50 exceed 33.4ms
    expect(stats.over33Count).toBe(2);
  });

  it("does not mutate the input array while sorting for percentiles", () => {
    const input = [5, 1, 3];
    computeFrameStats(input);
    expect(input).toEqual([5, 1, 3]);
  });
});
