import { Profiler, useEffect, useRef, useState, useSyncExternalStore, type ProfilerOnRenderCallback } from "react";
import { flushSync } from "react-dom";
import { TranscriptStore, type TranscriptStoreOptions } from "../api/transcriptStore";
import { TurnTranscript } from "../components/turns/TurnTranscript";
import { computeFrameStats, type FrameStats } from "./frameStats";
import { buildSyntheticHistory, liveEventGenerator, makeSeqSource } from "./syntheticFeed";
import { HeightHarness } from "./HeightHarness";
import { RailHarness } from "./RailHarness";

// PerfHarnessScreen is the instrument behind docs/DESIGN.md §5.5, not a
// nicety: a synthetic delta feed at a fixed rate against a mounted
// transcript, with render cost recorded, answering the virtualisation
// question with a number. §5.5 records what it found; the short version is
// in the page's own header text below.
//
// Two measurements run side by side:
//
//   - React's own Profiler, which times every commit's render work
//     (actualDuration) regardless of whether the tab is foregrounded. This
//     is what answers "does an update's cost scale with block count" — the
//     question that decides virtualisation — and it is what this harness
//     leans on, because a real requestAnimationFrame cadence needs a
//     visible, focused tab and automated runs do not reliably have one
//     (Chrome throttles rAF and setTimeout in a backgrounded tab).
//   - requestAnimationFrame deltas, the actual frame-to-frame time a user
//     would experience. Trustworthy when this page is open in a real,
//     focused tab; near-meaningless (and usually near-zero samples) when
//     it is not, because the browser simply is not scheduling frames.
//
// Query params:
//   ?blocks=400&rate=60&duration=6                    one run
//   ?sweep=50,200,500,1000,2000&rate=60&duration=6     one run per block
//                                                       count — the sweep is
//                                                       what shows whether
//                                                       cost scales with N.
//   &fast=1                                            drive the store's
//                                                       flush with a
//                                                       microtask scheduler
//                                                       instead of rAF, and
//                                                       pace the feed by
//                                                       event count instead
//                                                       of a wall-clock
//                                                       sleep. Same fold and
//                                                       commit code path;
//                                                       just not gated on a
//                                                       browser tab actually
//                                                       being scheduled
//                                                       frames, which an
//                                                       automated/backgrounded
//                                                       run cannot rely on.
// No params runs the sweep with sensible defaults.

interface RunConfig {
  blocks: number;
  ratePerSec: number;
  durationMs: number;
  fast: boolean;
}

interface RunResult extends FrameStats {
  blocks: number;
  ratePerSec: number;
  durationMs: number;
  mountMs: number;
  mountCommitMeanMs: number;
  // Delta commits are the common case docs/DESIGN.md §5.1 cares about: a
  // reasoning/content delta or a tool_stdout chunk, with the frozen blocks
  // array unchanged. Append commits are the ones where a new block just
  // froze — turn_finished, tool_result, and so on — which forces React to
  // re-run FrozenBlocks' map and reconcile every list child's key, an O(n)
  // cost no per-block memoisation removes. Splitting them is what makes the
  // sweep answer "does the hot path scale with N" instead of a blended
  // number that looks like it does only because appends do.
  liveDeltaCommits: number;
  liveDeltaMeanMs: number;
  liveDeltaP95Ms: number;
  liveDeltaMaxMs: number;
  liveAppendCommits: number;
  liveAppendMeanMs: number;
  liveAppendP95Ms: number;
  liveAppendMaxMs: number;
}

declare global {
  interface Window {
    __perfResults?: RunResult[];
  }
}

const DEFAULT_SWEEP = [50, 200, 500, 1000, 2000];

function parseParams(): RunConfig[] {
  const url = new URL(window.location.href);
  const rate = Number(url.searchParams.get("rate") ?? 60);
  const duration = Number(url.searchParams.get("duration") ?? 6) * 1000;
  const fast = url.searchParams.get("fast") === "1";
  const sweep = url.searchParams.get("sweep");
  const single = url.searchParams.get("blocks");

  if (single) return [{ blocks: Number(single), ratePerSec: rate, durationMs: duration, fast }];

  const blockCounts = sweep
    ? sweep
        .split(",")
        .map((s) => Number(s.trim()))
        .filter((n) => n > 0)
    : DEFAULT_SWEEP;
  return blockCounts.map((blocks) => ({ blocks, ratePerSec: rate, durationMs: duration, fast }));
}

