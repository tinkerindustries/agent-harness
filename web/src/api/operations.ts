// The operations client: the fetch calls, the wire types, and the pure
// logic the operations screen is built from (docs/DATA-API.md phase 5).
// Kept as a module separate from the React component, like settings.ts and
// fold.ts — the component renders, this talks to the server, and the tests
// pin this module's wire shape and decisions without any DOM.
//
// The screen this module feeds is where the browser first *acts* on the
// harness: closing a stuck session, closing a dead work request, releasing a
// stranded lease. Every write therefore echoes the row's version back in
// If-Match and the server's own error text is what the screen shows when a
// write is refused — a 412 names the current version, a 409 names the
// evidence (the last event, the last heartbeat). Nothing here guesses at the
// reason.

import type { EventsPage, SessionState } from "./types";

// sessionIdleThresholdMs mirrors internal/httpapi's sessionIdleThreshold
// (docs/DATA-API.md "Preconditions"): ten minutes. The server owns the real
// guard — the close endpoints refuse with a 409 naming the last event's time
// when they disagree — so this value is the screen's display-side estimate
// of "quiet", not a second enforcement of the rule.
export const SESSION_IDLE_THRESHOLD_MS = 10 * 60 * 1000;

// eventsPageSize is the ?limit= the module asks for when it wants the tail
// of a log: http.events_limit_max's default (internal/settings/registry.go),
// the largest page the server accepts. The server clamps a request to its
// configured max, and the paging loop follows has_more/next either way, so
// the constant only bounds the number of round trips, never the answer.
export const EVENTS_PAGE_SIZE = 5000;

// WorkRequestRow is one work_requests row over HTTP, mirroring
// internal/httpapi.workRequestRow: the idempotency row plus the version
// every row resource carries (docs/DATA-API.md phase 3). session_id is
// absent for a request that never ran; finished_at is absent while running.
export interface WorkRequestRow {
  request_id: string;
  session_id?: string;
  status: string;
  result?: unknown;
  received_at: string;
  finished_at?: string;
  delivery_count: number;
  version: number;
}

// WorkspaceLeaseRow is one workspace_leases row over HTTP, mirroring
// internal/httpapi.workspaceLeaseRow (docs/DATA-API.md phase 3). The
// workspace key is a path and may contain slashes, so a client
// percent-encodes it when addressing the row.
export interface WorkspaceLeaseRow {
  workspace: string;
  session_id: string;
  acquired_at: string;
  heartbeat_at: string;
  version: number;
}

// --- reads ---

// listSessions fetches GET /api/sessions: every session row, newest first,
// each carrying the version a write must echo back in If-Match.
export async function listSessions(): Promise<SessionState[]> {
  const res = await fetch("/api/sessions");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as SessionState[];
}

// getWorkRequest fetches GET /api/requests/{request_id}: the row, distinct
// from the /status poll snapshot — this answers "what does the table say".
export async function getWorkRequest(requestId: string): Promise<WorkRequestRow> {
  const res = await fetch(`/api/requests/${encodeURIComponent(requestId)}`);
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as WorkRequestRow;
}

// listWorkRequests fetches GET /api/requests: every work-request row,
// newest first by received_at, each carrying the version a write must echo
// back in If-Match. The collection is how the operations screen finds a
// request whose worker died during workspace preparation: it never got a
// session, so it has no session to be discovered through, only this row.
export async function listWorkRequests(): Promise<WorkRequestRow[]> {
  const res = await fetch("/api/requests");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as WorkRequestRow[];
}

// listLeases fetches GET /api/leases: the lease table, keyed by workspace.
export async function listLeases(): Promise<WorkspaceLeaseRow[]> {
  const res = await fetch("/api/leases");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as WorkspaceLeaseRow[];
}

// fetchEvents fetches one page of a session's event log
// (docs/DATA-API.md "events"). The log is read-only over HTTP, forever.
export async function fetchEvents(sessionId: string, from: number, limit: number): Promise<EventsPage> {
  const res = await fetch(
    `/api/sessions/${encodeURIComponent(sessionId)}/events?from=${from}&limit=${limit}`,
  );
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as EventsPage;
}

