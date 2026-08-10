# The data write API

The HTTP API's read-only rule is retired ([docs/DESIGN.md §4.2](DESIGN.md#42-transport))
and this document is the surface that replaces it: every resource the harness
manages, what each endpoint does, and the invariants every write carries. It is
the contract the remaining phases of the data-API work implement against, so it
covers the whole surface — including the parts later phases build — and names
the decisions those phases must copy rather than re-make.

Two distinctions bracket what belongs here:

- **Data, not run control.** A write that changes a row the harness manages —
  closing an abandoned session, deleting a finished one, closing a dead work
  request — is data and belongs in this API. A write that *starts, steers, or
  stops a run* is run control and lives in [RUN-CONTROL.md](RUN-CONTROL.md),
  whose seams are chosen: the HTTP server holds no JetStream handle and
  `internal/httpapi` imports neither `internal/session` nor
  `internal/worker` ([ARCHITECTURE.md](../ARCHITECTURE.md)). Three
  run-control actions are built: `POST /api/runs`, which publishes a
  validated work request to the WORK stream through the declared
  `RunPublisher` interface (implemented by `cmd/harness` over the queue's own
  handle); `POST /api/sessions/{id}/stop`, authenticated by the
  `http.control_token` bearer token and acting on a run through the declared
  `RunController` interface; and `POST /api/sessions/{id}/steer`, the same
  guards but no seam at all — it is a store write the loop reads at its next
  sub-turn boundary. That import boundary is what keeps run control a
  declared seam rather than a reach into a running loop.
