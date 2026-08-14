import { describe, expect, it } from "vitest";
import {
  BUCKETS,
  BUCKET_MS,
  DurationStats,
  MIN_BASELINE_MS,
  MIN_SAMPLES,
  PULSE_NORMAL_MS,
  PULSE_STALLED_MS,
  PulseMeter,
  STALL_FACTOR_MAX,
  median,
  pulsePeriodMs,
  stallFactor,
} from "./pulse";
import type { StoreEvent } from "./types";

// The two halves of the liveness layer. Neither is a field the server sends,
// so the only thing standing between "the dot means something" and "the dot
// is decoration" is that these hold.

const T0 = Date.parse("2026-08-14T00:00:00Z");

function ev(kind: StoreEvent["kind"], payload: unknown, atMs = T0): StoreEvent {
  return { session_id: "sess-1", seq: 1, kind, payload, created_at: new Date(atMs).toISOString() };
}

// copy, because a PulseFrame's arrays are the meter's own storage and are
// reused on the next read (see PulseFrame).
function snap(m: PulseMeter, atMs: number) {
  const f = m.read(atMs);
  return {
    reasoning: [...f.reasoning],
    content: [...f.content],
    stdout: [...f.stdout],
    tools: [...f.tools],
    errors: [...f.errors],
  };
}

describe("PulseMeter", () => {
  it("starts empty and reports so", () => {
    const m = new PulseMeter();
    expect(m.hasData).toBe(false);
    const f = snap(m, T0);
    expect(f.reasoning).toHaveLength(BUCKETS);
    expect(f.reasoning.every((n) => n === 0)).toBe(true);
  });

  it("lands text in the newest bucket, which is the last one read", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 40);
    m.text(T0 + 100, "reasoning", 7);
    const f = snap(m, T0 + 100);
    expect(m.hasData).toBe(true);
    expect(f.content[BUCKETS - 1]).toBe(40);
    expect(f.reasoning[BUCKETS - 1]).toBe(7);
    // Nothing bled into any other bucket.
    expect(f.content.slice(0, -1).every((n) => n === 0)).toBe(true);
  });

  it("separates the three bands", () => {
    const m = new PulseMeter();
    m.text(T0, "reasoning", 1);
    m.text(T0, "content", 2);
    m.text(T0, "stdout", 3);
    const f = snap(m, T0);
    expect([f.reasoning[BUCKETS - 1], f.content[BUCKETS - 1], f.stdout[BUCKETS - 1]]).toEqual([1, 2, 3]);
  });

  it("scrolls older text left as time passes", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 10);
    // Four buckets later the first write is four bars from the right.
    const f = snap(m, T0 + 4 * BUCKET_MS);
    expect(f.content[BUCKETS - 1]).toBe(0);
    expect(f.content[BUCKETS - 5]).toBe(10);
  });

  it("clears the buckets it scrolls through, so silence is silent", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 10);
    // Past the whole window: the write is gone, not stuck at the edge.
    const f = snap(m, T0 + (BUCKETS + 10) * BUCKET_MS);
    expect(f.content.every((n) => n === 0)).toBe(true);
  });

  it("a write within the same bucket accumulates rather than replacing", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 10);
    m.text(T0 + BUCKET_MS - 1, "content", 5);
    expect(snap(m, T0).content[BUCKETS - 1]).toBe(15);
  });

  it("ignores an empty or negative character count", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 0);
    expect(m.hasData).toBe(false);
  });

  it("does not drift: bar N is always N buckets back from now", () => {
    const m = new PulseMeter();
    // Write on a deliberately off-grid cadence — 137ms is not a divisor of
    // BUCKET_MS — and check the marks still land where the elapsed time says.
    m.text(T0 + 137, "content", 1);
    m.text(T0 + 137 + 3 * BUCKET_MS, "content", 2);
    const f = snap(m, T0 + 137 + 3 * BUCKET_MS);
    expect(f.content[BUCKETS - 1]).toBe(2);
    expect(f.content[BUCKETS - 4]).toBe(1);
  });

  it("counts tool starts and errors as marks, not as a band", () => {
    const m = new PulseMeter();
    m.feedEvent(T0, ev("tool_call", { index: 0, id: "c1", name: "Bash", arguments: "{}" }));
    m.feedEvent(T0, ev("tool_result", { tool_call_id: "c1", name: "Bash", content: "boom", is_error: true }));
    const f = snap(m, T0);
    expect(f.tools[BUCKETS - 1]).toBe(1);
    expect(f.errors[BUCKETS - 1]).toBe(1);
    expect(f.content[BUCKETS - 1]).toBe(0);
  });

  it("a successful tool result is not an error mark", () => {
    const m = new PulseMeter();
    m.feedEvent(T0, ev("tool_result", { tool_call_id: "c1", name: "Read", content: "ok" }));
    expect(snap(m, T0).errors[BUCKETS - 1]).toBe(0);
  });

  it("routes tool_stdout to the stdout band", () => {
    const m = new PulseMeter();
    m.feedEvent(T0, ev("tool_stdout", { tool_call_id: "c1", text: "12345" }));
    expect(snap(m, T0).stdout[BUCKETS - 1]).toBe(5);
  });

  it("routes live frames by channel", () => {
    const m = new PulseMeter();
    m.feedLive(T0, { sub_turn: 1, channel: "reasoning", text: "abc" });
    m.feedLive(T0, { sub_turn: 1, channel: "content", text: "de" });
    const f = snap(m, T0);
    expect([f.reasoning[BUCKETS - 1], f.content[BUCKETS - 1]]).toEqual([3, 2]);
  });

  it("ignores the committed delta kinds, which would double every sub-turn", () => {
    const m = new PulseMeter();
    m.feedEvent(T0, ev("content_delta", { text: "a long committed batch" }));
    m.feedEvent(T0, ev("reasoning_delta", { text: "and its reasoning" }));
    expect(m.hasData).toBe(false);
  });

  it("bumps its revision on a write and on the window scrolling, and not otherwise", () => {
    const m = new PulseMeter();
    const start = m.revision;
    m.text(T0, "content", 5);
    const afterWrite = m.revision;
    expect(afterWrite).toBeGreaterThan(start);

    // A read inside the same bucket changes nothing, so the strip has
    // nothing to redraw.
    m.read(T0 + 10);
    expect(m.revision).toBe(afterWrite);

    // A read past the bucket boundary scrolls the window, which the strip
    // does have to redraw — this is the case a revision check alone would
    // miss, and why the drawing pass also watches the clock.
    m.read(T0 + BUCKET_MS + 1);
    expect(m.revision).toBeGreaterThan(afterWrite);
  });

  it("survives a gap far longer than the window in bounded work", () => {
    const m = new PulseMeter();
    m.text(T0, "content", 1);
    // An hour in a backgrounded tab. The cap is what keeps this from
    // spinning 7,200 iterations on the frame the tab comes back.
    const f = snap(m, T0 + 3_600_000);
    expect(f.content.every((n) => n === 0)).toBe(true);
  });
});

