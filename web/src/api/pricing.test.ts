import { describe, expect, it } from "vitest";
import { isPeakAt, localWindows, minutesUntilTierChange, peakNote, type PricingSchedule } from "./pricing";

// DeepSeek's own schedule (configs/prices.json): two peak windows a day in
// UTC, from 16:00 UTC on 2026-08-16.
const SCHEDULE: PricingSchedule = {
  effective_at: "2026-08-16T16:00:00Z",
  peak_windows_utc: [
    { from: "01:00", to: "04:00" },
    { from: "06:00", to: "10:00" },
  ],
  models: ["deepseek-v4-flash", "deepseek-v4-pro"],
};

// A day well after the split, so effective_at is not what any of these are
// testing.
const utc = (hh: number, mm = 0) => new Date(Date.UTC(2026, 7, 20, hh, mm));

describe("isPeakAt", () => {
  it("is false with no schedule at all", () => {
    expect(isPeakAt(undefined, utc(2))).toBe(false);
  });

  it("walks the day, with half-open windows", () => {
    // The boundaries are the point: 04:00 is off-peak and 03:59 is not. A
    // closed window would flag the run starting at 04:00 sharp as expensive.
    const cases: [number, number, boolean][] = [
      [0, 59, false],
      [1, 0, true],
      [3, 59, true],
      [4, 0, false],
      [5, 30, false],
      [6, 0, true],
      [9, 59, true],
      [10, 0, false],
      [23, 59, false],
    ];
    for (const [hh, mm, want] of cases) {
      expect(isPeakAt(SCHEDULE, utc(hh, mm)), `${hh}:${mm}`).toBe(want);
    }
  });

  it("is false before the split takes effect, even inside a window", () => {
    // 02:00 UTC on the 15th is inside a peak window as written, but nothing
    // is billed by it yet. Flagging it would be lying about a bill.
    expect(isPeakAt(SCHEDULE, new Date(Date.UTC(2026, 7, 15, 2, 0)))).toBe(false);
    // A minute before, and a minute after.
    expect(isPeakAt(SCHEDULE, new Date(Date.UTC(2026, 7, 16, 15, 59)))).toBe(false);
    expect(isPeakAt(SCHEDULE, new Date(Date.UTC(2026, 7, 17, 2, 0)))).toBe(true);
  });

  it("decides on UTC, not on the reader's clock", () => {
    // The invariant the server holds too: the same instant prices the same
    // however it is expressed. A Date IS an instant, so this is really a
    // check that nothing here reaches for a local getHours().
    const instant = utc(2);
    expect(isPeakAt(SCHEDULE, new Date(instant.toISOString()))).toBe(true);
    expect(isPeakAt(SCHEDULE, new Date(instant.getTime()))).toBe(true);
  });

  it("ignores an unusable window rather than throwing", () => {
    const bad: PricingSchedule = { ...SCHEDULE, peak_windows_utc: [{ from: "1am", to: "04:00" }, { from: "25:00", to: "26:00" }] };
    expect(isPeakAt(bad, utc(2))).toBe(false);
  });

  it("handles a window that wraps midnight", () => {
    const wrapping: PricingSchedule = { ...SCHEDULE, peak_windows_utc: [{ from: "22:00", to: "02:00" }] };
    expect(isPeakAt(wrapping, utc(23))).toBe(true);
    expect(isPeakAt(wrapping, utc(1))).toBe(true);
    expect(isPeakAt(wrapping, utc(3))).toBe(false);
  });
});

describe("minutesUntilTierChange", () => {
  it("counts to the end of the window it is in", () => {
    expect(minutesUntilTierChange(SCHEDULE, utc(3, 20))).toBe(40);
  });

  it("counts to the start of the next one when off-peak", () => {
    expect(minutesUntilTierChange(SCHEDULE, utc(5, 0))).toBe(60);
  });

  it("wraps past midnight to the first window of the next day", () => {
    // 23:00 to the 01:00 window is two hours.
    expect(minutesUntilTierChange(SCHEDULE, utc(23, 0))).toBe(120);
  });

  it("does not return zero when sitting exactly on a boundary", () => {
    // 04:00 is the instant peak just ended; the next change is 06:00.
    expect(minutesUntilTierChange(SCHEDULE, utc(4, 0))).toBe(120);
  });

  it("is null with no schedule, and before it takes effect", () => {
    expect(minutesUntilTierChange(undefined, utc(2))).toBeNull();
    expect(minutesUntilTierChange(SCHEDULE, new Date(Date.UTC(2026, 7, 15, 2, 0)))).toBeNull();
  });
});

describe("localWindows", () => {
  // The tests run at a fixed TZ (see web/vite.config.ts test env), so the
  // absolute numbers here would be brittle. What is stable and worth pinning
  // is the shape: one entry per window, in order, with a crossed flag that
  // means what it says.
  it("returns one entry per usable window, in order", () => {
    const got = localWindows(SCHEDULE, new Date(Date.UTC(2026, 7, 20)));
    expect(got).toHaveLength(2);
    for (const w of got) {
      expect(w.from).toMatch(/^\d{2}:\d{2}$/);
      expect(w.to).toMatch(/^\d{2}:\d{2}$/);
    }
  });

  it("skips an unusable window instead of emitting a broken range", () => {
    const bad: PricingSchedule = { ...SCHEDULE, peak_windows_utc: [{ from: "01:00", to: "04:00" }, { from: "nope", to: "04:00" }] };
    expect(localWindows(bad, new Date(Date.UTC(2026, 7, 20)))).toHaveLength(1);
  });

  it("is empty with no schedule", () => {
    expect(localWindows(undefined)).toEqual([]);
  });

  it("preserves the window's own duration whatever the zone", () => {
    // Whatever offset the test environment has, 01:00-04:00 UTC is three
    // hours long where the reader is too. This is the check that catches a
    // conversion applied to one end and not the other.
    const [first] = localWindows(SCHEDULE, new Date(Date.UTC(2026, 7, 20)));
    const [fh, fm] = first.from.split(":").map(Number);
    const [th, tm] = first.to.split(":").map(Number);
    const span = ((th * 60 + tm - (fh * 60 + fm)) + 24 * 60) % (24 * 60);
    expect(span).toBe(180);
  });
});

describe("peakNote", () => {
  it("says nothing off-peak", () => {
    // A banner that speaks when everything is normal is a banner nobody
    // reads by the time it matters.
    expect(peakNote(SCHEDULE, utc(5))).toBe("");
    expect(peakNote(undefined, utc(2))).toBe("");
  });

  it("says nothing before the split takes effect", () => {
    expect(peakNote(SCHEDULE, new Date(Date.UTC(2026, 7, 15, 2, 0)))).toBe("");
  });

  it("names how long is left, which is the part worth acting on", () => {
    expect(peakNote(SCHEDULE, utc(3, 20))).toBe("peak rate for 40m");
    expect(peakNote(SCHEDULE, utc(2, 0))).toBe("peak rate for 2h");
    expect(peakNote(SCHEDULE, utc(1, 30))).toBe("peak rate for 2h 30m");
  });
});