// fetchLastEventAt walks a session's event log to its end and returns the
// last event's created_at, or null when the log is empty. The session row
// itself carries no last-activity time, and the events endpoint reads
// forward from seq 0, so the tail is reachable only by paging: follow
// has_more/next until the page reports no more, and keep the last event
// seen. A session that never appended an event has nothing recent and is
// treated as quiet — the same rule the server's idle precondition applies
// (docs/DATA-API.md "Preconditions": "A session with no events has nothing
// recent and passes").
export async function fetchLastEventAt(sessionId: string): Promise<string | null> {
  let from = 0;
  let last: string | null = null;
  for (;;) {
    const page = await fetchEvents(sessionId, from, EVENTS_PAGE_SIZE);
    if (page.events.length > 0) {
      last = page.events[page.events.length - 1].created_at;
    }
    if (!page.has_more || page.next === undefined) break;
    from = page.next;
  }
  return last;
}

// --- writes ---

// writeInit is the header set every mutating write carries: the JSON
// content type the server demands (415 without it) and the row's version
// echoed back in If-Match (428 without it, 412 when stale). The browser
// adds its own same-origin Origin, which passes the origin guard
// (docs/DATA-API.md "The guards every write carries").
function writeInit(version: number): RequestInit {
  return {
    method: "PATCH",
    headers: { "Content-Type": "application/json", "If-Match": String(version) },
  };
}

function deleteInit(version: number): RequestInit {
  return {
    method: "DELETE",
    headers: { "Content-Type": "application/json", "If-Match": String(version) },
  };
}

// closeSession closes a session into a terminal status via PATCH
// /api/sessions/{id} (docs/DATA-API.md phase 1). A running session whose
// most recent event is newer than the idle threshold is refused with a 409
// naming the last event's time; a stale version is a 412 naming the current
// one — both carried out as the Error's message, which the screen shows
// verbatim.
export async function closeSession(id: string, version: number, status: string): Promise<void> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}`, {
    ...writeInit(version),
    body: JSON.stringify({ status }),
  });
  if (!res.ok) throw await apiError(res);
}

// deleteSession removes a session row and its whole event log via DELETE
// /api/sessions/{id} (docs/DATA-API.md phase 1). A running session is
// refused with 409 regardless of idleness — the operator closes it first
// with PATCH, then deletes it.
export async function deleteSession(id: string, version: number): Promise<void> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}`, deleteInit(version));
  if (!res.ok) throw await apiError(res);
}

// closeWorkRequest closes a work request into a terminal status via PATCH
// /api/requests/{request_id} (docs/DATA-API.md phase 3). A request whose
// session is still live is refused with a 409 naming the last event's time.
export async function closeWorkRequest(requestId: string, version: number, status: string): Promise<void> {
  const res = await fetch(`/api/requests/${encodeURIComponent(requestId)}`, {
    ...writeInit(version),
    body: JSON.stringify({ status }),
  });
  if (!res.ok) throw await apiError(res);
}

// deleteWorkRequest removes a work-request row via DELETE
// /api/requests/{request_id} (docs/DATA-API.md phase 3).
export async function deleteWorkRequest(requestId: string, version: number): Promise<void> {
  const res = await fetch(`/api/requests/${encodeURIComponent(requestId)}`, deleteInit(version));
  if (!res.ok) throw await apiError(res);
}

// releaseLease releases a workspace lease via DELETE /api/leases/{workspace}
// (docs/DATA-API.md phase 3). The lease key is a workspace path and may
// contain slashes, so it is percent-encoded: "/tmp/ws" becomes %2Ftmp%2Fws,
// which is the exact shape the server's wildcard route matches. A lease
// whose heartbeat is newer than the idle threshold is refused with a 409
// naming the last heartbeat.
export async function releaseLease(workspace: string, version: number): Promise<void> {
  const res = await fetch(`/api/leases/${encodeURIComponent(workspace)}`, deleteInit(version));
  if (!res.ok) throw await apiError(res);
}