describe("pulsePeriodMs", () => {
  it("rests when there is nothing to compare against", () => {
    expect(pulsePeriodMs(null)).toBe(PULSE_NORMAL_MS);
  });

  it("rests at or below the baseline", () => {
    expect(pulsePeriodMs(0.2)).toBe(PULSE_NORMAL_MS);
    expect(pulsePeriodMs(1)).toBe(PULSE_NORMAL_MS);
  });

  it("reaches its slowest at the cap and never goes past it", () => {
    expect(pulsePeriodMs(STALL_FACTOR_MAX)).toBe(PULSE_STALLED_MS);
    expect(pulsePeriodMs(40)).toBe(PULSE_STALLED_MS);
  });

  it("interpolates in between, monotonically", () => {
    const mid = pulsePeriodMs((1 + STALL_FACTOR_MAX) / 2);
    expect(mid).toBeGreaterThan(PULSE_NORMAL_MS);
    expect(mid).toBeLessThan(PULSE_STALLED_MS);
    expect(pulsePeriodMs(2)).toBeLessThan(pulsePeriodMs(3));
  });
});

describe("stallFactor", () => {
  it("is null with no activity or no baseline", () => {
    expect(stallFactor(null, 5000)).toBeNull();
    expect(stallFactor(5000, null)).toBeNull();
  });

  it("is the ratio of age to baseline", () => {
    expect(stallFactor(10_000, 5000)).toBe(2);
  });

  it("floors the baseline, so a fast session does not cry stall at one second", () => {
    // A session whose median tool call is 40ms: a one-second call is 25x the
    // median but nowhere near slow.
    expect(stallFactor(1000, 40)).toBe(1000 / MIN_BASELINE_MS);
    expect(pulsePeriodMs(stallFactor(1000, 40))).toBe(PULSE_NORMAL_MS);
  });
});