- **The store is the only record.** The event log is append-only and never
  editable; see [Events are not writable](#events-are-not-writable).

Phase ownership: sessions (list, get, PATCH, DELETE), work requests (list,
get, PATCH, DELETE), and workspace leases (list, DELETE) are built. Events
paging and the admin UI are specified here and built in later phases.
Settings is built and is the pattern the other resources follow.

## Resource model

Five resources. One shape of write everywhere: a guard trio (content type,
origin, and the idle precondition where the row a run may be using), optimistic
concurrency on the row, and a JSON error body.

| Resource | Endpoint | Read | Write | Phase |
| --- | --- | --- | --- | --- |
| sessions | `/api/sessions` | GET (list), GET `/api/sessions/{id}` | PATCH `/api/sessions/{id}`, DELETE `/api/sessions/{id}` | 1 (built) |
| events | `/api/sessions/{id}/events` | GET (paged, `?from=&limit=`) | **none** | 4 (paging) |
| work_requests | `/api/requests` | GET (list), GET `/api/requests/{request_id}` (row), GET `/api/requests/{request_id}/status` (poll snapshot) | PATCH, DELETE `/api/requests/{request_id}` | 3 (built) |
| workspace_leases | `/api/leases` | GET (list) | DELETE `/api/leases/{workspace}` | 3 (built) |
| settings | `/api/settings` | GET `/api/settings` | PUT, DELETE `/api/settings/{key}` | built |

### sessions

A session is the frozen metadata row for one agent run — model, effort,
workspace, permission mode, the rendered system prompt and tool schema, status,
`created_at`, `finished_at` — plus its event log (the fold, the transcript, and
resume all read it as an append-only log; [DESIGN.md §4.1](DESIGN.md#41-event-sourced-session)).

- `GET /api/sessions` — list, newest first, each row with status, cost, and the
  originating request id.
- `GET /api/sessions/{id}` — one row, the same shape as a list row.
- `PATCH /api/sessions/{id}` — close an abandoned session: transition to a
  terminal status and set `finished_at`. Phase 1.
- `DELETE /api/sessions/{id}` — remove the row and its whole event log. Phase 1.

A session row's representation carries `version` (see
[Optimistic concurrency](#optimistic-concurrency)). The write endpoints change
status and timestamps only; the rendered system prompt, tool schema, and event
log are never editable over HTTP.

### events

The event log is read-only over HTTP, forever — see
[Events are not writable](#events-are-not-writable). Phase 4 adds paging and
kind filtering:

- `GET /api/sessions/{id}/events?from=<seq>&limit=<n>&kind=<a>,<b>` — one
  page of the log in seq order, starting at seq `from` (0, the default,
  starts at the beginning), at most `limit` rows (clamped to
  `http.events_limit_default` and `http.events_limit_max`), restricted to the
  comma-separated `kind` names when present.
- The response is the pre-existing body plus paging metadata alongside it —
  `events`, `from`, and `limit` keep their exact meanings:

  ```json
  {"events": [...], "from": 1, "limit": 500, "has_more": true, "next": 501}
  ```

  `has_more` is whether more events — more *matching* events when `kind` is
  set — follow this page, decided exactly by peeking one row past it, so a
  log that ends precisely on a page boundary reports no more. `next` is
  present only when `has_more` is true and is the `from` to ask for the next
  page with: a client pages forward by echoing it back and stops when
  `has_more` is false.
- An unknown `kind` name is a 400 whose message names the valid kinds. The
  valid list is `store.EventKinds` in `internal/store` — the filter accepts
  exactly the kinds the log can hold and the 400 names the same list, so
  they can never disagree. No `kind` means every kind. The filter runs in
  SQL: a page of tool traffic never pulls the transcript's reasoning and
  content deltas off the disk.

Plus the SSE transcript stream.

### work_requests

A work request is the idempotency row for one queued job: request id, the
session that ran it, status, result JSON, `received_at`, `finished_at`,
delivery count. It is **single-use**: the row's `session_id` is attached the
moment an attempt's session row exists, and once it is set the request never
runs again, whatever its status ([DESIGN.md §4.10](DESIGN.md#410-work-ingress-over-nats-jetstream)).
A request is retried by republishing it under a new `request_id`, never by
re-running the old one. Phase 3 adds:

- `GET /api/requests` — the work_requests table: every row, newest first by
  `received_at` (the work-request analog of the sessions list's
  `created_at` order), each row the same shape as `GET
  /api/requests/{request_id}` including `version`. The collection is how an
  operator finds a request whose worker died during workspace preparation:
  it never got a session, so it has no session to be discovered through,
  only this row. It is a read and carries no write guards.
- `GET /api/requests/{request_id}` — the row: request id, session id, status,
  result JSON, `received_at`, `finished_at`, delivery count, and `version`.
  The existing `/status` snapshot is untouched: the snapshot answers "what is
  the run doing right now", this answers "what does the table say".
- `PATCH /api/requests/{request_id}` — close a dead request (a request left
  `running` by a dead worker still holds its spent row and needs the same
  kind of operator close sessions do).
- `DELETE /api/requests/{request_id}` — remove the row.

### workspace_leases

A lease is a workspace held by a session: `workspace`, `session_id`,
`acquired_at`, `heartbeat_at`. Phase 3 adds:

- `GET /api/leases` — the lease table, keyed by workspace, each row carrying
  `version`.
- `DELETE /api/leases/{workspace}` — release a lease. The lease key is a
  workspace path, so it can contain slashes; a client percent-encodes them
  (`DELETE /api/leases/%2Ftmp%2Fws`).

### settings

Already built and the pattern the other resources copy: the write guards, the
JSON error body, `{"ok": true}` on success. Settings has no version column —
it is a key/value table whose writes are last-write-wins by design — so it does
not carry the concurrency mechanism; every *row* resource does.

## Events are not writable

The event log is the store's only record and reasoning cannot be reconstructed
([DESIGN.md §3.1](DESIGN.md#31-reasoning_content-replay)). The fold builds the
DeepSeek `messages` array from it, the transcript and the SSE stream replay it,
and resume continues from it — all of them read the log as an append-only
sequence with a per-session monotonic `seq`. An edit or a delete would silently
rewrite history every consumer trusts.

This is a *different* invariant from the read-only rule retired today. That
rule said the HTTP surface had no writes at all; this one says a specific
resource is never writable, and it does not retire. The two write endpoints of
phase 1 change the session row and leave the log alone.

An operator note, if one is ever wanted, is a **new event appended** — never an
edit of an existing one. There is deliberately no endpoint to append one by
hand; if that day comes it is a store-level append exactly like the runner's,
with the same `seq` assignment and hub fan-out, not a new privilege on the
events resource.

## The guards every write carries

Every write endpoint — settings today, sessions from phase 1, work requests and
leases from phase 3 — runs the same two guards before touching anything:

- **`Content-Type: application/json`, or 415.** A missing or wrong content type
  is refused. This stops a cross-origin form post before anything else runs.
- **Same-origin, or 403.** When the request carries an `Origin` header it must
  match the request's own `Host` (scheme aside); a request with no `Origin` is
  not browser-initiated and passes. The server binds loopback by default.

Both are implemented once in `internal/httpapi` — `writeGuards`, which composes
`requireJSONContentType` and `checkOrigin` — and every write handler calls that
one helper rather than repeating the pair.

Say plainly what this is: **thin.** Loopback plus an origin check is the whole
of the current story, and it holds only while the surface is one operator's own
machine. Transcripts carry workspace paths, file contents, and command output,
and the write surface now reaches the store. Authentication is the open
question the interactive frontend forces — [DESIGN.md §4.2](DESIGN.md#42-transport)
names it as stage two's — and nothing in this API pretends to be a substitute.
Treat the port as sensitive and keep it loopback-bound.

## Preconditions: a write must not race a run

A write that targets a row a run may be using must **fail rather than race**.
Two rules, both surfaced as **409 Conflict**:

- **PATCH /api/sessions/{id} on a session that is still live.** Status alone is
  not enough to tell "abandoned" from "mid-run" — the whole point of the
  endpoint is that an abandoned row still says `running`. Idleness is the
  test: a live run appends events continuously, so a session whose most recent
  event is newer than the idle threshold is presumed live and the close is
  refused. The threshold is `sessionIdleThreshold` in `internal/httpapi` — ten
  minutes, a named constant with a comment, never a literal — and the refusal
  is a 409 whose message says when the last event was. A session with no events
  has nothing recent and passes: a row that has never appended has no
  demonstrated liveness to protect.
- **DELETE /api/sessions/{id} on a running session.** `store.DeleteSession`
  refuses on status regardless of idleness (it cannot close an abandoned row —
  the operator closes it with PATCH first, then deletes it). The refusal is a
  409.

The PATCH idleness check runs **inside the store's write transaction**, the
same transaction that performs the status change: every write, including every
event append, funnels through the store's single writer goroutine, so checking
"last event older than the threshold" and "now set the terminal status" in one
transaction means no live run's append can slip between the two. A check
outside the transaction would be a race, not a guard.

Phase 3 reuses the same rule and the same status code for the two new row
resources, each judged by the signal its row already carries rather than a new
one:

- **PATCH and DELETE /api/requests/{request_id} on a request in flight.** The
  request's `session_id` is the signal. A running request whose session's most
  recent event is newer than the idle threshold is being run by a live pool
  worker right now — it will publish its own terminal result — and both the
  close and the delete are refused with a 409 naming the session's last
  event's time, exactly as the session endpoints name it. A request with **no
  session id at all has never run** — the single-use guard only fires once an
  attempt's session row exists — so it has no demonstrated liveness to protect
  and is closable and deletable. That is a deliberate decision, not an
  omission: the alternative (refuse everything sessionless) would strand a
  request whose worker died during workspace preparation, before its session
  existed, forever. The narrow race it accepts — closing a request in the
  seconds between claim and session attach — costs at most a run whose result
  the worker still publishes (its finish is fenced on the row's session id).
- **DELETE /api/leases/{workspace} on a live lease.** `heartbeat_at` is the
  signal: a live session heartbeats its lease, so a lease whose heartbeat is
  newer than the idle threshold is held by a live session right now and the
  release is refused with a 409 naming the last heartbeat, the lease analog of
  the session endpoints' last-event refusal. Both thresholds are the same
  named constant — `sessionIdleThreshold` in `internal/httpapi`, ten minutes.

A dead request — one whose session is idle, or that never ran — and a stranded
lease are the exact rows these endpoints exist to close and release, so both
writes go through once the row is demonstrably quiet. A request DELETE differs
from the session DELETE in one respect deliberately: it does not refuse a
`running` row that is merely abandoned. The session DELETE refuses running
regardless of idleness because a live session goroutine may still be appending
to the session row at any instant; a dead request's row is written only by the
worker's finish path, which the operator has already proven quiet, so an
abandoned request row can be deleted directly. The idempotency caveat is the
same one session deletes carry — deleting the row of a request whose message
is still being redelivered lets a later delivery claim it fresh, so the
PATCH-then-DELETE sequence remains the safe order — but the endpoint does not
force it, and the spec's own words ("refuse with 409 when the request row a
live worker may be using") name only the live case.

Phases 4 and 5 use the same rule and the same status code: a write that targets
a row a run may be using refuses with 409, never queues, never overwrites.

## Optimistic concurrency

The mechanism, decided in phase 1 and copied by phases 3 to 5:

> Every row a mutating endpoint targets carries a monotonic `version` integer,
> starting at 1 and incremented by 1 on every successful mutation of that row.
> The API returns it in the resource representation and in the response to
> every mutating write. A mutating write must echo it back in an **`If-Match`**
> header: `If-Match: <version>`. A missing header is **428 Precondition
> Required**; a header that does not match the row's current version is
> **412 Precondition Failed** with a message naming the current version.

Why `If-Match` against a per-row version rather than a timestamp or a body
field:

- **If-Match is the standard HTTP form** (RFC 7232), so any HTTP client, cache,
  or proxy already knows what it means; the body stays a pure description of
  the intended change, and the version is just an opaque token the client
  echoes back.
- **A version is robust where a timestamp is not.** The client echoes it
  blindly, so it survives clock skew between the harness and the operator's
  machine, two writes in the same millisecond, and an NTP step — all of which
  break a "compare my timestamp against yours" scheme.
- **A column applies uniformly to every row resource.** Sessions,
  work_requests, and workspace_leases each get one; none of them has an
  `updated_at` to lean on, and inventing per-resource timestamps would give
  phases 3 to 5 three different mechanisms to copy instead of one.

For a session, the version is checked **inside the store write transaction**
alongside the precondition, so the two together are atomic with respect to
every other writer. The runner's own terminal write bumps the version, which is
exactly the protection the close endpoints need: an operator who read the
session before the run finished gets a 412, not a write that lands on a row
that already changed.

Phase 3 adds the column to `work_requests` and `workspace_leases` with the same
semantics: version 1 at creation, `+1` per mutation, `If-Match` required on
mutating writes, 428 when absent, 412 when stale.

**Run control is the deliberate exception** (docs/RUN-CONTROL.md "The HTTP
surface"): `POST /api/sessions/{id}/stop` and `POST /api/sessions/{id}/steer`
require no `If-Match`. That rule
exists so an operator's write cannot land on a row that changed since they
read it — a stop or a steer is an action on a run, not an edit of a row, and a
running session's `version` changes continuously underneath the caller as the
runner commits. Requiring a version echo would make a correct stop racy by
construction and push the operator to fetch-then-immediately-post, which is
the check without the protection. The preconditions that matter there are
about the *run*, not the row: the session exists (404), and it is running
(409 naming the session's status).

Why a **lease** carries a version when the only mutation this API performs on
it is a delete: the version is what makes the delete's `If-Match` mean
something. The lease is a row a run acquires and releases; if the operator
reads the table and the lease is released and re-acquired by a new session in
between, a versionless delete could not tell "the lease I read" from "the
lease that replaced it" — the delete would release a lease the operator never
saw. The version turns that into a 412 naming the current version, the same
answer every other stale write gets. It costs one column and the copy of the
mechanism phase 1 already built; a later reader should not have to infer the
decision from the absence of the column.

## Error shape

Every error is a single JSON object:

```json
{"error": "what went wrong"}
```

The status code carries the class — 400 bad body or value, 404 not found,
409 precondition (live or running), 412 stale version, 415 content type, 428
missing precondition, 403 cross-origin, 500 internal. A successful write is
`{"ok": true}` for a delete, and the updated resource representation for a
patch. This is the shape the settings endpoints already use; phases 3 to 5
keep it.

## Endpoints, phase by phase

### Phase 1 — sessions (built)

`GET /api/sessions`, `GET /api/sessions/{id}` unchanged, now carrying
`version` on every row.

```
PATCH /api/sessions/{id}
Content-Type: application/json
If-Match: <version>
{"status": "cancelled"}
```

- Body: `{"status": "<terminal status>"}` — one of `ok`, `failed`, `timeout`,
  `max_turns`, `cancelled`, `compacted` (every status except `running`; closing
  *into* running would be a resume, which is run control). Anything else is
  a 400 naming the accepted values; a malformed body is a 400.
- Preconditions, both inside the store transaction: version must equal
  `If-Match` (412 on mismatch, 428 if the header is missing) and, when the
  session is running, its most recent event must be older than
  `sessionIdleThreshold` (409 naming the last event's time).
- Effect on a **running** session: status set, `finished_at` set to now.
- Effect on a session **already terminal**: nothing but a version bump. Both
  the status and `finished_at` it finished with are kept, so a retried PATCH
  is idempotent and a completed run cannot be relabelled. This endpoint closes
  an abandoned run; it does not rewrite what a finished one did, and a
  finished run's status is a fact the transcript, the fold and resume all read
  as history. Correcting a genuinely wrong terminal status is a DELETE, or the
  CLI — not a silent overwrite through an endpoint that will have a button on
  it (phase 5).
- The event log is untouched — no close event is appended.
- Success: 200 with the updated session row (same shape as `GET
  /api/sessions/{id}`, including the new `version`), also fanned out to the
  `/api/stream` list feed so open session lists update.

The workflow that prompted it: a worker process died, leaving a session row
`running` forever. The operator reads the row, sees its last event is hours
old, and closes it with the PATCH above. A live run cannot be closed: its
events keep the last-event time inside the threshold, and the 409 says when the
last event was.

```
DELETE /api/sessions/{id}
Content-Type: application/json
If-Match: <version>
```

- Preconditions: version must equal `If-Match` (412/428 as above); a running
  session is refused with 409 regardless of idleness.
- Effect: the row and its whole event log are removed, atomically. The derived
  disk mirror directory is **not** removed — the HTTP server has no data-dir
  handle, the mirror is never read back, and `harness export` rebuilds it. The
  CLI's `harness delete` is the way to remove a mirror directory too.
- Success: 200 `{"ok": true}`.

Because a running session cannot be deleted, closing an abandoned session is
two calls: PATCH to a terminal status, then DELETE with the version the PATCH
response returned.

### Phase 3 — work_requests and workspace_leases (built)

```
PATCH /api/requests/{request_id}
Content-Type: application/json
If-Match: <version>
{"status": "cancelled"}
```

- Body: `{"status": "<terminal status>"}` — one of `ok`, `failed`, `denied`,
  `timeout`, `cancelled` (every status except `running`). Anything else is a
  400 naming the accepted values; a malformed body is a 400.
- Preconditions, both inside the store transaction: version must equal
  `If-Match` (412 on mismatch, 428 if the header is missing) and, when the
  request is running, its session's most recent event must be older than
  `sessionIdleThreshold` (409 naming the last event's time). A request with no
  session id has never run and passes (see [Preconditions](#preconditions-a-write-must-not-race-a-run)).
- Effect on a **running** request: status set, `finished_at` set to now.
- Effect on a request **already terminal**: nothing but a version bump — a
  retried PATCH is idempotent and a finished request cannot be relabelled,
  mirroring the session rule.
- The session row and event log are untouched.
- Success: 200 with the updated row (the same shape as `GET
  /api/requests/{request_id}`, including the new `version`).

```
DELETE /api/requests/{request_id}
Content-Type: application/json
If-Match: <version>
```

- Preconditions: version must equal `If-Match` (412/428 as above); a request
  whose session is still live is refused with 409 regardless — the row a live
  worker is using (see [Preconditions](#preconditions-a-write-must-not-race-a-run)).
- Effect: the row is removed. A dead request — one whose session is idle, or
  one that never ran — may be deleted directly; closing it with PATCH first
  remains the safe order when its message might still be redelivered.
- Success: 200 `{"ok": true}`.

`GET /api/requests` returns the whole table in `received_at` order, newest
first — the same list convention `GET /api/sessions` establishes — with every
row in the `GET /api/requests/{request_id}` shape below, `version` included.
`GET /api/requests/{request_id}` returns one row: `request_id`, `session_id`,
`status`, `result`, `received_at`, `finished_at`, `delivery_count`, `version`.
The `/status` poll snapshot is unchanged.

```
GET /api/leases
```

Returns the lease table in workspace order, each row carrying `workspace`,
`session_id`, `acquired_at`, `heartbeat_at`, `version`.

```
DELETE /api/leases/{workspace}
Content-Type: application/json
If-Match: <version>
```

- Preconditions: version must equal `If-Match` (412/428 as above); a lease
  whose `heartbeat_at` is newer than `sessionIdleThreshold` is held by a live
  session and refused with 409 naming the last heartbeat.
- Effect: the lease row is removed.
- Success: 200 `{"ok": true}`.

The lease key is a workspace path and may contain slashes; a client
percent-encodes them (`DELETE /api/leases/%2Ftmp%2Fws`).

### Phase 4 — events paging

Paging parameters and nothing else. The events resource stays read-only.

### Phase 5 — the admin UI

A browser over these endpoints. Nothing here is an endpoint.
