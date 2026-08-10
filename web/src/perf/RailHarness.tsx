import { Profiler, useEffect, useRef, useState, useSyncExternalStore, type ProfilerOnRenderCallback } from "react";
import { flushSync } from "react-dom";
import { TranscriptStore, type TranscriptStoreOptions } from "../api/transcriptStore";
import { BlockList } from "../components/BlockList";
import { PlanPanel } from "../components/PlanPanel";
import { TimelineRail } from "../components/TimelineRail";
import { TranscriptToolbar } from "../components/TranscriptToolbar";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import type { Density } from "../components/blocks/SubTurnCard";
import type { TranscriptFilter } from "../api/groups";
import { computeFrameStats, type FrameStats } from "./frameStats";
import { buildSyntheticHistory, liveEventGenerator, makeSeqSource, type SeqSource } from "./syntheticFeed";

// RailHarness is the phase 6 exit measurement (docs/WEB-REDESIGN.md): mount
// the transcript screen with its timeline rail against the synthetic
// 142-sub-turn session, then answer the two questions the phase has to
// answer with numbers rather than arguments —
//
//   - the rail renders one entry per sub-turn (142), grouped under the plan
//     items the synthetic feed's TodoWrite calls mark, with exactly one
//     IntersectionObserver watching the cards;
//   - scrolling the whole transcript updates the current marker without a
//     measurable frame cost: every marker update is a React commit the
//     Profiler times, and frame-to-frame time is recorded as rAF deltas
//     (meaningful when the page is open in a real, focused tab, exactly as
//     the render-cost harness documents).
//
// The observer count is not asserted by reading the code: the page replaces
// window.IntersectionObserver with a counting subclass at module load —
// before any component mounts — so whatever the screen constructs during the
// run is what gets reported. The rail is the only component that constructs
// one, so a session that mounts the rail exactly once reports 1. If the
// constraint is ever broken (an observer mounted inside each sub-turn card),
// this number says so.
//
// Configured by ?blocks=N; the default 391 blocks is the opening block plus
// 142 sub-turns of the standard synthetic cycle, the measured session's
// size. Nothing here talks to the network.

const DEFAULT_BLOCKS = 391;

const railObserverCount = { value: 0 };
const NativeIntersectionObserver = window.IntersectionObserver;
window.IntersectionObserver = class RailCountingIntersectionObserver extends NativeIntersectionObserver {
  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    super(callback, options);
    railObserverCount.value++;
  }
} as typeof IntersectionObserver;

function microtaskScheduler(): TranscriptStoreOptions {
  return {
    scheduleFlush: (cb) => {
      queueMicrotask(() => flushSync(cb));
      return 0;
    },
    cancelFlush: () => {},
  };
}

