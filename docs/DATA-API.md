# The data write API

This document is the surface for every resource the harness manages: what
each endpoint does, and the invariants every write carries.

Two distinctions bracket what belongs here:

- **Data, not run control.** A write that changes a row the harness manages —
  closing an abandoned session, deleting a finished one, closing a dead work
  request — is data and belongs in this API. A write that *starts, steers, or
  stops a run* is run control and lives in [RUN-CONTROL.md](RUN-CONTROL.md),
  whose seams are chosen: the HTTP server holds no queue handle and
  `internal/httpapi` imports neither `internal/session` nor
  `internal/worker` ([ARCHITECTURE.md](../ARCHITECTURE.md)). Three
  run-control actions exist: `POST /api/runs`, which enqueues a validated
  work request through the declared `RunPublisher`
  interface (implemented by `cmd/harness` over the queue's own handle);
  `POST /api/sessions/{id}/stop`, authenticated by the `http.control_token`
  bearer token and acting on a run through the declared `RunController`
  interface; and `POST /api/sessions/{id}/steer`, the same guards but no seam
  at all — it is a store write the loop reads at its next sub-turn boundary.
  That import boundary is what keeps run control a declared seam rather than
  a reach into a running loop.
- **The store is the only record.** The event log is append-only and never
  editable; see [Events are not writable](#events-are-not-writable).

## Resource model

Five resources. One shape of write everywhere: a guard trio (content type,
origin, and the idle precondition where the row a run may be using), optimistic
concurrency on the row, and a JSON error body.

| Resource | Endpoint | Read | Write |
| --- | --- | --- | --- |
| sessions | `/api/sessions` | GET (list), GET `/api/sessions/{id}` | PATCH `/api/sessions/{id}`, DELETE `/api/sessions/{id}` |
| events | `/api/sessions/{id}/events` | GET (paged, `?from=&limit=`) | **none** |
| work_requests | `/api/requests` | GET (list), GET `/api/requests/{request_id}` (row), GET `/api/requests/{request_id}/status` (poll snapshot) | PATCH, DELETE `/api/requests/{request_id}` |
| workspace_leases | `/api/leases` | GET (list) | DELETE `/api/leases/{workspace}` |
| settings | `/api/settings` | GET `/api/settings` | PUT, DELETE `/api/settings/{key}` |
| mcp_servers | `/api/mcp/servers` | GET (list) | POST `/api/mcp/servers`, PATCH/DELETE `/api/mcp/servers/{name}`, POST `/api/mcp/servers/{name}/refresh` |
| pricing | `/api/pricing` | GET (the rate schedule, no rates) | **none** |

### sessions

A session is the frozen metadata row for one agent run — model, effort,
workspace, permission mode, the job's description (`task`, the launching
instruction written once at creation), the rendered system prompt and tool
schema, status, `created_at`, `finished_at` — plus its event log (the fold,
the transcript, and resume all read it as an append-only log;
[DESIGN.md §4.1](DESIGN.md#41-event-sourced-session)).
A row also carries the provenance triple — `job_type`, `parent_agent_type`,
`parent_agent_id`, and `parent_is_user` (producer-stamped: true when a person
started the run, false on a pre-migration row).

The session representation also carries four fields the main page renders in
place of the raw prompt: `title`, the run's name shown bold, at most **10
words**; `description`, what change the agent is making, shown under the
title, at most **50 words**; and `phase` / `total_phases`, this run's 1-based
position in a multi-phase chain (the `deepseek-flash-plan` skill cuts a job
into phases) — both zero, or absent, means the run is not part of a chain.
All four are optional on the wire, and `task` stays exactly as it is —
still stored, still on the wire, still shown on the session page; it just
stops being what the main page's description column renders. A row with no
title (a pre-migration row, or a browser start that left the fields blank)
renders the raw prompt as the description line. The MCP launch path
(`deepseek_agent`) requires `title` and `description` — a launch without
them is refused at the tool — while the browser path (`POST /api/runs`) and
`harness publish` treat both as optional; the queue enforces only the word
caps and the phase relationship (`agentmeta.ValidateTitle`,
`ValidateDescription`, `ValidatePhase`), never presence.

- `GET /api/sessions` — one page of the list, newest first (`created_at DESC`),
  each row with status, cost, and the originating request id, wrapped in the
  [Pagination envelope](#pagination) — the response is the envelope even when
  the caller asked for no page. Four query parameters:
  `?status=` (`running` — the live set, running plus `creating`, the rows the
  in-flight list shows — or `finished`, everything not live; an unknown
  value is a 400 naming the valid ones, the same refusal `?kind=` gives),
  `?q=` (a case-insensitive substring of the session id, workspace, or work
  request id), `?limit=` and `?offset=` (clamped to the endpoint's defaults,
  never a 400).
- `GET /api/sessions/{id}` — one row, the same shape as a list row.
- `GET /api/sessions/{id}/screenshot?path=<path>` — one image file from that
  session's workspace, so the transcript can render what a `Screenshot`,
  `Glance`, `Ground`, or `Detect` call was looking at
  ([TOOLS.md, "Seeing the screenshots"](TOOLS.md)). Not a resource of its own
  and not a general file read: the path must resolve inside that session's
  workspace and carry a PNG, JPEG, or WebP extension, and the image is read
  live, so a session whose workspace has been cleaned up returns 404.

A session row's representation carries `version` (see
[Optimistic concurrency](#optimistic-concurrency)). The write endpoints change
status and timestamps only; the rendered system prompt, tool schema, and event
log are never editable over HTTP.

```
PATCH /api/sessions/{id}
Content-Type: application/json
If-Match: <version>
{"status": "cancelled"}
```

- Body: `{"status": "<terminal status>"}` — one of `ok`, `failed`, `timeout`,
  `max_turns`, `cancelled`, `compacted` (every status except `running` and
  `creating`; closing *into* a live status would be a resume, which is run
  control). Anything else is a 400 naming the accepted values; a malformed
  body is a 400.
- Preconditions, both inside the store transaction: version must equal
  `If-Match` (412 on mismatch, 428 if the header is missing) and, when the
  session is live (`running`, or `creating` while a worker is still preparing
  its workspace), its most recent event must be older than
  `sessionIdleThreshold` (409 naming the last event's time).
- Effect on a **live** session — `running`, or `creating` (a row a dead
  worker left mid-clone closes exactly like an abandoned running one): status
  set, `finished_at` set to now.
- Effect on a session **already terminal**: nothing but a version bump. Both
  the status and `finished_at` it finished with are kept, so a retried PATCH
  is idempotent and a completed run cannot be relabelled. This endpoint closes
  an abandoned run; it does not rewrite what a finished one did, and a
  finished run's status is a fact the transcript, the fold and resume all read
  as history. Correcting a genuinely wrong terminal status is a DELETE, or the
  CLI — not a silent overwrite.
- The event log is untouched — no close event is appended.
- Success: 200 with the updated session row (same shape as `GET
  /api/sessions/{id}`, including the new `version`), also fanned out to the
  `/api/stream` list feed so open session lists update.

The workflow that prompted it: a worker process died, leaving a session row
`running` forever — or `creating`, when it died mid-clone, which the same
close now handles. The operator reads the row, sees its last event is hours
old (a `creating` row has no events at all), and closes it with the PATCH
above. A live run cannot be closed: its
events keep the last-event time inside the threshold, and the 409 says when the
last event was.

```
DELETE /api/sessions/{id}
Content-Type: application/json
If-Match: <version>
```

- Preconditions: version must equal `If-Match` (412/428 as above); a live
  session — `running`, or `creating` while a worker is cloning into that
  directory — is refused with 409 regardless of idleness.
- Effect: the row and its whole event log are removed, atomically. The derived
  disk mirror directory is **not** removed — the HTTP server has no data-dir
  handle, the mirror is never read back, and `harness export` rebuilds it. The
  CLI's `harness delete` is the way to remove a mirror directory too.
- Success: 200 `{"ok": true}`.

Because a live session cannot be deleted, closing an abandoned session is
two calls: PATCH to a terminal status, then DELETE with the version the PATCH
response returned.

### events

The event log is read-only over HTTP, forever — see
[Events are not writable](#events-are-not-writable). It supports paging and
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

Plus the SSE transcript stream, which carries three kinds of frame:

- **Committed events**, as `id: <seq>` plus `data:` — the resumable log. A
  client resumes with `Last-Event-ID` and the server replays from there.
- **Live deltas**, as `event: live` plus `data:`, with **no id**. These are
  model output the backend has not committed yet: the same text arrives again,
  moments later, as ordinary `reasoning_delta` and `content_delta` events in the
  sub-turn's commit batch.

  ```
  event: live
  data: {"sub_turn": 4, "channel": "reasoning", "text": "weighing it"}
  ```

  Both halves of that shape are load-bearing. The **name** keeps it off the
  browser's default `onmessage` handler, which folds committed events — a client
  that folded both into one field would double the text of every streamed
  sub-turn. The **missing id** keeps it out of `Last-Event-ID`, which must only
  ever name a committed seq, or a reconnect would skip whatever was committed in
  between. A client that ignores `event: live` entirely is correct and loses
  nothing durable.

  Deltas are coalesced to about ten frames a second rather than one per token,
  because a lagging subscriber is dropped rather than blocking the session
  goroutine. They exist because a sub-turn's events commit in one batch when the
  response completes, so `turn_started` and `turn_finished` reach a browser in
  the same instant and the transcript's live states were otherwise unreachable
  on a real run.

- **State frames**, as `event: state` plus `data:`, with **no id**. One
  session's whole metadata row — the same JSON `GET /api/sessions/{id}`
  returns — republished whenever the row changes: every sub-turn, and every
  write of the plan. A client watching one session gets every change to its
  row here rather than by polling the REST endpoint.

  ```
  event: state
  data: {"id": "sess-…", "status": "running", "sub_turns": 4, "usage": {…}, …}
  ```

  Named and id-less for the same two reasons a live delta is: the name keeps
  it off `onmessage`, which folds committed events and would choke on a row,
  and the missing id keeps it out of `Last-Event-ID`, which may only ever name
  a committed seq. A state frame is a full replacement, never a patch, and a
  client that ignores it entirely is correct — it just has to re-fetch the row
  to notice a change.

  The last row of a run may not arrive: the terminal state is published after
  the `run_finished` event that tells a client to close the stream. A client
  that wants it re-fetches `GET /api/sessions/{id}` when the connection ends,
  which is what the frontend does.

The list feed, `GET /api/stream`, is the counterpart and deliberately not the
same shape. It sends `hub.ListRow` — the projection of a session row down to
what the session list screen renders — as a plain `data:` frame with no id and
no event name, both for the connect snapshot (one frame per session) and for
every update after it. `recent_tool_calls`, `summary`, `permission_mode`,
`parent_id`, `version`, `price_table_date` and the cache-token counts are not
on it, and `task` is capped to `hub.MaxListTaskChars` with a trailing ellipsis
where it was cut. The reason is rate: this feed re-sends a whole row on every
sub-turn of every running session to every open list, so a field nobody draws
costs the same as one that is. A caller that wants the whole row asks for it —
`GET /api/sessions`, `GET /api/sessions/{id}`, or the `state` frames above.

**Every one of these transports redacts credential shapes on the way out**
(`internal/redact`, applied in `internal/httpapi`) — the paged events
resource, the transcript stream's events and live deltas, and the two feeds
carrying session rows, which reach the same browser and carry the prompt a run
was launched with. A session's tool output is whatever its commands
printed, and an agent that needs a token in its container will read one back —
a run once put a full `github_pat_` value in a tool result with `head -2
.env`. Values matching a recognisable credential format — GitHub, GitLab,
`sk-`, Google, Slack, AWS, npm tokens and PEM private key blocks — are replaced
with `[redacted]`; the surrounding output is untouched.

Three boundaries this draws:

- **Not on the write path.** The event log keeps the literal bytes. The fold
  rebuilds the model's own conversation from the same log, and a model that
  cannot see what its command actually printed cannot verify the file it just
  wrote.
- **Not in the mirror.** `workspaces/<id>/events.jsonl` keeps the literal bytes
  too. It sits beside the workspace holding the `.env` the token came from, so
  redacting one while the other is readable is false comfort. What is different
  about HTTP is that it is *reachable*.
- **Not a guarantee.** Shape matching catches tokens with a distinctive prefix.
  A password or a bare hex key passes straight through. This raises the cost of
  a leak; it does not close it, and it is not a substitute for the
  authentication [RUN-CONTROL.md](RUN-CONTROL.md) names as the open question for
  a surface anyone exposes beyond loopback.

### attachments

A work request may carry images — a mockup the task asks the agent to match,
say. The bytes live in the `attachments` table, never inline in the work
request: the store is the authority the disk mirror derives from, so
`harness export` stays complete, and a request never carries bytes that
could blow its size. The request carries only `attachment_ids`; the worker
reads the rows back and the workspace materialises them into
`scratch/attachments/`, which the run's opening message names.

The two producers accept them in the same shape — `POST /api/runs` (a
top-level `attachments` array beside the work-request fields) and the MCP
`deepseek_agent` tool:

```json
{"name": "mockup.png", "mime_type": "image/png", "data": "<base64>"}
```

Each attachment is validated before it is stored: the name must be a plain
file name ending in `.png`, `.jpg`, `.jpeg`, or `.webp` — the three types the
vision tools accept, because the model's use of the file is passing it to
`Glance`, `Ground`, or `Detect` — the MIME type, when supplied, must match the
extension, and the decoded bytes must be within the per-file cap. The count
and the per-file bytes are settings, not fixed values
(`tools.attachments_max_count`, `tools.attachments_max_bytes`, both under
`Tool limits` with the same defaults shape as the
`tools.reviewscreenshot_max_*` pair). A failing attachment is a 400 (or a
tool error from `deepseek_agent`) naming the problem, and nothing is
published; a request that passes validation is already complete, because the
bytes were stored before the publish.

### work_requests

A work request is the idempotency row for one queued job: request id, the
session that ran it, status, result JSON, `received_at`, `finished_at`,
delivery count. It is **single-use**: the row's `session_id` is attached the
moment an attempt's session row exists, and once it is set the request never
runs again, whatever its status ([DESIGN.md §4.10](DESIGN.md#410-work-ingress-and-the-durable-work-queue)).
A request is retried by republishing it under a new `request_id`, never by
re-running the old one.

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

`PATCH`/`DELETE` close and remove a dead request row (a request left `running`
by a dead worker still holds its spent row and needs the same kind of operator
close sessions do).

### workspace_leases

A lease is a workspace held by a session: `workspace`, `session_id`,
`acquired_at`, `heartbeat_at`.

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

The lease key is a workspace path, so it can contain slashes; a client
percent-encodes them (`DELETE /api/leases/%2Ftmp%2Fws`).

### settings

The pattern the other resources follow: the write guards, the JSON error
body, `{"ok": true}` on success. Settings has no version column — it is a
key/value table whose writes are last-write-wins by design — so it does not
carry the concurrency mechanism; every *row* resource does.

### MCP servers

`/api/mcp/servers` is the registry an operator edits to give every session
after this moment a new set of tools ([MCP.md](MCP.md)). It is one table,
`mcp_servers`, and unlike settings it carries child data — the last
successful probe's tool list — alongside the operator's own configuration,
so a row is bigger than a key/value pair and the write shapes differ from
`/api/settings` in the ways below.

- **`GET /api/mcp/servers`** — every configured server, name-ascending,
  masked (below). Always a JSON array, even with nothing configured — never
  `null`.
- **`POST /api/mcp/servers`** — create. The body is the writable subset:
  `name`, `transport` (`"stdio"` or `"http"`), `command`, `args`, `env`,
  `url`, `headers`, `enabled` (defaults to `true` when absent), and
  `allow_readonly` (defaults to `false`). A shape failure — a bad name, a
  stdio server carrying a `url`, an http server carrying a `command` — is a
  **400** carrying `store.ValidateMCPServer`'s message; a name already
  registered is a **409**. On success the row is created and, when the
  harness has an MCP client manager wired, probed once before the response
  is sent — but **a failed probe is not a failed create**: the row exists
  either way, the failure lands on the row's own `probe_error`, and the
  response is **201** carrying whichever the freshest read of the row is.
- **`PATCH /api/mcp/servers/{name}`** — partial update. Every writable field
  is optional; a field absent from the body leaves the stored value alone,
  which is what lets an operator flip `allow_readonly` without resending
  `command`, `args`, and every other field verbatim. **404** when `name` is
  unknown, **400** on a shape failure the merged row fails
  `store.ValidateMCPServer` on. A body that touches only `enabled` is the
  toggle path and never disturbs anything else on the row, connection
  details included. A re-probe follows the write only when a connection
  field changed (`transport`, `command`, `args`, `env`, `url`, or `headers`)
  or the server was just enabled — toggling `allow_readonly`, or disabling a
  server, must not restart a subprocess or reopen an HTTP connection for no
  reason.
- **`DELETE /api/mcp/servers/{name}`** — **204** with no body on success,
  **404** when `name` is unknown.
- **`POST /api/mcp/servers/{name}/refresh`** — probe now, the button beside
  a row on the screen. **503** when the harness has no MCP client manager
  wired at all (the same fail-closed shape the missing run publisher gives
  `POST /api/runs`), **404** when `name` is unknown. Otherwise **200** with
  the row — including when the probe itself failed: `probe_error` is what
  the screen shows, and a probe failure is never turned into a 5xx here.

**Secrets.** `env` and `headers` values can hold API keys, so `GET
/api/mcp/servers` masks every value the same way the settings endpoints mask
a secret (`redact.Secret` — keys stay in the clear, values are masked to at
most their last four characters). A write reverses that asymmetrically
rather than symmetrically: `env` and `headers` in a `PATCH` body describe the
server's *complete* desired key set, not a diff, except that **a key sent
with an empty string keeps whatever value is already stored for that key**.
That one exception is the whole point — it is the only way an operator can
edit a server's command or arguments without re-typing every API key `GET`
only ever showed them masked. A key that is simply absent from the body is
removed. `POST /api/mcp/servers` (create) carries no such exception: there is
nothing stored yet, so every value in the body is taken literally.

### github repos

`GET /api/github/repos` — the operator's GitHub repositories, newest-updated
first, for the start-run form's repo picker. It is not a store resource: the
list is fetched live from the GitHub REST API (`/user/repos?sort=updated`,
paginated via `Link: rel="next"`, capped at five pages) using the
`github.token` setting's personal access token, and it is read-only like every
GET here — no write guards, because there is nothing to guard.

- No `github.token` set: `200 {"repos": [], "configured": false}`. The
  unconfigured state is an expected answer, not an error, so the form can tell
  "add a token in Settings" apart from "token set but GitHub failed".
- Token set and the fetch succeeds: `200 {"repos": [{"full_name", "clone_url",
  "default_branch", "private", "updated_at"}], "configured": true}`. GitHub
  already returns the list ordered by `updated_at` desc, so the order is
  preserved field-for-field.
- Token set but GitHub refuses or is unreachable (bad/expired token, rate
  limit, network error): `502 {"error": "<readable message>"}` naming the
  GitHub side of the failure.
- A successful fetch is cached in memory for 60 seconds, so reopening the
  start-run dialog does not re-hit GitHub's rate-limited API on every open.

### models

`GET /api/models` — the model names the harness knows, straight from
`internal/provider`'s table — the same list that validates a work request
(`internal/queue`) and routes a run to its client. The start-run and eval
forms' model dropdowns are fed from here, so a model added to the table shows
up in the browser without a frontend change; a hardcoded copy in the frontend
would be a second list to update, the same drift the system prompt's tool
inventory was bitten by once (`TestPromptNamesExactlyTheToolArray`). It is a
read of a compiled-in table — no store, no seams, no write guards, exactly
like the GitHub repo list above.

- `200 {"models": ["deepseek-v4-flash", "deepseek-v4-pro", "kimi-k3"]}` — the
  names in `provider.KnownModels()`, sorted. Nothing here can fail, so this
  is the only answer.
- The browser falls back to the `model.default` and `model.flash` settings
  rows when this endpoint is unreachable, so a failed fetch degrades to the
  configured defaults rather than an empty dropdown.

## Pagination

Every paginated list endpoint returns the same envelope — one shape, one
contract — whether or not the caller asked for a page:

```json
{"items": [...], "total": 137, "limit": 20, "offset": 40, "has_more": true, "next": 60}
```

- `items` — the rows of this page, in the endpoint's own order (`created_at
  DESC` for the session list). Always an array, even when empty.
- `total` — the number of rows matching the filter, ignoring `limit` and
  `offset`; the count a client shows ("137 sessions") and pages against.
- `limit` — the limit actually applied, after clamping; `offset` — the
  offset actually applied, after clamping. Both are echoed back so a client
  never has to guess what the server did with its request.
- `has_more` — whether more rows follow this page (`offset + len(items) <
  total`). `next` is present only when `has_more` is true and is the
  `offset` to ask for the next page with.

The paging parameters clamp rather than 400, the same forgiving shape the
events endpoint's `?limit=` uses: a `limit` that is absent, non-numeric,
`<= 0`, or over the endpoint's max falls back to the default, and a
negative `offset` clamps to 0. An unknown *status* or *kind* filter value
is still a 400 — the clamp is for windowing arithmetic, never for a filter
name the caller misspelled. A paginated list endpoint always returns the
envelope, even when the caller asked for no page at all: there is no
"bare array" form of a paginated list for a client to accidentally code
against.

The events endpoint's pager is deliberately *not* this one: `from`/`next`
there are sequence numbers over an append-only log, and `has_more` is
decided by peeking one row past the page — a cursor pager for a different
shape of data (a log a page can end exactly on), not an offset pager over
a filtered table.

## Events are not writable

The event log is the store's only record and reasoning cannot be reconstructed
([DESIGN.md §3.1](DESIGN.md#31-reasoning_content-replay)). The fold builds the
DeepSeek `messages` array from it, the transcript and the SSE stream replay it,
and resume continues from it — all of them read the log as an append-only
sequence with a per-session monotonic `seq`. An edit or a delete would silently
rewrite history every consumer trusts.

This is a *different* invariant from the general rule that the HTTP surface
writes only through the endpoints above: that rule bounds where writes can
happen; this one says a specific resource — events — is never writable by any
of them. The two session write endpoints change the session row and leave the
log alone.

An operator note, if one is ever wanted, is a **new event appended** — never an
edit of an existing one. There is no endpoint to append one by hand; if that
day comes it is a store-level append exactly like the runner's, with the same
`seq` assignment and hub fan-out, not a new privilege on the events resource.

## The guards every write carries

Every write endpoint — settings, sessions, work requests, and leases — runs
the same two guards before touching anything:

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
and the write surface reaches the store. Authentication is the open
question the interactive frontend forces — [DESIGN.md §4.2](DESIGN.md#42-transport)
names it — and nothing in this API pretends to be a substitute. Treat the port
as sensitive and keep it loopback-bound.

## Preconditions: a write must not race a run

A write that targets a row a run may be using must **fail rather than race**.
Two rules, both surfaced as **409 Conflict**:

- **PATCH /api/sessions/{id} on a session that is still live.** Status alone is
  not enough to tell "abandoned" from "mid-run" — the whole point of the
  endpoint is that an abandoned row still says `running`. Idleness is the
  test: a live run appends events continuously, so a session whose most
  recent event is newer than the idle threshold is presumed live and the
  close is refused. The threshold is `sessionIdleThreshold` in
  `internal/httpapi` — ten minutes, a named constant with a comment, never a
  literal — and the refusal is a 409 whose message says when the last event
  was. A session with no events has nothing recent and passes: a row that has
  never appended has no demonstrated liveness to protect.
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

The two new row resources, work_requests and workspace_leases, reuse the same
rule and the same status code, each judged by the signal its row already
carries rather than a new one:

- **PATCH and DELETE /api/requests/{request_id} on a request in flight.** The
  request's `session_id` is the signal. A running request whose session's most
  recent event is newer than the idle threshold is being run by a live pool
  worker right now — it will publish its own terminal result — and both the
  close and the delete are refused with a 409 naming the session's last
  event's time, exactly as the session endpoints name it. A request with **no
  session id at all has never run** — the single-use guard only fires once an
  attempt's session row exists — so it has no demonstrated liveness to protect
  and is closable and deletable. This is intentional, not an omission: the
  alternative (refuse everything sessionless) would strand a request whose
  worker died during workspace preparation, before its session existed,
  forever. The narrow race it accepts — closing a request in the
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
from the session DELETE in one respect: it does not refuse a `running` row
that is merely abandoned. The session DELETE refuses running regardless of
idleness because a live session goroutine may still be appending to the
session row at any instant; a dead request's row is written only by the
worker's finish path, which the operator has already proven quiet, so an
abandoned request row can be deleted directly. The idempotency caveat is the
same one session deletes carry — deleting the row of a request whose message
is still being redelivered lets a later delivery claim it fresh, so the
PATCH-then-DELETE sequence remains the safe order — but the endpoint does not
force it, and the spec's own words ("refuse with 409 when the request row a
live worker may be using") name only the live case.

## Optimistic concurrency

Every row a mutating endpoint targets — sessions, work_requests,
workspace_leases — carries a monotonic `version` integer, starting at 1 and
incremented by 1 on every successful mutation of that row. The API returns it
in the resource representation and in the response to every mutating write. A
mutating write must echo it back in an **`If-Match`** header: `If-Match:
<version>`. A missing header is **428 Precondition Required**; a header that
does not match the row's current version is **412 Precondition Failed** with
a message naming the current version.

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
  each resource a different mechanism to copy instead of one.

For a session, the version is checked **inside the store write transaction**
alongside the precondition, so the two together are atomic with respect to
every other writer. The runner's own terminal write bumps the version, which is
exactly the protection the close endpoints need: an operator who read the
session before the run finished gets a 412, not a write that lands on a row
that already changed.

work_requests and workspace_leases carry the column with the same semantics:
version 1 at creation, `+1` per mutation, `If-Match` required on mutating
writes, 428 when absent, 412 when stale.

**Run control is the exception** (docs/RUN-CONTROL.md "The HTTP surface"):
`POST /api/sessions/{id}/stop` and `POST /api/sessions/{id}/steer` require no
`If-Match`. That rule exists so an operator's write cannot land on a row that
changed since they read it — a stop or a steer is an action on a run, not an
edit of a row, and a running session's `version` changes continuously
underneath the caller as the runner commits. Requiring a version echo would
make a correct stop racy by construction and push the operator to
fetch-then-immediately-post, which is the check without the protection. The
preconditions that matter there are about the *run*, not the row: the session
exists (404), and it is running (409 naming the session's status).

Why a **lease** carries a version when the only mutation this API performs on
it is a delete: the version is what makes the delete's `If-Match` mean
something. The lease is a row a run acquires and releases; if the operator
reads the table and the lease is released and re-acquired by a new session in
between, a versionless delete could not tell "the lease I read" from "the
lease that replaced it" — the delete would release a lease the operator never
saw. The version turns that into a 412 naming the current version, the same
answer every other stale write gets. It costs one column and the same
mechanism every other row resource already carries; a later reader should not
have to infer the decision from the absence of the column.

## Error shape

Every error is a single JSON object:

```json
{"error": "what went wrong"}
```

The status code carries the class — 400 bad body or value, 404 not found,
409 precondition (live or running), 412 stale version, 415 content type, 428
missing precondition, 403 cross-origin, 500 internal. A successful write is
`{"ok": true}` for a delete, and the updated resource representation for a
patch. This is the shape the settings endpoints established and every other
resource keeps.

### pricing

`GET /api/pricing` answers one question: when are the expensive hours, and
from when. It returns the price table's capture date and — once the table has
one — its rate schedule: the `effective_at` instant, the peak windows in UTC,
and the models the schedule prices by the hour. Nothing is writable.

**It carries no rates, deliberately.** Every cost figure the UI shows was
computed on the server when its usage event was committed and stored on that
event ([DESIGN.md §4.9](DESIGN.md#49-cost-accounting)), which is what keeps a
historical figure stable when prices move. A browser holding a rate card could
only ever use it to compute a second, disagreeing number, so it does not get
one. What it gets instead is the schedule, because that is a fact about the
future rather than about a run that already happened.

The windows go out in UTC, as DeepSeek states them and bills on them, and the
browser renders them in its own zone (`web/src/api/pricing.ts`). That split is
the point: the server has no idea where the person reading is sitting and the
browser knows exactly. At UTC+10 the two windows read 11:00-14:00 and
16:00-20:00 — most of a working day, which is a fact worth showing and one the
server cannot produce.

A harness with no price table, or a table with no schedule, answers 200 with
the schedule absent rather than an error: a screen with nothing to say about
peak hours says nothing, which is also the correct display for every table
before 2026-08-16.
