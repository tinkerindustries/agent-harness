import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { TranscriptStore } from "./transcriptStore";
import type { StoreEvent } from "./types";

// A stand-in for the browser's EventSource: the tests run in node, which has
// none, and a real one would need a server. It records every instance so a
// test can assert which connection is live and push events into it.
class FakeEventSource {
  static opened: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((m: { data: string }) => void) | null = null;
  closed = false;
  private named = new Map<string, (m: unknown) => void>();

  constructor(readonly url: string) {
    FakeEventSource.opened.push(this);
  }

  addEventListener(name: string, fn: (m: unknown) => void) {
    this.named.set(name, fn);
  }

  close() {
    this.closed = true;
  }

  // --- test drivers ---
  emit(ev: StoreEvent) {
    this.onmessage?.({ data: JSON.stringify(ev) });
  }

  emitNamed(name: string, data: unknown) {
    this.named.get(name)?.({ data: JSON.stringify(data) } as unknown);
  }
}

// The store coalesces through requestAnimationFrame, which node also lacks.
// The replacement has to stay asynchronous: the store records the handle its
// scheduler returns, so a callback that runs before that assignment gets its
// own "no flush pending" marker overwritten and the store never flushes
// again. settle() below is the await that lets a scheduled flush run.
function timerScheduler() {
  let seq = 0;
  const cancelled = new Set<number>();
  return {
    scheduleFlush: (cb: () => void) => {
      const id = ++seq;
      queueMicrotask(() => {
        if (!cancelled.has(id)) cb();
      });
      return id;
    },
    cancelFlush: (id: number) => {
      cancelled.add(id);
    },
  };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

function started(seq: number): StoreEvent {
  return {
    seq,
    kind: "session_started",
    payload: { opening_message: "do the thing" },
  } as unknown as StoreEvent;
}

beforeEach(() => {
  FakeEventSource.opened = [];
  (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
});

afterEach(() => {
  delete (globalThis as { EventSource?: unknown }).EventSource;
});

describe("TranscriptStore connection lifecycle", () => {
  it("opens nothing until connect", () => {
    new TranscriptStore("sess-1", timerScheduler());
    expect(FakeEventSource.opened).toHaveLength(0);
  });

  // React's development StrictMode mounts, unmounts and mounts again with no
  // render in between, so the second mount reuses the same store object. A
  // store that could only be closed was dead from that point on, and the
  // screen sat on its empty snapshot for ever.
  it("survives a StrictMode mount / unmount / mount", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    store.disconnect();
    store.connect();

    expect(FakeEventSource.opened).toHaveLength(2);
    expect(FakeEventSource.opened[0].closed).toBe(true);
    expect(FakeEventSource.opened[1].closed).toBe(false);

    FakeEventSource.opened[1].emit(started(1));
    await settle();
    expect(store.getSnapshot().blocks.length).toBeGreaterThan(0);
  });

  // A fresh EventSource sends no Last-Event-ID, so the server replays the
  // session from the beginning. Folding that on top of the first
  // connection's history would show every block twice.
  it("replays into an empty fold rather than on top of the last one", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    FakeEventSource.opened[0].emit(started(1));
    await settle();
    const first = store.getSnapshot().blocks.length;
    expect(first).toBeGreaterThan(0);

    store.disconnect();
    store.connect();
    FakeEventSource.opened[1].emit(started(1));
    await settle();
    expect(store.getSnapshot().blocks).toHaveLength(first);
  });

  it("keeps the metadata row across a reconnect", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    FakeEventSource.opened[0].emitNamed("state", { id: "sess-1", status: "running" });
    await settle();
    expect(store.getSnapshot().state?.status).toBe("running");

    store.disconnect();
    store.connect();
    await settle();
    expect(store.getSnapshot().state?.status).toBe("running");
  });

  it("connects at most one stream at a time", () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    store.connect();
    expect(FakeEventSource.opened).toHaveLength(1);
  });

  // The measurement harness in web/src/perf drives the fold by hand and must
  // never reach the network.
  it("never opens a stream when connect is disabled", () => {
    const store = new TranscriptStore("perf", { connect: false, ...timerScheduler() });
    store.connect();
    expect(FakeEventSource.opened).toHaveLength(0);
    expect(store.getSnapshot().connection).toBe("open");
  });
});

// Ending a stream is the server's call, not an inference off the last
// event's kind (docs/RUN-CONTROL.md "Continuing"). Resume is what makes the
// difference matter: a continued session appends after its terminal event,
// and every later replay of its history carries that terminal event in the
// middle of the log.
describe("closing and reopening", () => {
  it("stays open through a terminal event the server has not closed on", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    const es = FakeEventSource.opened[0];
    es.emit({ seq: 1, kind: "run_finished", payload: { reason: "complete", status: "done", text: "did it" } } as unknown as StoreEvent);
    await settle();
    expect(es.closed).toBe(false);
    expect(store.getSnapshot().connection).not.toBe("closed");
  });

  it("closes on the server's closed marker", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    const es = FakeEventSource.opened[0];
    es.emitNamed("closed", {});
    await settle();
    expect(es.closed).toBe(true);
    expect(store.getSnapshot().connection).toBe("closed");
  });

  it("reopens a closed stream for a session that came back to life", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    const first = FakeEventSource.opened[0];
    first.emit(started(1));
    first.emitNamed("closed", {});
    await settle();

    store.reopen();
    await settle();
    expect(FakeEventSource.opened).toHaveLength(2);
    expect(store.getSnapshot().connection).not.toBe("closed");

    // The replay starts from scratch — a fresh EventSource sends no
    // Last-Event-ID — so the fold is reset and the history folds once, not
    // twice.
    const second = FakeEventSource.opened[1];
    second.emit(started(1));
    await settle();
    expect(store.getSnapshot().blocks.filter((b) => b.type === "opening")).toHaveLength(1);
  });

  it("does nothing when reopen is called on a stream that is already open", async () => {
    const store = new TranscriptStore("sess-1", timerScheduler());
    store.connect();
    store.reopen();
    await settle();
    expect(FakeEventSource.opened).toHaveLength(1);
  });
});
