import { describe, expect, it } from "vitest";
import { buildSyntheticHistory, liveEventGenerator, makeSeqSource } from "./syntheticFeed";

describe("buildSyntheticHistory", () => {
  it("produces strictly increasing, unique seq numbers", () => {
    const seq = makeSeqSource();
    const events = buildSyntheticHistory(120, "s", seq);
    const seqs = events.map((e) => e.seq);
    expect(new Set(seqs).size).toBe(seqs.length);
    for (let i = 1; i < seqs.length; i++) expect(seqs[i]).toBeGreaterThan(seqs[i - 1]);
  });

  it("continues a shared seq source rather than restarting it, so a following live feed cannot collide with the seeded history", () => {
    // Regression test: an earlier version of this harness gave the live
    // feed its own fresh seq source, producing duplicate React keys once
    // the two were rendered together (caught via a "duplicate key" console
    // error in web/src/perf/PerfHarnessScreen during manual testing).
    const seq = makeSeqSource();
    const history = buildSyntheticHistory(30, "s", seq);
    const lastHistorySeq = history[history.length - 1].seq;

    const gen = liveEventGenerator("s", seq);
    const liveSeqs = Array.from({ length: 20 }, () => gen.next().value.seq);

    expect(Math.min(...liveSeqs)).toBeGreaterThan(lastHistorySeq);
    const allSeqs = [...history.map((e) => e.seq), ...liveSeqs];
    expect(new Set(allSeqs).size).toBe(allSeqs.length);
  });
});
