import { FoldState, type Block, type LiveView } from "./fold";
import { SubTurnGroupState, type ChurnPoint, type GroupCounts, type TranscriptItem } from "./groups";
import type { StoreEvent, Todo, ToolCallPayload } from "./types";

// One session's transcript, live or historical (docs/DESIGN.md §4.2: "the
// same endpoint shape"). The SSE endpoint alone is the whole data source —
// it replays full history before any live event, so there is no separate
// REST fetch to race against it. The browser's EventSource resumes
// automatically on a dropped connection via Last-Event-ID; a page reload
// just opens a fresh connection with none set, which replays everything.
//
// Deltas never touch React state directly (docs/DESIGN.md §5.2). ingest()
// folds each event into FoldState's mutable buffers and marks the store
// dirty; a requestAnimationFrame loop is what turns that into a snapshot
// change, so a burst of events — token-rate deltas, or hundreds of
// historical events replayed at connect — costs at most one React update
// per frame no matter how many events arrived in it.

type Listener = () => void;

export type ConnectionState = "connecting" | "open" | "closed";

export interface TranscriptSnapshot {
  blocks: Block[];
  // The display-side grouping of `blocks` (docs/WEB-REDESIGN.md phase 4):
  // sub-turn cards plus the top-level blocks outside any group. Computed
  // incrementally by SubTurnGroupState so a live-only update keeps the same
  // items reference — the grouped analogue of FoldState's stable blocks
  // reference, and what lets the SubTurnList memo bail out on every delta.
  items: TranscriptItem[];
  live: LiveView;
  todos: Todo[];
  connection: ConnectionState;
  // counts and churnPoint come out of the same incremental pass that builds
  // items (docs/WEB-REDESIGN.md phase 5): the filter chip row's numbers and
  // the first cache-churn diagnostic, without a second walk over the blocks.
  counts: GroupCounts;
  churnPoint: ChurnPoint | null;
  // getToolCall is the fold's tool-call registry, exposed read-only for the
  // display layer: the sub-turn card builds its tool headers from the call
  // the fold keeps (web/src/api/fold.ts getToolCall). Stable across
  // snapshots, so memoised components can take it as a prop without breaking
  // their bailouts.
  getToolCall: (id: string) => ToolCallPayload | undefined;
}

const TERMINAL_KINDS = new Set<StoreEvent["kind"]>(["run_finished", "error"]);

export interface TranscriptStoreOptions {
  connect?: boolean;
  // scheduleFlush/cancelFlush override what coalesces a dirty store into a
  // flush. Production leaves these as requestAnimationFrame/
  // cancelAnimationFrame, which is the real behaviour docs/DESIGN.md §5.2
  // describes. web/src/perf's measurement harness overrides them with a
  // microtask-based scheduler when it detects rAF is unusably throttled —
  // an automated, backgrounded browser tab clamps requestAnimationFrame the
  // same way it clamps setTimeout, which would make a scripted measurement
  // run take tens of minutes for no more truth than a microtask flush
  // already gives: React's commit cost is what scales with block count, and
  // that cost does not depend on which scheduler triggered the commit.
  scheduleFlush?: (cb: () => void) => number;
  cancelFlush?: (handle: number) => void;
}

const rafSchedule = (cb: () => void) => requestAnimationFrame(cb);
const rafCancel = (handle: number) => cancelAnimationFrame(handle);

export class TranscriptStore {
  private fold = new FoldState();
  private groups = new SubTurnGroupState();
  private listeners = new Set<Listener>();
  private snapshot: TranscriptSnapshot;
  private es?: EventSource;
  private connection: ConnectionState = "connecting";
  private dirty = false;
  private flushHandle: number | null = null;
  private scheduleFlushImpl: (cb: () => void) => number;
  private cancelFlushImpl: (handle: number) => void;

  // opts.connect === false is the synthetic/test mode web/src/perf's
  // measurement harness uses: no EventSource, no network at all. The caller
  // drives the exact same fold-and-flush pipeline a live session runs by
  // calling ingest() directly, at whatever rate it wants to measure.
  constructor(sessionID: string, opts: TranscriptStoreOptions = {}) {
    this.scheduleFlushImpl = opts.scheduleFlush ?? rafSchedule;
    this.cancelFlushImpl = opts.cancelFlush ?? rafCancel;
    this.snapshot = this.buildSnapshot();
    if (opts.connect === false) {
      this.connection = "open";
      this.snapshot = this.buildSnapshot();
      return;
    }

    this.es = new EventSource(`/api/sessions/${encodeURIComponent(sessionID)}/stream`);
    this.es.onopen = () => this.setConnection("open");
    this.es.onerror = () => {
      if (this.connection !== "closed") this.setConnection("connecting");
    };
    this.es.onmessage = (m) => {
      const ev = JSON.parse(m.data) as StoreEvent;
      this.ingest(ev);
      if (TERMINAL_KINDS.has(ev.kind)) {
        // No more events will ever arrive for this session id (compaction
        // aside, which the server already accounts for by closing its end
        // of a compacted session's stream). Closing here stops the
        // browser's automatic reconnect from polling a session that will
        // never have anything new to say.
        this.es!.close();
        this.setConnection("closed");
      }
    };
  }

  ingest(ev: StoreEvent): void {
    this.fold.ingest(ev);
    this.markDirty();
  }

  private setConnection(connection: ConnectionState) {
    this.connection = connection;
    this.markDirty();
  }

  private markDirty() {
    this.dirty = true;
    this.scheduleFlush();
  }

  private scheduleFlush() {
    if (this.flushHandle !== null) return;
    this.flushHandle = this.scheduleFlushImpl(() => {
      this.flushHandle = null;
      this.flush();
    });
  }

  private flush() {
    if (!this.dirty) return;
    this.dirty = false;
    this.snapshot = this.buildSnapshot();
    this.notify();
  }

  private buildSnapshot(): TranscriptSnapshot {
    return {
      blocks: this.fold.blocks,
      items: this.groups.sync(this.fold.blocks),
      live: this.fold.live,
      todos: this.fold.latestTodos,
      connection: this.connection,
      counts: { ...this.groups.counts },
      churnPoint: this.groups.churnPoint,
      getToolCall: this.getToolCall,
    };
  }

  // getToolCall is a stable arrow property so every snapshot carries the same
  // reference — a fresh closure per buildSnapshot would defeat the memoised
  // components that take it as a prop (docs/WEB-REDESIGN.md phase 5).
  private getToolCall = (id: string): ToolCallPayload | undefined => this.fold.getToolCall(id);

  private notify() {
    for (const l of this.listeners) l();
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): TranscriptSnapshot => this.snapshot;

  close(): void {
    this.es?.close();
    if (this.flushHandle !== null) {
      this.cancelFlushImpl(this.flushHandle);
      this.flushHandle = null;
    }
  }
}
