import { useEffect, useRef, useState } from "react";
import type { QueueHealth } from "./api/types";
import { TranscriptStore } from "./api/transcriptStore";

// useNow re-renders its caller on an interval — used only for the session
// list's live elapsed-time column, a handful of rows ticking once a second.
// This is not the phase 5 performance problem (token-rate deltas across
// hundreds of blocks); it is orders of magnitude cheaper.
export function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
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
