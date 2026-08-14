import { createContext, useContext, useEffect, useRef, useState } from "react";
import type { QueueHealth, SessionState } from "./api/types";
import { getPricing, peakNote, type Pricing } from "./api/pricing";
import { TranscriptStore } from "./api/transcriptStore";

// SessionIdContext carries the session a transcript's blocks belong to.
//
// It is a context rather than a prop because the only consumer is a leaf —
// the screenshot gallery, which needs the id to build the image URL
// (docs/TOOLS.md, "Seeing the screenshots") — and the path to that leaf runs
// through the memoised sub-turn cards whose whole job is to bail out of
// re-rendering (web/CLAUDE.md, "Completed content freezes"). Threading a prop
// down that path would add one to every frozen component on the way. The
// value is a session id, constant for a transcript's life, so no consumer
// ever re-renders on it after mount.
//
// A Task child's transcript re-provides it with the child's own id: the child
// ran in its own workspace, so its screenshots resolve against that session
// and not the parent's. The empty default is what the perf harnesses render
// under, and the gallery falls back to listing paths as text rather than
// requesting an image it cannot address.
export const SessionIdContext = createContext<string>("");

export function useSessionId(): string {
  return useContext(SessionIdContext);
}

// useNow re-renders its caller on an interval — used only for the session
// list's live elapsed-time column, a handful of rows ticking once a second.
// This is not the performance problem docs/DESIGN.md §5.1 is about
// (token-rate deltas across hundreds of blocks); it is orders of magnitude
// cheaper.
export function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}

// useLabelFlip is the gate on .anim-badge-in: it returns the animation class
// only after the label it watches has changed at least once in this mount, and
// the empty string until then. Without it a session list of 38 finished rows
// flips every badge on load — an outcome that was already DONE when the page
// opened did not just happen, and the whole point of the flip is that it did.
// The caller keys the badge on the same label, so a second flip remounts the
// element and replays the 220ms gesture rather than sitting on a class that is
// already applied.
export function useLabelFlip(label: string): string {
  const seen = useRef(label);
  const flipped = useRef(false);
  if (label !== seen.current) {
    seen.current = label;
    flipped.current = true;
  }
  return flipped.current ? "anim-badge-in" : "";
}

// useArrivals is the gate on .anim-row-in, the way useLabelFlip is the gate on
// .anim-badge-in: it answers whether a key is new to a list that was already
// on screen, which is the only thing that animation is for. A page load is not
// an arrival — forty sub-turns that were already in the log did not just
// happen, and animating them says they did.
//
// The caller says WHEN the backlog is complete, through `settled`, and the
// keys present on the first settled render become the baseline. It cannot be
// inferred here, and the obvious guesses are both wrong: these lists are empty
// on mount and fill in over SSE, so "the first render" would make the whole
// backlog an arrival, and "the first non-empty render" is barely better — a
// transcript's replay is delivered across several reads, so the first blocks
// to land are a fraction of the history and every later batch reads as new.
// Measured on a 64-turn session, that guess animated 63 of them.
//
// So each caller passes the signal it actually has: the transcript, the
// server's own `replayed` frame (api/transcriptStore.ts); the session list,
// whether its snapshot has arrived, which for that stream is a single event
// carrying every row at once.
//
// An arrival stays one. The class rides an element that never remounts, so the
// animation runs once when it mounts and leaving the class applied afterwards
// costs nothing — which is also why the returned predicate can be called
// during render without any memoisation.
export function useArrivals(
  keys: readonly (string | number)[],
  settled: boolean,
): (key: string | number) => boolean {
  const baseline = useRef<ReadonlySet<string | number> | null>(null);
  if (baseline.current === null && settled) baseline.current = new Set(keys);
  const base = baseline.current;
  return (key) => base !== null && !base.has(key);
}

// useHeldFrames answers whether `on` has been true for two animation frames
// running. It exists as the fallback half of the session list's "did this
// happen while I was watching" gate (useSettledFlip below): that gate settles
// when the list's snapshot arrives, but an empty harness has no snapshot to
// wait for, and its very first run is exactly the arrival the gate exists to
// catch — so the connection being open, and staying open, stands in for it.
//
// Two frames rather than one because the store coalesces the stream's connect
// burst into a single requestAnimationFrame flush (api/sessionListStore.ts)
// while the connection opening notifies straight away: one frame could easily
// land between the two, and settle on an empty list that was about to have
// rows in it.
export function useHeldFrames(on: boolean): boolean {
  const [held, setHeld] = useState(false);
  useEffect(() => {
    if (!on) {
      setHeld(false);
      return;
    }
    let inner = 0;
    const outer = requestAnimationFrame(() => {
      inner = requestAnimationFrame(() => setHeld(true));
    });
    return () => {
      cancelAnimationFrame(outer);
      cancelAnimationFrame(inner);
    };
  }, [on]);
  return held;
}

// useSettledFlip is useLabelFlip's shape for a fact that is false on the way
// in: it answers whether `value` has changed at least once since the caller's
// baseline, where the baseline is whatever `value` was on the first settled
// render. A page that loads with a session already running must not play the
// gesture that says one just started, and must not animate the stat strip
// down to its compact size on the way in — it was already that size, as far
// as this page load is concerned.
//
// The caller says when the baseline is trustworthy, the way useArrivals's
// `settled` does and for the same reason: these lists are empty on mount and
// fill in over SSE, so "the first render" is always false and would make
// every page load an onset.
export function useSettledFlip(value: boolean, settled: boolean): boolean {
  const baseline = useRef<boolean | null>(null);
  const flipped = useRef(false);
  if (baseline.current === null) {
    if (settled) baseline.current = value;
  } else if (value !== baseline.current) {
    baseline.current = value;
    flipped.current = true;
  }
  return flipped.current;
}