function microtaskScheduler(): TranscriptStoreOptions {
  return {
    // flushSync forces React to commit this update synchronously and alone
    // rather than batching it with whatever else happens to land in the
    // same task — otherwise React 18's automatic batching coalesces every
    // update from a chain of queueMicrotask callbacks into a single commit
    // at the very end, since none of them cross a real task boundary. That
    // defeats the point of the fast path, which needs one commit per flush
    // so the Profiler has individual samples to report.
    scheduleFlush: (cb) => {
      queueMicrotask(() => flushSync(cb));
      return 0;
    },
    cancelFlush: () => {},
  };
}

async function yieldMicrotask(): Promise<void> {
  await new Promise<void>((resolve) => queueMicrotask(resolve));
}

async function nextTwoFrames(fast: boolean): Promise<void> {
  if (fast) {
    await yieldMicrotask();
    await yieldMicrotask();
    return;
  }
  await new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
}

function mean(xs: number[]): number {
  return xs.length === 0 ? 0 : xs.reduce((a, b) => a + b, 0) / xs.length;
}

function percentile(xs: number[], p: number): number {
  if (xs.length === 0) return 0;
  const sorted = [...xs].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))];
}

// PhaseRef tags which part of a run a Profiler commit belongs to, so mount
// commits (a one-time bulk cost) and live-update commits (the steady-state
// cost that matters for holding frame rate) get reported separately.
type Phase = "mount" | "live" | null;

interface Sample {
  phase: Exclude<Phase, null>;
  ms: number;
  appended: boolean;
}

interface RunHooks {
  onStore: (s: TranscriptStore) => void;
  setPhase: (p: Phase) => void;
  takeSamples: (phase: Exclude<Phase, null>) => Sample[];
}

