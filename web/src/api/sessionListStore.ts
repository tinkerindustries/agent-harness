import type { SessionState } from "./types";

// The session list's external store (docs/DESIGN.md §5.7:
// useSyncExternalStore, no state library). GET /api/stream sends a full
// snapshot of every session on connect, then one session_state event per
// state change, so this store never needs a separate REST fetch to seed
// itself — one subscription is the whole data source, which also sidesteps
// any race between a snapshot fetch and the stream picking up where it left
// off.
//
// The browser's EventSource retries on its own after a drop; nothing here
// re-implements reconnect.

type Listener = () => void;

export type ConnectionState = "connecting" | "open";

interface Snapshot {
  sessions: SessionState[];
  connection: ConnectionState;
}

function bySessionAge(a: SessionState, b: SessionState): number {
  return b.created_at.localeCompare(a.created_at);
}

class SessionListStore {
  private byID = new Map<string, SessionState>();
  private listeners = new Set<Listener>();
  private snapshot: Snapshot = { sessions: [], connection: "connecting" };
  private dirty = false;
  private flushHandle: number | null = null;

  constructor() {
    this.connect();
  }

  private connect() {
    // Not stored on the instance: this store is a permanent, app-lifetime
    // singleton with nothing that ever tears it down, so there is no
    // second caller that would need this reference.
    const es = new EventSource("/api/stream");
    es.onopen = () => this.setConnection("open");
    es.onerror = () => this.setConnection("connecting"); // EventSource retries on its own
    es.onmessage = (m) => {
      const state = JSON.parse(m.data) as SessionState;
      this.byID.set(state.id, state);
      this.emit();
    };
  }

  private setConnection(connection: ConnectionState) {
    this.snapshot = { ...this.snapshot, connection };
    this.notify();
  }

  // A burst of session_state events becomes one snapshot, on the next frame,
  // the same way the transcript store coalesces deltas (docs/DESIGN.md §5.2).
  // The stream opens by sending one event per session, so a harness with a
  // few dozen rows re-sorted the whole list and notified React once per
  // message. React counts a store notification that lands while it is
  // committing as a nested update and throws "Maximum update depth exceeded"
  // past fifty of them — which is what the session list did on every load,
  // and what dropped the rest of the connect burst on the floor. Sorting once
  // per frame instead of once per message is the same win the transcript
  // store measured.
  private emit() {
    this.dirty = true;
    if (this.flushHandle !== null) return;
    this.flushHandle = requestAnimationFrame(() => {
      this.flushHandle = null;
      this.flush();
    });
  }

  private flush() {
    if (!this.dirty) return;
    this.dirty = false;
    this.snapshot = {
      sessions: [...this.byID.values()].sort(bySessionAge),
      connection: this.snapshot.connection,
    };
    this.notify();
  }

  private notify() {
    for (const l of this.listeners) l();
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): Snapshot => this.snapshot;
}

// One list, one store, for the app's lifetime — matching the single
// session-list screen there is to show (docs/DESIGN.md §5.7, "two screens").
export const sessionListStore = new SessionListStore();
