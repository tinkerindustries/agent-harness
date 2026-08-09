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

  private emit() {
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