async function runOne(cfg: RunConfig, hooks: RunHooks): Promise<RunResult> {
  const mountStart = performance.now();
  const store = new TranscriptStore("perf", { connect: false, ...(cfg.fast ? microtaskScheduler() : {}) });
  // In fast mode, mount PerfMount synchronously before any events are
  // ingested, so it is already subscribed when the first flush happens —
  // otherwise the mount commit itself (a React state update from outside an
  // event handler) can land after several ingests have already flushed to
  // no listeners, which is harmless for correctness (getSnapshot always
  // returns the latest fold) but starves the Profiler of the individual
  // commits this harness is trying to measure.
  if (cfg.fast) flushSync(() => hooks.onStore(store));
  else hooks.onStore(store);

  hooks.setPhase("mount");
  const seq = makeSeqSource();
  const history = buildSyntheticHistory(cfg.blocks, "perf", seq);
  for (const ev of history) store.ingest(ev);
  await nextTwoFrames(cfg.fast); // let React actually paint every block before measuring
  const mountMs = performance.now() - mountStart;
  const mountCommitMeanMs = mean(hooks.takeSamples("mount").map((s) => s.ms));

  // requestAnimationFrame deltas: real frame-to-frame time, meaningful only
  // when this tab is genuinely visible and focused (see the header comment).
  const deltas: number[] = [];
  let last = performance.now();
  let rafId = 0;
  const frameLoop = () => {
    const now = performance.now();
    deltas.push(now - last);
    last = now;
    rafId = requestAnimationFrame(frameLoop);
  };
  rafId = requestAnimationFrame(frameLoop);

  hooks.setPhase("live");
  const gen = liveEventGenerator("perf", seq);
  const targetEvents = Math.round((cfg.durationMs / 1000) * cfg.ratePerSec);

  if (cfg.fast) {
    // No wall-clock pacing at all: inject in small batches, yielding a
    // microtask between each so the store's own flush actually runs and
    // React commits between batches, rather than one batch's ingests all
    // landing before the coalesced flush fires. This is what makes the
    // sweep finish in milliseconds instead of minutes when the tab cannot
    // be scheduled frames — see the module comment.
    const batchSize = 5;
    for (let sent = 0; sent < targetEvents; sent += batchSize) {
      for (let i = 0; i < batchSize && sent + i < targetEvents; i++) store.ingest(gen.next().value);
      await yieldMicrotask();
    }
  } else {
    // The delta feed is paced against wall-clock elapsed time rather than
    // one fixed-size step per timer callback, so a throttled setTimeout
    // (background tab) still delivers roughly rate*duration events in
    // total — caught up in a burst rather than dropped — instead of
    // silently under-running.
    const start = performance.now();
    let delivered = 0;
    // eslint-disable-next-line no-constant-condition
    while (true) {
      const elapsed = performance.now() - start;
      if (elapsed >= cfg.durationMs) break;
      const due = Math.floor((elapsed / 1000) * cfg.ratePerSec);
      while (delivered < due) {
        store.ingest(gen.next().value);
        delivered++;
      }
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
  }
  cancelAnimationFrame(rafId);
  hooks.setPhase(null);

  const liveSamples = hooks.takeSamples("live");
  const deltaMs = liveSamples.filter((s) => !s.appended).map((s) => s.ms);
  const appendMs = liveSamples.filter((s) => s.appended).map((s) => s.ms);
  // The first frame delta measures time since the setup above, not
  // steady-state rendering; drop it when there is more than one sample.
  const frameStats = computeFrameStats(deltas.length > 1 ? deltas.slice(1) : deltas);

  return {
    ...frameStats,
    blocks: cfg.blocks,
    ratePerSec: cfg.ratePerSec,
    durationMs: cfg.durationMs,
    mountMs,
    mountCommitMeanMs,
    liveDeltaCommits: deltaMs.length,
    liveDeltaMeanMs: mean(deltaMs),
    liveDeltaP95Ms: percentile(deltaMs, 95),
    liveDeltaMaxMs: deltaMs.length ? Math.max(...deltaMs) : 0,
    liveAppendCommits: appendMs.length,
    liveAppendMeanMs: mean(appendMs),
    liveAppendP95Ms: percentile(appendMs, 95),
    liveAppendMaxMs: appendMs.length ? Math.max(...appendMs) : 0,
  };
}

export function PerfHarnessScreen() {
  // ?height=1 swaps the render-cost sweep for the page-height measurement:
  // the transcript screen in Compact mode against a synthetic session, with
  // its scroll height reported (web/src/perf/HeightHarness.tsx). ?rail=1
  // swaps it for the rail measurement: the same screen with the
  // timeline rail, scrolling through it to count marker updates and frame
  // cost (web/src/perf/RailHarness.tsx). Read before any hooks because the
  // modes share no state.
  if (new URL(window.location.href).searchParams.get("height") === "1") return <HeightHarness />;
  if (new URL(window.location.href).searchParams.get("rail") === "1") return <RailHarness />;

  const [status, setStatus] = useState("idle");
  const [results, setResults] = useState<RunResult[]>([]);
  const [store, setStore] = useState<TranscriptStore | null>(null);
  const runningRef = useRef(false);

  const phaseRef = useRef<Phase>(null);
  const samplesRef = useRef<Sample[]>([]);
  const lastBlocksLenRef = useRef(0);

  const handleRender: ProfilerOnRenderCallback = (_id, _reactPhase, actualDuration) => {
    const phase = phaseRef.current;
    if (!phase) return;
    // A commit "appended" a block when the frozen blocks array grew since
    // the last commit — the case that costs React an O(n) reconciliation
    // pass over FrozenBlocks' children regardless of memoisation (see
    // RunResult's comment). store is read fresh via closure, not a stale
    // capture, since handleRender is recreated on every render.
    const blocksLen = store?.getSnapshot().blocks.length ?? 0;
    const appended = blocksLen !== lastBlocksLenRef.current;
    lastBlocksLenRef.current = blocksLen;
    samplesRef.current.push({ phase, ms: actualDuration, appended });
  };

  const hooks: RunHooks = {
    onStore: setStore,
    setPhase: (p) => {
      phaseRef.current = p;
    },
    takeSamples: (phase) => {
      const out = samplesRef.current.filter((s) => s.phase === phase);
      samplesRef.current = samplesRef.current.filter((s) => s.phase !== phase);
      return out;
    },
  };

  useEffect(() => {
    if (runningRef.current) return;
    runningRef.current = true;
    const configs = parseParams();
    (async () => {
      const out: RunResult[] = [];
      for (const cfg of configs) {
        setStatus(`seeding ${cfg.blocks} blocks…`);
        const r = await runOne(cfg, hooks);
        out.push(r);
        setResults([...out]);
        setStatus(`measured ${cfg.blocks} blocks (${out.length}/${configs.length})`);
      }
      setStatus("done");
      window.__perfResults = out;
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="screen perf-harness">
      <header className="screen-header">
        <h1>Performance harness</h1>
      </header>
      <p className="dim">
        Synthetic delta feed at a fixed rate against N mounted blocks. "delta" commits are a reasoning/content/
        tool_stdout update with the frozen list unchanged — the token-rate hot path docs/DESIGN.md §5.1 names.
        "append" commits are the ones that just froze a new block, which costs React an O(n) reconciliation pass
        over the list regardless of memoisation. Both are React Profiler's actualDuration, reliable with or without
        a focused tab. "frame ms" is real requestAnimationFrame deltas — only meaningful when this tab is visible
        and focused. Configure with <code>?blocks=400&amp;rate=60&amp;duration=6</code> or{" "}
        <code>?sweep=50,200,500,1000,2000&amp;rate=60&amp;duration=6</code>; no params runs the default sweep. Add{" "}
        <code>&amp;fast=1</code> to bypass rAF-gated flushing when the tab cannot be scheduled frames.
      </p>
      <p data-testid="perf-status">
        status: <strong>{status}</strong>
      </p>
      <div className="table-scroll">
        <table className="session-table perf-results" data-testid="perf-results">
          <thead>
            <tr>
              <th>blocks</th>
              <th>rate/s</th>
              <th>duration</th>
              <th>mount ms</th>
              <th>mount commit ms</th>
              <th>delta n</th>
              <th>delta mean ms</th>
              <th>delta p95 ms</th>
              <th>delta max ms</th>
              <th>append n</th>
              <th>append mean ms</th>
              <th>append p95 ms</th>
              <th>append max ms</th>
              <th>frames</th>
              <th>frame mean ms</th>
              <th>frame max ms</th>
              <th>&gt;16.7ms</th>
            </tr>
          </thead>
          <tbody>
            {results.map((r) => (
              <tr key={r.blocks}>
                <td>{r.blocks}</td>
                <td>{r.ratePerSec}</td>
                <td>{r.durationMs / 1000}s</td>
                <td>{r.mountMs.toFixed(1)}</td>
                <td>{r.mountCommitMeanMs.toFixed(2)}</td>
                <td>{r.liveDeltaCommits}</td>
                <td>{r.liveDeltaMeanMs.toFixed(3)}</td>
                <td>{r.liveDeltaP95Ms.toFixed(3)}</td>
                <td>{r.liveDeltaMaxMs.toFixed(3)}</td>
                <td>{r.liveAppendCommits}</td>
                <td>{r.liveAppendMeanMs.toFixed(3)}</td>
                <td>{r.liveAppendP95Ms.toFixed(3)}</td>
                <td>{r.liveAppendMaxMs.toFixed(3)}</td>
                <td>{r.frames}</td>
                <td>{r.meanMs.toFixed(2)}</td>
                <td>{r.maxMs.toFixed(2)}</td>
                <td>{r.over16Count}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h2>Live mount (most recent configuration)</h2>
      {store && (
        <Profiler id="perf-mount" onRender={handleRender}>
          <PerfMount store={store} />
        </Profiler>
      )}
    </div>
  );
}

function PerfMount({ store }: { store: TranscriptStore }) {
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  // The mount measures the turn renderer the session screens ship — the
  // point of this harness is that it keeps measuring what actually ships.
  return <TurnTranscript items={snapshot.items} live={snapshot.live} filter="all" getToolCall={snapshot.getToolCall} />;
}
