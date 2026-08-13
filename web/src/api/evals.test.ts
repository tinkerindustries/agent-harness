import { describe, expect, it } from "vitest";

import {
  buildComparison,
  evalRunProgress,
  formatDelta,
  formatMetric,
  groupMembersByTask,
  isRunning,
  metricLabel,
  type EvalMemberRow,
  type EvalRunDetail,
} from "./evals";

function detail(over: Partial<EvalRunDetail> = {}): EvalRunDetail {
  return {
    id: "evr-1",
    suite: "search",
    variants: ["base", "search-first"],
    replicates: 2,
    status: "ok",
    started_at: "2026-08-13T04:00:00Z",
    total: 4,
    finished: 4,
    failed: 0,
    cost_usd: 0.08,
    version: 1,
    members: [],
    summary: [],
    deltas: [],
    ...over,
  };
}

describe("formatMetric", () => {
  // The browser must read the same as the terminal, so these mirror
  // internal/evals/report.go's format exactly.
  it.each([
    ["search_via_tool", 0.715, "71.5%"],
    ["tool_error_rate", 0.0412, "4.1%"],
    ["search_via_tool_decay", -0.183, "-18.3%"],
    ["judge_completed", 1, "100.0%"],
    ["cost_usd", 0.020612, "$0.0206"],
    ["context_tokens_max", 148_300, "148k"],
    ["sub_turns", 30.25, "30.25"],
    ["judge_score", 4, "4.00"],
  ])("formats %s", (metric, value, want) => {
    expect(formatMetric(metric, value)).toBe(want);
  });
});

describe("formatDelta", () => {
  it("signs the difference, because the direction is the point", () => {
    expect(formatDelta("search_via_tool", 0.18)).toBe("+18.0%");
    expect(formatDelta("search_via_tool", -0.18)).toBe("-18.0%");
    expect(formatDelta("sub_turns", 0)).toBe("0.00");
  });
});

describe("metricLabel", () => {
  it("names a known metric and falls back to the key for an unknown one", () => {
    expect(metricLabel("search_via_tool")).toBe("Searches via Grep/Glob");
    // A metric added on the Go side and not here must still appear.
    expect(metricLabel("some_new_metric")).toBe("some_new_metric");
  });
});

describe("buildComparison", () => {
  it("puts one cell per variant in the server's metric order", () => {
    const rows = buildComparison(
      detail({
        summary: [
          { metric: "search_via_tool", variant: "base", n: 2, mean: 0.15, stddev: 0.07, stderr: 0.05 },
          { metric: "search_via_tool", variant: "search-first", n: 2, mean: 0.85, stddev: 0.07, stderr: 0.05 },
          { metric: "sub_turns", variant: "base", n: 2, mean: 30, stddev: 2, stderr: 1.4 },
          { metric: "sub_turns", variant: "search-first", n: 2, mean: 27, stddev: 2, stderr: 1.4 },
        ],
        deltas: [
          {
            metric: "search_via_tool",
            baseline_variant: "base",
            variant: "search-first",
            diff: 0.7,
            combined_stderr: 0.07,
            significant: true,
          },
        ],
      }),
    );
    expect(rows.map((r) => r.metric)).toEqual(["search_via_tool", "sub_turns"]);
    expect(rows[0].cells.map((c) => c?.mean)).toEqual([0.15, 0.85]);
    expect(rows[0].delta?.significant).toBe(true);
    // A metric with no delta from the server carries none rather than a zero.
    expect(rows[1].delta).toBeNull();
  });

  // A metric with nothing to measure under a variant is absent from the
  // summary, and must reach the table as a gap rather than as a zero — a run
  // that never searched has no share, and 0% would claim it searched badly.
  it("renders a missing summary as an absent cell, never zero", () => {
    const rows = buildComparison(
      detail({
        summary: [
          { metric: "search_via_tool", variant: "base", n: 2, mean: 0.15, stddev: 0, stderr: 0 },
        ],
      }),
    );
    expect(rows[0].cells[0]?.mean).toBe(0.15);
    expect(rows[0].cells[1]).toBeNull();
  });
});

describe("groupMembersByTask", () => {
  it("keeps the arms of one task adjacent", () => {
    const members = [
      { task_id: "a", variant: "base" },
      { task_id: "a", variant: "search-first" },
      { task_id: "b", variant: "base" },
    ] as EvalMemberRow[];
    const groups = groupMembersByTask(members);
    expect(groups.map((g) => g.taskId)).toEqual(["a", "b"]);
    expect(groups[0].members).toHaveLength(2);
  });

  it("returns nothing for a run with no members", () => {
    expect(groupMembersByTask([])).toEqual([]);
  });
});

describe("evalRunProgress", () => {
  it("is the finished fraction, and complete for a run with no members", () => {
    expect(evalRunProgress({ total: 24, finished: 6 } as never)).toBeCloseTo(0.25);
    expect(evalRunProgress({ total: 0, finished: 0 } as never)).toBe(1);
  });
});

describe("isRunning", () => {
  it("is true only while the run is going", () => {
    expect(isRunning({ status: "running" })).toBe(true);
    for (const status of ["ok", "failed", "cancelled"]) {
      expect(isRunning({ status })).toBe(false);
    }
  });
});
