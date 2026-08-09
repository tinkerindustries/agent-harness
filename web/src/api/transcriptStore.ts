import { foldEvents, type Block } from "./fold";
import type { StoreEvent } from "./types";

// One session's transcript, live or historical (docs/DESIGN.md §4.2: "the
// same endpoint shape"). The SSE endpoint alone is the whole data source —
// it replays full history before any live event, so there is no separate
// REST fetch to race against it. The browser's EventSource resumes
// automatically on a dropped connection via Last-Event-ID; a page reload
// just opens a fresh connection with none set, which replays everything.

type Listener = () => void;

export type ConnectionState = "connecting" | "open" | "closed";

export interface TranscriptSnapshot {
  blocks: Block[];
  connection: ConnectionState;
}

const TERMINAL_KINDS = new Set<StoreEvent["kind"]>(["run_finished", "error"]);

export class TranscriptStore {
  private events: StoreEvent[] = [];
  private listeners = new Set<Listener>();
  private snapshot: TranscriptSnapshot = { blocks: [], connection: "connecting" };
  private es: EventSource;

  constructor(sessionID: string) {
    this.es = new EventSource(`/api/sessions/${encodeURIComponent(sessionID)}/stream`);
    this.es.onopen = () => this.setConnection("open");
    this.es.onerror = () => {
      if (this.snapshot.connection !== "closed") this.setConnection("connecting");
    };
    this.es.onmessage = (m) => {
      const ev = JSON.parse(m.data) as StoreEvent;
      this.events = [...this.events, ev];
      this.recompute();
      if (TERMINAL_KINDS.has(ev.kind)) {
        // No more events will ever arrive for this session id (compaction
        // aside, which the server already accounts for by closing its end
        // of a compacted session's stream). Closing here stops the
        // browser's automatic reconnect from polling a session that will
        // never have anything new to say.
        this.es.close();
        this.setConnection("closed");
      }
    };
  }

  private recompute() {
    this.snapshot = { blocks: foldEvents(this.events), connection: this.snapshot.connection };
    this.notify();
  }

  private setConnection(connection: ConnectionState) {
    this.snapshot = { ...this.snapshot, connection };
    this.notify();
  }

  private notify() {
    for (const l of this.listeners) l();
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): TranscriptSnapshot => this.snapshot;

  close(): void {
    this.es.close();
  }
}