describe("median", () => {
  it("withholds a figure below the sample floor", () => {
    expect(median([])).toBeNull();
    expect(median(Array(MIN_SAMPLES - 1).fill(10))).toBeNull();
  });

  it("is the middle of an odd sample and the mean of the middle two of an even one", () => {
    expect(median([5, 1, 3])).toBe(3);
    expect(median([1, 3, 5, 7])).toBe(4);
  });

  it("does not mutate its input", () => {
    const samples = [5, 1, 3];
    median(samples);
    expect(samples).toEqual([5, 1, 3]);
  });
});

describe("DurationStats", () => {
  function toolPair(stats: DurationStats, id: string, startMs: number, durationMs: number) {
    stats.feedEvent(ev("tool_call", { index: 0, id, name: "Bash", arguments: "{}" }, startMs));
    stats.feedEvent(ev("tool_result", { tool_call_id: id, name: "Bash", content: "" }, startMs + durationMs));
  }

  it("has no baseline until the sample floor is met", () => {
    const s = new DurationStats();
    toolPair(s, "c1", T0, 1000);
    toolPair(s, "c2", T0, 3000);
    expect(s.toolBaselineMs()).toBeNull();
    toolPair(s, "c3", T0, 2000);
    expect(s.toolBaselineMs()).toBe(2000);
  });

  it("measures a tool call from its call event to its result", () => {
    const s = new DurationStats();
    toolPair(s, "c1", T0, 4000);
    toolPair(s, "c2", T0 + 10_000, 6000);
    toolPair(s, "c3", T0 + 30_000, 5000);
    expect(s.toolBaselineMs()).toBe(5000);
  });

  it("counts a denial as a completed call", () => {
    const s = new DurationStats();
    for (const [i, ms] of [1000, 2000, 3000].entries()) {
      const id = `d${i}`;
      s.feedEvent(ev("tool_call", { index: 0, id, name: "Bash", arguments: "{}" }, T0));
      s.feedEvent(ev("tool_denied", { tool_call_id: id, name: "Bash", rule: "r", content: "" }, T0 + ms));
    }
    expect(s.toolBaselineMs()).toBe(2000);
  });

  it("ignores a result with no matching call, and a call that never returns", () => {
    const s = new DurationStats();
    s.feedEvent(ev("tool_result", { tool_call_id: "ghost", name: "Bash", content: "" }, T0));
    s.feedEvent(ev("tool_call", { index: 0, id: "hung", name: "Bash", arguments: "{}" }, T0));
    expect(s.toolBaselineMs()).toBeNull();
  });

  it("ignores a backwards clock rather than dragging the median to zero", () => {
    const s = new DurationStats();
    s.feedEvent(ev("tool_call", { index: 0, id: "c1", name: "Bash", arguments: "{}" }, T0 + 5000));
    s.feedEvent(ev("tool_result", { tool_call_id: "c1", name: "Bash", content: "" }, T0));
    expect(s.toolBaselineMs()).toBeNull();
  });

  it("takes the sub-turn baseline from turn_finished's own elapsed_ms", () => {
    const s = new DurationStats();
    for (const ms of [8000, 12_000, 10_000]) {
      s.feedEvent(ev("turn_finished", { sub_turn: 1, finish_reason: "stop", elapsed_ms: ms }));
    }
    expect(s.turnBaselineMs()).toBe(10_000);
  });

  it("skips a turn_finished with no elapsed_ms, which is a pre-migration row", () => {
    const s = new DurationStats();
    for (let i = 0; i < 5; i++) s.feedEvent(ev("turn_finished", { sub_turn: i, finish_reason: "stop" }));
    expect(s.turnBaselineMs()).toBeNull();
  });

  it("tracks a window, so early work stops being the standard", () => {
    const s = new DurationStats();
    // 60 quick calls, then 60 slow ones: the median follows the run rather
    // than averaging its whole history.
    for (let i = 0; i < 60; i++) toolPair(s, `q${i}`, T0, 1000);
    expect(s.toolBaselineMs()).toBe(1000);
    for (let i = 0; i < 60; i++) toolPair(s, `s${i}`, T0, 9000);
    expect(s.toolBaselineMs()).toBe(9000);
  });
});