// --- run control (docs/RUN-CONTROL.md) ---

// StopResponse is POST /api/sessions/{id}/stop's 202 body: the acceptance,
// not the outcome. The run is still ending; its terminal state arrives over
// the session's own SSE stream (a cancelled result for a queue caller, the
// stream for the browser), and the status badge renders CANCELLED when it
// lands.
export interface StopResponse {
  session_id: string;
  stopping: boolean;
}

// controlToken is the run-control bearer token, fetched once per page load
// and reused for every stop. GET /api/control-token serves the token to
// loopback callers only (docs/RUN-CONTROL.md "Authentication"), and the
// browser is served by the harness itself, so this works exactly when the
// page does. null means run control is not configured: a harness that never
// generated a token answers 200 with an empty string, and an empty token is
// treated as unavailable rather than cached and sent as an empty bearer,
// which would 503 — a missing credential fails closed.
let controlTokenPromise: Promise<string | null> | null = null;

export function controlToken(): Promise<string | null> {
  controlTokenPromise ??= fetch("/api/control-token")
    .then((res) => (res.ok ? res.json() : null))
    .then((body: { token?: string } | null) => (body && body.token ? body.token : null))
    .catch(() => null);
  return controlTokenPromise;
}

// stopSession asks the harness to end a running session via POST
// /api/sessions/{id}/stop (docs/RUN-CONTROL.md "The HTTP surface"). The
// response is an acceptance, not an outcome: a 202 means the stop landed and
// the run is ending — possibly on its own inside the grace period — and the
// terminal state arrives over the SSE stream the caller is already
// connected to; nothing here polls or guesses at it. The reason is optional
// and carried verbatim into the cancelled result. The bearer token is
// required: callers hold the controlToken() result and hide the control when
// it is null rather than sending a request that would 503.
export async function stopSession(id: string, token: string, reason?: string): Promise<StopResponse> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/stop`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify(reason ? { reason } : {}),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as StopResponse;
}

// SteerResponse is POST /api/sessions/{id}/steer's 202 body: the acceptance
// plus the seq the caller's text landed at. Steering is not idempotent — two
// steers are two instructions — so the seq is how a caller tells its own
// steer from any other (docs/RUN-CONTROL.md "The HTTP surface").
export interface SteerResponse {
  session_id: string;
  seq: number;
}

// steerSession appends an instruction to a running session via POST
// /api/sessions/{id}/steer (docs/RUN-CONTROL.md "The HTTP surface"). The
// response is an acceptance, not a delivery: the text reaches the model at
// the next sub-turn boundary, which may be a minute or more away if a long
// tool call is in flight, and the transcript's steer block shows it as
// pending until then. The text is carried verbatim. The bearer token is
// required, exactly as for stopSession.
export async function steerSession(id: string, token: string, text: string): Promise<SteerResponse> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/steer`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify({ text, source: "web" }),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as SteerResponse;
}

// StartRunResponse is POST /api/runs's 202 body: the request_id the run was
// accepted under. A start is not answered by the run's outcome — the session
// appears on the existing GET /api/stream list feed once the pool claims the
// request, and nothing here polls for it or invents a row (docs/RUN-CONTROL.md
// "POST /api/runs").
export interface StartRunResponse {
  request_id: string;
}

// WorkRequest is the queue.Request wire shape POST /api/runs accepts,
// mirroring internal/queue.Request: the required repos and permission_mode,
// plus the optional prompt — a browser start may create the run first and
// let the operator type the first message into the session (docs/RUN-CONTROL.md
// "Start") — and the optional fields harness publish's flags set. request_id
// is absent for a browser start — the server generates one, since a browser
// form has no idempotency key to offer (docs/RUN-CONTROL.md "POST /api/runs").
// Provenance is deliberately absent: the server stamps parent_is_user,
// parent_agent_type, and parent_agent_id on POST /api/runs and ignores
// anything the body sends, so the browser must not carry the fields at all
// (docs/RUN-CONTROL.md "POST /api/runs").
export interface WorkRequest {
  prompt?: string;
  repos: { url: string; branch?: string }[];
  permission_mode: string;
  model?: string;
  effort?: string;
  deny?: string[];
  result_schema?: unknown;
  max_sub_turns?: number;
  deadline_ms?: number;
  job_type?: string;
}

