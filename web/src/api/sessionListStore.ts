import type { SessionListRow } from "./types";

// The session list's external store (docs/DESIGN.md §5.7:
// useSyncExternalStore, no state library). GET /api/stream sends a full
// snapshot of every session on connect, then one session_state event per
// state change, so this store never needs a separate REST fetch to seed
// itself — one subscription is the whole data source, which also sidesteps
// any race between a snapshot fetch and the stream picking up where it left
// off.
//
// Every frame is a SessionListRow, not a whole session row: this feed
// re-sends a row on every sub-turn of every running session, so it carries
// only the fields this screen draws (internal/hub's ListRow). A row here is
// a replacement, never a patch — the snapshot and the updates are the same
// shape by construction, so a set() can never drop a field the previous
// frame had.
//
// The browser's EventSource retries on its own after a drop; nothing here
// re-implements reconnect.

type Listener = () => void;

export type ConnectionState = "connecting" | "open";

interface Snapshot {
  sessions: SessionListRow[];
  connection: ConnectionState;
  // finishedRevision is a counter over the *set* of terminal sessions,
  // bumped in flush() only when a cheap signature of that set changes (its
  // count plus the newest terminal session's id). The finished table's hook
  // keys its refetch on it: a run finishing, or a session disappearing,
  // bumps it; a running session streaming progress — which bumps this
  // snapshot many times a second — does not.
  finishedRevision: number;
}

function bySessionAge(a: SessionListRow, b: SessionListRow): number {
  return b.created_at.localeCompare(a.created_at);
}

// finishedSignature is the cheap fingerprint of the terminal-session set:
// the count plus the newest terminal session's id (newest because sessions
// arrive sorted by created_at DESC, so the first terminal row is the newest
// one). A running session's progress changes neither; a run finishing
// changes both; a deletion changes the count.
function finishedSignature(sessions: SessionListRow[]): string {
  let count = 0;
  let newest = "";
  for (const s of sessions) {
    if (s.status === "running") continue;
    count++;
    if (newest === "") newest = s.id;
  }
  return `${count}:${newest}`;
}

class SessionListStore {
  private byID = new Map<string, SessionListRow>();
  private listeners = new Set<Listener>();
  private snapshot: Snapshot = { sessions: [], connection: "connecting", finishedRevision: 0 };
  private finishedSig = "";
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
      const state = JSON.parse(m.data) as SessionListRow;
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
    const sessions = [...this.byID.values()].sort(bySessionAge);
    // The signature is compared before the snapshot is replaced, so the
    // revision advances only when the terminal set actually changed — the
    // property the finished table's refetch depends on.
    const sig = finishedSignature(sessions);
    const finishedRevision =
      sig === this.finishedSig ? this.snapshot.finishedRevision : this.snapshot.finishedRevision + 1;
    this.finishedSig = sig;
    this.snapshot = {
      sessions,
      connection: this.snapshot.connection,
      finishedRevision,
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
