// The eval SSE clients. Each frame is a whole snapshot rather than a delta,
// so the store is a straight replace: there is nothing to merge and no way
// for the comparison table to disagree with the rows above it. That also
// means none of web/CLAUDE.md's frame-budget machinery applies here — an
// eval changes a few times a minute, not a few times a frame.
//
// EventSource retries on its own, so neither store carries reconnect logic.

import type { EvalRunDetail, EvalRunRow } from "./evals";

// A Store is the useSyncExternalStore triple, in the shape sessionListStore
// exposes: subscribe, read, and the connection state the nav badge shows.
export interface EvalStore<T> {
  subscribe(listener: () => void): () => void;
  snapshot(): T | null;
  connected(): boolean;
  close(): void;
}

function makeStore<T>(url: string): EvalStore<T> {
  let snapshot: T | null = null;
  let connected = false;
  const listeners = new Set<() => void>();
  const emit = () => listeners.forEach((l) => l());

  const source = new EventSource(url);
  source.onopen = () => {
    connected = true;
    emit();
  };
  source.onerror = () => {
    connected = false;
    emit();
  };
  source.onmessage = (e) => {
    try {
      snapshot = JSON.parse(e.data) as T;
    } catch {
      // A frame that will not parse is dropped rather than clearing what is
      // on screen: the next snapshot is whole and arrives shortly.
      return;
    }
    connected = true;
    emit();
  };

  return {
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    snapshot: () => snapshot,
    connected: () => connected,
    close: () => source.close(),
  };
}

// openEvalListStream follows every run: the list on connect, and again
// whenever any of them changes.
export function openEvalListStream(): EvalStore<EvalRunRow[]> {
  return makeStore<EvalRunRow[]>("/api/evals/stream");
}

// openEvalRunStream follows one run's whole detail. Unlike the session list's
// app-lifetime singleton, this is created and torn down by the screen — a run
// stream is about one page, and holding every run a session ever opened would
// be a connection each.
export function openEvalRunStream(id: string): EvalStore<EvalRunDetail> {
  return makeStore<EvalRunDetail>(`/api/evals/${encodeURIComponent(id)}/stream`);
}