// startRun publishes a work request via POST /api/runs (docs/RUN-CONTROL.md
// "POST /api/runs"). The 202 carries the request_id the run was accepted
// under; the session itself appears on the session-list feed once the pool
// claims it, and the screen follows it from there rather than polling. The
// bearer token is required, exactly as for stopSession: callers hide the
// form when controlToken() is null rather than sending a request that would
// 503.
export async function startRun(token: string, body: WorkRequest): Promise<StartRunResponse> {
  const res = await fetch("/api/runs", {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as StartRunResponse;
}

// parseRepoSpec splits one repository spec on its last "#" into the url and
// branch the wire shape carries, exactly as harness publish's -repo flag
// does (cmd/harness/publish.go parseRepoFlags): "https://x/y.git#dev" →
// {url, branch}, "https://x/y.git" → {url} with no branch. Splitting on the
// last "#" keeps a "#" inside a URL intact, and a spec with none at all
// clones the default branch.
export function parseRepoSpec(spec: string): { url: string; branch?: string } {
  const v = spec.trim();
  const i = v.lastIndexOf("#");
  if (i < 0) return { url: v };
  return { url: v.slice(0, i), branch: v.slice(i + 1) };
}

// --- logic the screen is built from (tested without a DOM) ---

// isStuckSession reports whether a session row counts as stuck: still
// running, with no demonstrated liveness — either it never appended an
// event, or its most recent event is older than the idle threshold
// (docs/DATA-API.md "Preconditions"). The threshold is the server's
// sessionIdleThreshold mirrored in this module; the server's 409 is the
// authority when the two disagree. lastEventAtMs is null for a session with
// no events, which counts as stuck because the server's own idleness check
// would let the close through.
export function isStuckSession(
  status: string,
  lastEventAtMs: number | null,
  nowMs: number,
  thresholdMs: number = SESSION_IDLE_THRESHOLD_MS,
): boolean {
  if (status !== "running") return false;
  return lastEventAtMs === null || nowMs - lastEventAtMs > thresholdMs;
}

// quietMs is how long a row has been quiet: now minus the last event (or
// last heartbeat), clamped at zero, or null when there is no timestamp to
// measure from. Null renders as "no events yet" / "never heartbeated"
// rather than a fabricated duration.
export function quietMs(lastActivityAtMs: number | null, nowMs: number): number | null {
  if (lastActivityAtMs === null) return null;
  return Math.max(0, nowMs - lastActivityAtMs);
}

// formatDuration renders a quiet duration the way the operations screen
// shows it: the largest units that are non-zero, "45s" under a minute,
// "12m" under an hour, "3h 5m" beyond — the "how long has it been quiet"
// question, which does not need the seconds of a row that has been quiet
// for hours.
export function formatDuration(ms: number): string {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  return `${seconds}s`;
}

// errorMessage is the text the screen shows under a row after a failed
// write: the server's own words. apiError already carries the {"error": "..."}
// body's message out, so this is just the Error-to-string hop — kept here so
// the module owns the whole "error response becomes message" path and the
// component never formats a failure itself.
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// apiError turns a non-2xx response into a readable Error. Every error the
// server writes carries {"error": "..."} — a 409 names the last event or
// heartbeat, a 412 names the current version, a 400 names the accepted
// values — so the message can go straight to the screen instead of a failed
// write looking like a success. A non-JSON body (the plain-text 404s) falls
// back to the status line. status rides on the Error so a caller can tell
// "the row is gone" (404) from "the write was refused" without parsing the
// message.
export async function apiError(res: Response): Promise<Error> {
  let message = `${res.status} ${res.statusText}`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) message = body.error;
  } catch {
    // A non-JSON error body falls back to the status line.
  }
  const err = new Error(message) as Error & { status?: number };
  err.status = res.status;
  return err;
}
