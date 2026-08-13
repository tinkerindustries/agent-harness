import { describe, expect, it } from "vitest";
import { tableEmptyState } from "./sessionListEmpty";

// The finished table must tell "a genuinely empty harness" apart from "a
// filter that matched nothing": with sessions loaded and a query matching
// none, collapsing the two would drop both sections and render nothing below
// the stat strip — no table, no message, no indication the filter was the
// reason.

describe("tableEmptyState", () => {
  it("is none while any session matches", () => {
    expect(tableEmptyState(83, 1)).toBe("none");
    expect(tableEmptyState(1, 1)).toBe("none");
  });

  it("says no-sessions for a genuinely empty harness", () => {
    expect(tableEmptyState(0, 0)).toBe("no-sessions");
  });

  it("says no-match when sessions exist but none matched the query", () => {
    expect(tableEmptyState(83, 0)).toBe("no-match");
    expect(tableEmptyState(1, 0)).toBe("no-match");
  });
});