// useQueueHealth polls GET /api/queue on an interval. A plain poll rather
// than the external-store/SSE shape the rest of this app uses: queue health
// changes at human timescales (a redelivery, a halt), not token rate, so
// there is no frame-budget problem here for an external store to solve
// (docs/DESIGN.md §5.2 is about per-token updates, not this).
export function useQueueHealth(intervalMs: number): QueueHealth | null {
  const [health, setHealth] = useState<QueueHealth | null>(null);
  useEffect(() => {
    let cancelled = false;
    const poll = () => {
      fetch("/api/queue")
        .then((r) => (r.ok ? r.json() : null))
        .then((data: QueueHealth | null) => {
          if (!cancelled && data) setHealth(data);
        })
        .catch(() => {
          // A transient fetch failure just leaves the banner showing
          // whatever it last had.
        });
    };
    poll();
    const id = setInterval(poll, intervalMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [intervalMs]);
  return health;
}

// useTranscriptStore owns one TranscriptStore per mounted transcript
// screen: created on mount (or when sessionID changes), closed on unmount.
// No cross-session cache — each visit to a transcript opens its own SSE
// connection and replays from the store, which is cheap and simple rather
// than reusing a possibly-stale one (docs/DESIGN.md §4.2, "the same
// endpoint shape" for historical and live).
export function useTranscriptStore(sessionID: string): TranscriptStore {
  const ref = useRef<{ id: string; store: TranscriptStore } | null>(null);
  if (!ref.current || ref.current.id !== sessionID) {
    ref.current?.store.close();
    ref.current = { id: sessionID, store: new TranscriptStore(sessionID) };
  }
  useEffect(() => {
    const store = ref.current!.store;
    return () => store.close();
  }, [sessionID]);
  return ref.current.store;
}

// useSessionMeta fetches GET /api/sessions/{id} and returns the metadata
// row, preferring the live row the caller passes in — the session stream's
// own `state` frames (transcriptStore), which are what keep a running
// session's figures moving. Re-fetches when refreshOn changes (the stream
// connection opening or closing), so a session with no live row still picks
// up the terminal status the transcript's own run_finished/error block
// already shows, rather than freezing on whatever the first fetch returned;
// that is also what covers the last row of a run, published after the
// terminal event has already closed the stream. Clears only on a genuine
// session switch — a refreshOn change re-fetches without a visible blank
// flicker in between — and swallows a transient fetch failure, which just
// leaves the caller showing whatever it last had.
//
// settled is whether the FIRST fetch for this session has completed,
// successfully or not — not whether the latest one has. The session route
// needs it to tell "row not here yet" (render the shell with an empty stream)
// from "row will never arrive" (keep the chat screen, the safe default for a
// fetch-failed run), and that question is only ever asked once per session.
//
// Only the session-switch effect below clears it. A refreshOn re-fetch must
// not, because the route renders a different subtree while unsettled: every
// connection change would unmount the whole screen and remount it when the
// row landed, throwing away its scroll position and its local state, and
// replaying every mount animation in the transcript. The stream connection
// opens a beat after the first fetch returns on every single page load, so
// that fired every time — see SessionScreen.
export function useSessionMeta(
  sessionId: string,
  refreshOn: unknown,
  live?: SessionState | null,
): { meta: SessionState | null; settled: boolean } {
  const [meta, setMeta] = useState<SessionState | null>(null);
  const [settled, setSettled] = useState(false);

  // Clear on a genuine session switch only, so a refreshOn change (the
  // stream opening or closing) re-fetches without a visible blank flicker
  // in between.
  useEffect(() => {
    setMeta(null);
    setSettled(false);
  }, [sessionId]);

  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api/sessions/${encodeURIComponent(sessionId)}`, { signal: controller.signal })
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => {
        if (data) setMeta(data);
      })
      .catch(() => {
        // A transient fetch failure just leaves the caller showing
        // whatever it last had; the transcript itself still streams from
        // the SSE connection regardless.
      })
      .finally(() => {
        if (!controller.signal.aborted) setSettled(true);
      });
    return () => controller.abort();
  }, [sessionId, refreshOn]);

  // The live row wins whenever there is one. It comes from this session's own
  // SSE stream (transcriptStore's `state` frames), which is republished on
  // every change to the row, so it is never older than the fetch — while the
  // fetch, which only re-runs when the connection flips, went stale the
  // moment the run's next sub-turn landed. A session with no live row (a
  // finished run, a stream that has not opened) falls back to the fetch, and
  // `settled` stays a statement about the fetch alone: it is the "will a row
  // ever arrive" question, and only the fetch can answer it.
  return { meta: live ?? meta, settled };
}

// usePricingSchedule fetches the rate schedule once per mount. It is config,
// not live data — the table changes when somebody redeploys — so a plain
// fetch, not the SSE feed, and a failure leaves the caller with null, which
// every consumer renders as saying nothing about peak hours. That is also
// what a harness with no schedule shows, and it is correct for both.
export function usePricingSchedule(): Pricing | null {
  const [pricing, setPricing] = useState<Pricing | null>(null);
  useEffect(() => {
    let cancelled = false;
    getPricing().then((p) => {
      if (!cancelled) setPricing(p);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  return pricing;
}

// usePeakNote is usePricingSchedule turned into the sentence a screen shows,
// recomputed on the caller's own clock: "" off-peak, "peak rate for 40m"
// inside a window. The caller passes the `now` it already ticks on rather
// than this starting a second interval — the session list re-renders every
// second for its elapsed column regardless, so the note costs nothing there.
export function usePeakNote(nowMs: number): string {
  const pricing = usePricingSchedule();
  return peakNote(pricing?.schedule, new Date(nowMs));
}