async function nextFrames(n: number): Promise<void> {
  for (let i = 0; i < n; i++) await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

async function yieldMicrotask(): Promise<void> {
  await new Promise<void>((resolve) => queueMicrotask(resolve));
}

function mean(xs: number[]): number {
  return xs.length === 0 ? 0 : xs.reduce((a, b) => a + b, 0) / xs.length;
}

interface RailMeasure {
  entries: number;
  phases: number;
  observers: number;
  subTurns: number;
  scrollHeight: number;
  markerUpdates: number;
  scrollCommitCount: number;
  scrollCommitMeanMs: number;
  scrollCommitMaxMs: number;
  frameStats: FrameStats;
  liveAppendCount: number;
  liveAppendMeanMs: number;
  liveAppendMaxMs: number;
}

type MeasurePhase = "scroll" | "live" | null;

export function RailHarness() {
  const params = new URL(window.location.href).searchParams;
  const blocks = Number(params.get("blocks") ?? DEFAULT_BLOCKS);
  const [store] = useState(() => new TranscriptStore("perf-rail", { connect: false, ...microtaskScheduler() }));
  const [seeded, setSeeded] = useState(false);
  const [subTurns, setSubTurns] = useState(0);
  // StrictMode double-invokes effects in dev; the guard makes seeding
  // idempotent so the fold is not fed the history twice.
  const seededRef = useRef(false);
  // The seq source is shared with the live burst at the end of the
  // measurement — the same continuity invariant the render-cost harness
  // relies on, so live keys never collide with history keys.
  const seqRef = useRef<SeqSource | null>(null);

  useEffect(() => {
    if (seededRef.current) return;
    seededRef.current = true;
    const seq = makeSeqSource();
    seqRef.current = seq;
    const events = buildSyntheticHistory(blocks, "perf-rail", seq);
    for (const ev of events) store.ingest(ev);
    setSubTurns(events.filter((e) => e.kind === "turn_finished").length);
    setSeeded(true);
  }, [store, blocks]);

  return <RailMount store={store} seeded={seeded} subTurns={subTurns} seqRef={seqRef} />;
}

function RailMount({
  store,
  seeded,
  subTurns,
  seqRef,
}: {
  store: TranscriptStore;
  seeded: boolean;
  subTurns: number;
  seqRef: React.RefObject<SeqSource | null>;
}) {
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const [density, setDensity] = useState<Density>("compact");
  const [filter, setFilter] = useState<TranscriptFilter>("all");
  const containerRef = useRef<HTMLDivElement>(null);
  const [measure, setMeasure] = useState<RailMeasure | null>(null);
  const runningRef = useRef(false);
  const phaseRef = useRef<MeasurePhase>(null);
  const samplesRef = useRef<{ scroll: number[]; live: number[] }>({ scroll: [], live: [] });

  const onRender: ProfilerOnRenderCallback = (_id, _phase, actualDuration) => {
    const p = phaseRef.current;
    if (!p) return;
    samplesRef.current[p].push(actualDuration);
  };

  useEffect(() => {
    if (!seeded || snapshot.items.length === 0 || runningRef.current) return;
    runningRef.current = true;
    let cancelled = false;
    (async () => {
      // Let the browser actually paint the seeded transcript before
      // measuring anything against the viewport.
      await nextFrames(3);
      if (cancelled) return;

      // The rail's accordion mounts while the store is still empty, so its
      // default (first phase open) never applies and every phase renders
      // closed. Radix unmounts a closed phase's rows, so the .rail-turn
      // count below would read 0 while the rail holds all 142 entries —
      // a reporting bug in this harness, not in the rail. Open every closed
      // phase before counting so the DOM count is the real one.
      for (const trigger of document.querySelectorAll<HTMLElement>(".rail-phase-trigger[data-state=\"closed\"]")) {
        trigger.click();
      }
      await nextFrames(2);
      if (cancelled) return;

      const railNav = document.querySelector<HTMLElement>(".timeline-rail");
      const entries = document.querySelectorAll(".rail-turn").length;
      const phases = document.querySelectorAll(".rail-phase").length;
      const scrollHeight = document.documentElement.scrollHeight;

      // Frame deltas around the scroll sweep: the number that answers "did
      // this hold frame rate" while the marker is moving.
      const frameDeltas: number[] = [];
      let last = performance.now();
      let rafId = 0;
      const frameLoop = () => {
        const now = performance.now();
        frameDeltas.push(now - last);
        last = now;
        rafId = requestAnimationFrame(frameLoop);
      };
      rafId = requestAnimationFrame(frameLoop);

      // Sweep the page top to bottom in viewport-sized steps, waiting two
      // frames between steps so the observer can fire and React can commit
      // the marker move. Marker updates are counted off the rail's own
      // data-current-seq.
      let markerUpdates = 0;
      let lastMarker = railNav?.dataset.currentSeq ?? "";
      phaseRef.current = "scroll";
      const maxY = Math.max(0, scrollHeight - window.innerHeight);
      const step = Math.max(240, Math.round(window.innerHeight * 0.6));
      for (let y = 0; y <= maxY; y += step) {
        window.scrollTo(0, y);
        await nextFrames(2);
        if (cancelled) break;
        const marker = railNav?.dataset.currentSeq ?? "";
        if (marker !== lastMarker) {
          markerUpdates++;
          lastMarker = marker;
        }
      }
      window.scrollTo(0, maxY);
      await nextFrames(2);
      window.scrollTo(0, 0);
      await nextFrames(2);
      phaseRef.current = null;
      cancelAnimationFrame(rafId);

      const scrollSamples = samplesRef.current.scroll;
      const frameStats = computeFrameStats(frameDeltas.length > 1 ? frameDeltas.slice(1) : frameDeltas);

      // Live append measurement: push a few synthetic sub-turns through the
      // same store and record the append commits — the rail's per-append
      // render cost at this entry count, which is the one cost the rail adds
      // to a running session.
      const gen = liveEventGenerator("perf-rail", seqRef.current ?? makeSeqSource());
      phaseRef.current = "live";
      for (let batch = 0; batch < 4 && !cancelled; batch++) {
        for (let i = 0; i < 6; i++) store.ingest(gen.next().value);
        await yieldMicrotask();
      }
      phaseRef.current = null;

      const liveSamples = samplesRef.current.live;
      setMeasure({
        entries,
        phases,
        observers: railObserverCount.value,
        subTurns,
        scrollHeight,
        markerUpdates,
        scrollCommitCount: scrollSamples.length,
        scrollCommitMeanMs: mean(scrollSamples),
        scrollCommitMaxMs: scrollSamples.length ? Math.max(...scrollSamples) : 0,
        frameStats,
        liveAppendCount: liveSamples.length,
        liveAppendMeanMs: mean(liveSamples),
        liveAppendMaxMs: liveSamples.length ? Math.max(...liveSamples) : 0,
      });
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [seeded, snapshot.items.length]);

  return (
    <div className="screen screen-transcript">
      <header className="screen-header">
        <Button variant="outline" size="sm">
          ← sessions
        </Button>
        <h1>perf-rail</h1>
        <Badge variant="outline" className="connection-badge connection-closed">
          closed
        </Badge>
      </header>
      <div className="session-meta">
        <Badge variant="done">DONE</Badge>
        <span className="dim">synthetic feed</span>
        <span className="dim">compact mode</span>
      </div>
      <p className="perf-height-result" data-testid="rail-result" data-measure={measure ? JSON.stringify(measure) : ""}>
        {measure ? (
          <>
            Rail: <strong>{measure.entries} entries</strong> · {measure.phases} phases ·{" "}
            <strong>{measure.observers} IntersectionObserver</strong> · compact scroll height{" "}
            {measure.scrollHeight.toLocaleString("en-US")} px · {measure.subTurns} sub-turns
            <br />
            Scroll: {measure.markerUpdates} marker updates · {measure.scrollCommitCount} commits · mean{" "}
            {measure.scrollCommitMeanMs.toFixed(3)} ms · max {measure.scrollCommitMaxMs.toFixed(3)} ms · frames{" "}
            {measure.frameStats.frames} · frame mean {measure.frameStats.meanMs.toFixed(2)} ms · frame max{" "}
            {measure.frameStats.maxMs.toFixed(2)} ms · &gt;16.7ms {measure.frameStats.over16Count}
            <br />
            Live append: {measure.liveAppendCount} commits · mean {measure.liveAppendMeanMs.toFixed(3)} ms · max{" "}
            {measure.liveAppendMaxMs.toFixed(3)} ms
          </>
        ) : (
          <>seeding {subTurns || "…"} sub-turns…</>
        )}
      </p>
      <TranscriptToolbar
        density={density}
        onDensityChange={setDensity}
        filter={filter}
        onFilterChange={setFilter}
        counts={snapshot.counts}
      />
      <Profiler id="rail-measure" onRender={onRender}>
        <div className="transcript-layout">
          <TimelineRail
            items={snapshot.items}
            getToolCall={snapshot.getToolCall}
            filter={filter}
            containerRef={containerRef}
          />
          <div ref={containerRef} className="transcript-col">
            <BlockList
              items={snapshot.items}
              live={snapshot.live}
              density={density}
              filter={filter}
              getToolCall={snapshot.getToolCall}
            />
          </div>
          <PlanPanel todos={snapshot.todos} />
        </div>
      </Profiler>
    </div>
  );
}
