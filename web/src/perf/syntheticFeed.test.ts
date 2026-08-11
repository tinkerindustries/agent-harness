import { describe, expect, it } from "vitest";
import { FoldState } from "../api/fold";
import { buildSyntheticHistory, liveEventGenerator, makeSeqSource } from "./syntheticFeed";

describe("buildSyntheticHistory", () => {
  it("produces strictly increasing, unique seq numbers", () => {
    const seq = makeSeqSource();
    const events = buildSyntheticHistory(120, "s", seq);
    const seqs = events.map((e) => e.seq);
    expect(new Set(seqs).size).toBe(seqs.length);
    for (let i = 1; i < seqs.length; i++) expect(seqs[i]).toBeGreaterThan(seqs[i - 1]);
  });

  it("folds a plan the rail can render — TaskCreate speaks the fold's vocabulary and TaskUpdate advances it", () => {
    // Regression test: the feed's TaskCreate carried content where the fold
    // reads subject, so applyTaskEvent silently dropped the whole plan and
    // no plan rail (or timeline phase) ever rendered in the perf harness;
    // its TaskUpdate pairs named id where the fold reads taskId, so the plan
    // never advanced past the first boundary either. Folding the seeded
    // history must yield the seven plan items with the completed count
    // growing past the first boundary by turn 21.
    const seq = makeSeqSource();
    const events = buildSyntheticHistory(120, "s", seq);
    const state = new FoldState();
    for (const ev of events) state.ingest(ev);
    expect(state.latestTodos.length).toBe(7);
    const done = state.latestTodos.filter((t) => t.status === "completed").length;
    expect(done).toBeGreaterThan(0);
    const inProgress = state.latestTodos.findIndex((t) => t.status === "in_progress");
    expect(inProgress).toBeGreaterThan(0);
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
