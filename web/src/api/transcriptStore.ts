import { FoldState, type Block, type LiveDelta, type LiveView } from "./fold";
import { SubTurnGroupState, type ChurnPoint, type GroupCounts, type TranscriptItem } from "./groups";
import { DurationStats, PulseMeter } from "./pulse";
import type { SessionState, StoreEvent, Todo, ToolCallPayload } from "./types";

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
  // The display-side grouping of `blocks`: sub-turn cards plus the
  // top-level blocks outside any group. Computed
  // incrementally by SubTurnGroupState so a live-only update keeps the same
  // items reference — the grouped analogue of FoldState's stable blocks
  // reference, and what lets the SubTurnList memo bail out on every delta.
  items: TranscriptItem[];
  live: LiveView;
  todos: Todo[];
  connection: ConnectionState;
  // Whether the server has finished replaying this session's history, marked
  // by its own `replayed` SSE frame (internal/httpapi handleSessionStream).
  // Everything folded before it is the backlog the page loaded with;
  // everything after happened while somebody was watching. The display uses
  // it to animate only the second kind (web/src/hooks.ts useArrivals), which
  // it cannot work out for itself — a long replay arrives across several
  // reads, so the first blocks to land are a fraction of the history.
  replayed: boolean;
  // state is this session's metadata row as the server last published it,
  // delivered by the stream's own `state` frames (internal/httpapi
  // handleSessionStream). null until one arrives, which is the normal case
  // for a finished session: the row only changes while a run is going, so a
  // terminal session's page runs on its REST fetch alone.
  //
  // This is the session-scoped half of the SSE split: the list feed carries a
  // projection of every session's row, and one session's own stream carries
  // all of that session's row. Before it existed the detail screen re-fetched
  // GET /api/sessions/{id} only when its connection flipped, so its cost,
  // cache and sub-turn figures sat frozen for the length of a run.
  state: SessionState | null;
  // counts and churnPoint come out of the same incremental pass that builds
  // items: the filter chip row's numbers and the first cache-churn
  // diagnostic, without a second walk over the blocks.
  counts: GroupCounts;
  churnPoint: ChurnPoint | null;
  // getToolCall is the fold's tool-call registry, exposed read-only for the
  // display layer: the sub-turn card builds its tool headers from the call
  // the fold keeps (web/src/api/fold.ts getToolCall). Stable across
  // snapshots, so memoised components can take it as a prop without breaking
  // their bailouts.
  getToolCall: (id: string) => ToolCallPayload | undefined;
  // The liveness layer (api/pulse.ts). Both ride the snapshot as stable
  // references rather than as values, for the same reason getToolCall does:
  // they are mutable, they are written on the delta path, and a snapshot that
  // carried their contents would put the token-rate channel back into React
  // state — which is the one thing docs/DESIGN.md §5.2 is about.
  //
  // pulse is read by the run pulse canvas on its own animation frame; nothing
  // re-renders when it changes. durations is read during render, but only by
  // components that were already re-rendering on the second (the footer's
  // clock), and only for a median over at most fifty samples.
  pulse: PulseMeter;
  durations: DurationStats;
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
  private pulse = new PulseMeter();
  private durations = new DurationStats();
  // The fold's already-parsed plan as of each block index, appended by
  // ingest() as the fold freezes blocks (see ingest). SubTurnGroupState
  // reads it to name a rail phase from the boundary sub-turn's own plan even
  // when a flush folds a whole burst of blocks at once.
  private todosAtBlock: Todo[][] = [];
  private listeners = new Set<Listener>();
  private snapshot: TranscriptSnapshot;
  private es?: EventSource;
  private connection: ConnectionState = "connecting";
  private replayed = false;
  private state: SessionState | null = null;
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
    // `live` frames are model output the backend has not committed yet
    // (hub.LiveDelta). They arrive as a *named* SSE event, so onmessage —
    // which folds committed events — never sees them, and they carry no id,
    // so they cannot move the EventSource's Last-Event-ID cursor. Both
    // properties are what let the transcript show streaming text without
    // the resume path having to know this feature exists.
    this.es.addEventListener("live", (m) => {
      this.ingestLive(JSON.parse((m as MessageEvent).data) as LiveDelta);
    });
    // The seam between the replayed history and the live tail, for the same
    // two reasons a live frame is shaped this way: named, so onmessage never
    // mistakes it for a committed event, and carrying no id, so it cannot
    // move the Last-Event-ID cursor.
    this.es.addEventListener("replayed", () => this.markReplayed());
    // `state` frames are this session's metadata row, republished whenever it
    // changes. Named and id-less for the same two reasons a live frame is:
    // onmessage folds committed events and would choke on a row, and the
    // resume cursor may only ever name a committed seq.
    this.es.addEventListener("state", (m) => {
      this.state = JSON.parse((m as MessageEvent).data) as SessionState;
      this.markDirty();
    });
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
    const before = this.fold.blocks.length;
    this.fold.ingest(ev);
    // The two halves of the liveness layer are fed on different terms, and
    // the difference is the `replayed` seam (api/pulse.ts explains why).
    //
    // The baselines take the whole log, off the events' own created_at: they
    // are a statement about this session, and a page opened forty sub-turns
    // in should already know what normal looks like here rather than spend
    // the next few minutes learning it.
    this.durations.feedEvent(ev);
    // The pulse takes only what arrived while somebody was watching, off the
    // wall clock. Feeding it the replay would put an entire finished run into
    // whichever half-second bucket the connection happened to land in.
    if (this.replayed) this.pulse.feedEvent(Date.now(), ev);
    // Record the fold's already-applied plan as of the moment each block
    // froze, for the rail's phase grouping: a flush folds whatever blocks
    // arrived since the last one, so without a
    // per-block record every phase in a burst — a finished session's replay,
    // or the perf harness seeding at once — would be named from the plan at
    // the END of the burst (its last TaskCreate/TaskUpdate). Nothing is
    // re-parsed here; latestTodos is the fold's own application of the plan
    // tools' calls.
    for (let i = before; i < this.fold.blocks.length; i++) this.todosAtBlock.push(this.fold.latestTodos);
    this.markDirty();
  }

  // markReplayed closes the history replay by hand, for the connect: false
  // mode the measurement harness runs in: there is no server to send the
  // frame, so the harness calls this once it has seeded its synthetic
  // history and before it starts appending live turns. A live session gets
  // it from the stream instead.
  markReplayed(): void {
    if (this.replayed) return;
    this.replayed = true;
    this.markDirty();
  }

  // ingestLive folds one uncommitted `live` frame, the counterpart to ingest
  // for the other of the two channels a streaming session delivers. The
  // EventSource path above calls the fold directly; this exists so the
  // measurement harness in web/src/perf can drive the same pipeline with
  // opts.connect === false, which it has to in order to measure the streaming
  // reveal at all — the live buffer is the only thing that grows a piece at a
  // time, and it is the input StreamText renders from.
  ingestLive(d: LiveDelta): void {
    this.fold.ingestLive(d);
    // Live frames are the only text that arrives a piece at a time
    // (web/CLAUDE.md, "A `live` SSE frame is not an event"), which makes them
    // the run pulse's whole model-output signal: the committed pair lands in
    // one batch with the turn_finished that freezes the block, and counting
    // it too would draw every sub-turn twice.
    if (this.replayed) this.pulse.feedLive(Date.now(), d);
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
      items: this.groups.sync(this.fold.blocks, this.fold.latestTodos, this.todosAtBlock),
      live: this.fold.live,
      todos: this.fold.latestTodos,
      connection: this.connection,
      replayed: this.replayed,
      state: this.state,
      counts: { ...this.groups.counts },
      churnPoint: this.groups.churnPoint,
      getToolCall: this.getToolCall,
      pulse: this.pulse,
      durations: this.durations,
    };
  }

  // getToolCall is a stable arrow property so every snapshot carries the same
  // reference — a fresh closure per buildSnapshot would defeat the memoised
  // components that take it as a prop.
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
