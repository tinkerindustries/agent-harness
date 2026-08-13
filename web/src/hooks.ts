import { createContext, useContext, useEffect, useRef, useState } from "react";
import type { QueueHealth, SessionState } from "./api/types";
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
// row. Re-fetches when refreshOn changes (the stream connection opening or
// closing), so the status badge picks up the terminal status the
// transcript's own run_finished/error block already shows, rather than
// freezing on whatever the first fetch returned. Clears only on a genuine
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

  return { meta, settled };
}
