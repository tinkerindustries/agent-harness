import { describe, expect, it } from "vitest";

import { tickerSlots } from "./tickerSlots";

/** The characters that carry an outgoing value, i.e. the ones that roll. */
const moved = (shown: string, prev: string | null) =>
  tickerSlots(shown, prev)
    .filter((s) => s.prev !== null)
    .map((s) => `${s.prev}->${s.char}`);

describe("tickerSlots", () => {
  it("rolls nothing on the first render", () => {
    expect(moved("6:45", null)).toEqual([]);
    expect(tickerSlots("6:45", null).map((s) => s.char).join("")).toBe("6:45");
  });

  it("rolls only the digit that changed", () => {
    expect(moved("6:45", "6:44")).toEqual(["4->5"]);
    expect(moved("$0.0508", "$0.0507")).toEqual(["7->8"]);
    expect(moved("99.1%", "98.1%")).toEqual(["8->9"]);
  });

  it("rolls nothing when the figure is unchanged", () => {
    expect(moved("6:45", "6:45")).toEqual([]);
  });

  it("carries: the leading digit arrives without an outgoing character", () => {
    // "9:59" to "10:00" — the colon holds still, the three digits roll, and
    // the new leading "1" rolls in from nothing.
    const slots = tickerSlots("10:00", "9:59");
    expect(slots.map((s) => s.char).join("")).toBe("10:00");
    expect(slots[0]).toEqual({ key: 4, char: "1", prev: null });
    expect(slots[2]).toEqual({ key: 2, char: ":", prev: null });
    expect(moved("10:00", "9:59")).toEqual(["9->0", "5->0", "9->0"]);
  });

  it("aligns on the right when the figure shrinks", () => {
    expect(moved("9", "10")).toEqual(["0->9"]);
  });

  it("keys a position by its distance from the end, so a carry does not renumber it", () => {
    const before = tickerSlots("9:59", null);
    const after = tickerSlots("10:00", "9:59");
    const colonBefore = before.find((s) => s.char === ":");
    const colonAfter = after.find((s) => s.char === ":");
    expect(colonBefore?.key).toBe(2);
    expect(colonAfter?.key).toBe(2);
  });

  it("keeps a space wide enough to see", () => {
    // An ordinary space alone in an inline-block box collapses to no width.
    expect(tickerSlots("1h 5m", null)[2].char).toBe("\u00a0");
    expect(tickerSlots("1h 5m", "1h 4m")[2]).toEqual({ key: 2, char: "\u00a0", prev: null });
  });

  it("handles a figure that had nothing to show", () => {
    expect(moved("6:45", "—")).toEqual(["—->5"]);
    expect(tickerSlots("6:45", "—")).toHaveLength(4);
  });
});
