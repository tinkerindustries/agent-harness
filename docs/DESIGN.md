# agent-harness — technical design

A coding harness: an agent loop that reads and writes files in a workspace, runs
commands, and iterates until a task is done. Go owns the loop and the tools.

The harness runs as a service. Work requests arrive on the durable work
queue — the `work_queue` table in serve's SQLite store — execute as one of
several concurrent agent sessions inside a single Go process, and record
their result on the request's `work_requests` row (§4.10). An MCP server
exposes the same loop to another agent. A React frontend shows what the
sessions are doing and, on the settings screen, configures the harness's
keys (§4.2).

One DeepSeek behaviour drives most of the decisions below: the prompt cache is
worth 50–120× on input tokens, and hits are blocked at 128 tokens of common
prefix. Section 3.2 covers it. Section 3.1 covers reasoning replay, which the
docs present as mandatory and which measurement shows is not.

## 1. Scope

Concurrent agent sessions in one process, durable work-queue ingress and
row-backed result publication, the twenty tools in [TOOLS.md](TOOLS.md), a
declarative
per-request permission policy, flash-backed subagents via `Task`, an
append-only event log in SQLite mirrored to disk for review, cost and cache
accounting, a browser transcript with a live plan panel driven by the plan
tools (`TaskCreate`/`TaskUpdate`), session resume, and run control from the
browser — starting, steering, continuing, and stopping a run (§4.2).

Not built: auth and multi-user identity, remote or containerised workspaces,
an editor pane, FIM inline completion, prefix
completion, background shells, edit checkpointing and rollback. Each is
additive against this architecture.

Both MCP directions are now in scope, and they are unrelated to each other
beyond the name. Outward: an MCP server that lets an external agent harness
launch and collect agent-harness runs by enqueueing work requests through
the same queue `harness serve` consumes (§4.10), mounted at `/mcp` and never
touching the system prompt or the tool array the model sees. Inward: the
harness *consuming* tools from MCP servers an operator has registered, which
join its own tool array ([`MCP.md`](MCP.md)).

That inward direction was ruled out here for a long time, and the objection
was a real one worth recording rather than deleting: a set of tool
definitions that varied per request would sit in front of the frozen cached
prefix (§3.2) and cost the shared cache on every call. What makes it
tractable is that the set does not vary per request. Configuration is global
and stored, a session's array is resolved once at `Run` from a *stored*
snapshot of each server's tools and frozen on the session row, and the
snapshot is only ever rewritten by a successful probe — so an unreachable
server contributes what it contributed last time instead of silently
reshaping the head. The prefix moves when an operator changes the
configuration, which is a deliberate act, and not otherwise.

No control in the UI can approve a tool call, so approval is never a question
the loop asks a human and waits on. Section 4.6 covers what replaces it. That
holds even though the data the harness manages is writable over HTTP and the
browser can start, steer, and stop a run (§4.2): none of those give the
browser a way to answer a mid-run tool-call decision, so §4.6's policy is not
merely a workaround for a browser that cannot answer — it is also what keeps
an unattended run from stalling on a question nobody is there to answer.

## 2. Which API surface

Native OpenAI format, `https://api.deepseek.com`, `POST /chat/completions`. No
`/v1` prefix and no `/beta` unless a beta feature is actually in use — every
integration configuration DeepSeek publishes uses the plain host.

The Anthropic-format endpoint at `/anthropic` is a compatibility shim. It ignores
`cache_control`, `anthropic-beta`, `anthropic-version`, `top_k`, and
`thinking.budget_tokens`; it does not support image, document, or
`redacted_thinking` content blocks; and it cannot reach FIM, prefix completion,
or strict tool mode. `CLAUDE.md` says to exercise DeepSeek's behaviour directly,
which points at the native endpoint.

The cost of this choice is server-side web search, which DeepSeek serves only
through the Anthropic format — the OpenAI-format API accepts `type: "function"`
tools and nothing else. We build `WebFetch` ourselves instead. TOOLS.md prices
that trade-off in full.

The Anthropic endpoint was also credited with handling thinking-block replay
itself, sparing callers a documented 400. Measurement since shows that 400 does
not fire on the native endpoint either (§3.1), so the advantage is moot.

Input to DeepSeek is text only. Both models declare `input_modalities: ["text"]`, the
Anthropic table marks image and document blocks unsupported, and the Responses
API replaces image parts with placeholder text. No screenshots reach DeepSeek,
no image paste, no visual diffing inside the loop. Vision is a set of tools
instead: `Screenshot` captures a page, and `Glance`, `Ground`, `Detect`, and
`Crop` send images to Google Gemini and return a prose answer, a located pixel
box, or a local crop, so a screenshot the agent captures itself can still be
looked at ([TOOLS.md](TOOLS.md)).

## 3. The rules that shape everything

### 3.1 reasoning_content replay

The docs require that, for requests carrying `tools`, the `reasoning_content` of
every prior assistant message is passed back, and say the API returns 400
otherwise. Measurement on 2026-08-09 could not provoke that 400 on either model
in any configuration ([OBSERVED.md](OBSERVED.md)), so treat it as a strong
convention rather than an enforced constraint.

The harness replays reasoning anyway. It is what the docs prescribe, the tokens
sit inside the cached prefix so the cost is negligible, and the docs claim a
quality benefit — the model continuing its own reasoning — that the test says
nothing about.

What the finding buys is the absence of a cliff. Compaction may drop reasoning
from older turns. A session resumed from a store that lost reasoning degrades
instead of failing. No error path needs to exist for it.

The same applies to a rule that appears in no API reference: assistant messages
carrying `tool_calls` are said to need non-null `content`. Also unenforced in
testing. The fold still emits `""` rather than `null`, because matching the
shape the API itself returns costs nothing.

Reasoning is stored verbatim regardless. Display truncation happens at render
time; nothing truncated is written to the store. That was the right call when
the API demanded it and remains so now that it does not, because the store is
the only record and reasoning cannot be reconstructed.

Context grows quickly as a result. Reasoning tokens bill as output when
generated, then bill again as input on every later request in the turn — at the
cache-hit rate, so cheaply, but they still consume the window and count toward
the 768K compaction threshold.

### 3.2 The message array is append-only, or the cache dies

Cache-hit input costs $0.0028/M against $0.14/M on a miss for flash, and
$0.003625/M against $0.435/M for pro. That is 50× and 120×. Under the rates
taking effect 2026-08-16 the ratio narrows to about 31× and 30×, which changes
none of what follows.

An agent loop re-sends the whole conversation every sub-turn. A byte-stable
prefix means nearly all of it hits cache. Any change to the prefix means
everything after the change misses.

Rules that follow:

- The system prompt is fixed for the life of a harness version and shared by
  every session running against a given model. No clock, no cwd, no git status,
  no changed-file list, and nothing drawn from a work request.
- Tool definitions are fixed for the life of a session and serialised in a
  stable order. The array is per-provider — DeepSeek's nineteen tools, Kimi
  K3's fourteen without the five vision tools (docs/KIMI-INTEGRATION.md
  decision 5) — so there are two frozen heads, each shared by every session on
  its provider and each pinned by its own golden file. A session's head is
  chosen at creation from its model and never changes for the session's life.
- Volatile context goes in the newest message. It is never retrofitted into an
  older one.
- No mid-conversation compaction that rewrites history. At 768K tokens — the
  window DeepSeek's own Claude Code configuration recommends compacting at — the
  harness opens a new session seeded with a summary and links it to the parent.
  A visible session boundary, not a silent rewrite.
- Request bodies serialise from structs, not `map[string]any`, so the bytes are
  stable.

The loop's rhythm — model output, tool result appended, re-request — grows the
prefix only at the tail, which is the shape the cache rewards.

One property of the mechanism deserves stating here rather than only in the
tactics. Measured on both models, the cached length is
`floor(common_prefix_tokens / 128) × 128`. The trailing partial block never
hits, which costs under 127 tokens and does not matter. What matters is that the
formula runs on the length of the *common* prefix, so divergence near the head
truncates that length to near zero and the whole conversation misses on every
request. Cheap tail, catastrophic head.

Two rules follow that are not obvious from the invariant alone. The session
freezes its rendered system prompt and tool schema at creation, so upgrading the
harness cannot change the prefix of a resumable session. And permission modes
gate execution rather than tool availability, so the tool array never varies
within a provider — the per-provider split (DeepSeek's nineteen tools, Kimi's
fourteen) is chosen once at session creation and is part of the frozen head,
not a per-request variation.

Running many sessions at once makes the shared head worth more. Every session
sends the same rendered system prompt and the same tool array, so the first
request of the first session persists that head and every session afterwards
starts warm on it. The price is that nothing from a work request may appear in
the system prompt. Workspace path, task-specific instructions, and any result
schema go in the opening user message, where they append rather than divide.

[CACHE.md](CACHE.md) covers the tactics and the churn diagnostic.

## 4. Backend

### 4.1 Event-sourced session

A session is an append-only log of events. The log is what streams to the
browser, what persists, and what the
DeepSeek `messages` array folds from. One source of truth, four consumers.

The frontend runs its own fold over the same log. It mirrors the Go fold in
shape — pure, append-only, a switch on event kind — and not in output: one
produces a `messages` array for the API, the other produces display blocks.

Events: `session_started`, `turn_started`, `reasoning_delta`, `content_delta`,
`tool_call`, `tool_denied`, `tool_stdout`, `tool_result`, `usage`,
`turn_finished`, `run_finished`, `error`, `steer_message`, `steer_applied`.
Each carries a per-session monotonic sequence number.

The last two are the steering pair (docs/RUN-CONTROL.md "Two event kinds,
not one"): `steer_message` records that an operator sent text at this
instant and carries no messages-array content, and `steer_applied` records
that the loop folded that text into a user message at this sub-turn
boundary — linked by the applied event's `source_seq`, which is what lets a
resumed run recompute which steers are outstanding from the log alone.

There is no approval event. A permission decision resolves synchronously inside
the tool call from a policy the session already holds (§4.6), so the loop never
blocks on a human.

### 4.2 Transport

SSE down, nothing up. The browser subscribes to
`GET /api/sessions/{id}/events` and to a session-list stream, and that is the
whole *live* session surface. Traffic on it is one-way — a firehose down, no
clicks up — so SSE fits, and `Last-Event-ID` gives replay without extra
protocol. The settings endpoints below are the one place a click goes up, and
they are plain fetch calls, not SSE.

The backlog is a fetch, not a stream. Replay over SSE is correct and it is
gapless, but it arrives at the speed of a stream: a browser folds and paints
each chunk as it lands, so a long session visibly filled in over seconds
rather than appearing. A transcript screen therefore fetches
`GET /api/sessions/{id}/snapshot` — the whole log in one gzipped response,
with any image bytes detached to URLs of their own — and opens the stream at
the cursor that response ends on (`?from=<seq>`). The stream keeps the half
it is good at. The seam holds because the log is append-only with monotonic
seq: everything at or below the cursor is in the snapshot, everything above it
is in the stream's replay, and the two halves overlap rather than abut, so the
client folds by seq and drops what it already has.

The HTTP API reads and writes the data the harness manages, and it also
carries run control; it cannot reach the run loop directly. The data surface —
sessions, events, work requests, settings — is specified in
[DATA-API.md](DATA-API.md). Run control — stopping, steering, starting — is
specified in [RUN-CONTROL.md](RUN-CONTROL.md), and the seams are: stopping
goes through the `RunController` interface declared in `internal/httpapi` and
implemented by `*worker.Pool`; starting goes through the `RunPublisher`
interface, declared in `internal/httpapi` and implemented by `cmd/harness`
over the store-backed queue — so the HTTP server holds no queue handle
itself, only the narrow ability to enqueue one validated request, and work
enters through the queue whichever surface asked for it. `POST
/api/sessions/{id}/stop` and `POST /api/runs` are authenticated by the
`http.control_token` bearer token. Steering is a store write by the handler
and a store read by the loop, so `POST /api/sessions/{id}/steer` needs no seam
at all. A write that closes an abandoned session row is data; a write that
publishes a work request is run control — the run-control endpoints are the
three actions in docs/RUN-CONTROL.md, and the data write surface stays
docs/DATA-API.md.

A transcript stream carries two frames that are not committed events, both
*named* and both without an id — so neither reaches the browser's `onmessage`,
where it could be mistaken for an event, and neither can move the
`Last-Event-ID` cursor, which must only ever name a real seq or a reconnect
would skip whatever came in between. `live` is model output the backend has
not committed yet (§5.3). `replayed` is written once, after the history and
before the first live event, and marks the seam between them. The browser
needs the seam named because it cannot find it: a long history is delivered
across several reads, so the first events to arrive are a fraction of the
backlog, and treating them as the whole of it makes everything after look
newly arrived. What that distinction buys is small and worth stating plainly —
the frontend animates a row that arrived while somebody was watching and
leaves the backlog alone (§5.5).

Two questions the current design leaves open:

- **Authentication.** Loopback is the whole of the current story, and it holds
  only while the surface is one operator's own machine. An interactive site is
  the kind of thing someone exposes, and transcripts carry workspace paths,
  file contents, and command output.
- **§4.6's approval model.** It is built on the browser being unable to answer
  the loop, so a run never blocks on a human. Making the browser able to answer
  reopens that, and the answer is not "add an approve button" — a loop that
  waits on a person is a loop that stalls when nobody is watching. Any answer
  here needs a default for the unattended case.

    GET /api/sessions                    list, newest first, with status and cost
    GET /api/sessions/{id}               metadata
    GET /api/sessions/{id}/events        historical page, ?from=<seq>&limit=
    GET /api/sessions/{id}/stream        SSE, honours Last-Event-ID
    GET /api/stream                      SSE of session-level state changes
    GET /api/settings                    every setting, grouped, with its type, default, description, and the secret/restart flags
    PUT /api/settings/{key}              set a key, JSON body {"value": "..."}
    DELETE /api/settings/{key}           unset a key
    PATCH /api/sessions/{id}             close an abandoned session into a terminal status
    DELETE /api/sessions/{id}            delete a finished session and its event log

`GET` and `HEAD` are served on every path. The writing methods are allowed
where a write route exists — settings keys, and one session's row — and
nowhere else: a POST to `/api/sessions` still 405s with an `Allow` header
naming what the path actually permits, the settings collection path itself
(`/api/settings` without a key) has no write route, and the events resource
is never writable. The whole surface, including the work-request and lease
endpoints, is specified in [DATA-API.md](DATA-API.md).
Secret keys are masked in `GET /api/settings` to at most their last four
characters, and the full value never leaves the process over HTTP — there is
no reveal parameter, on this endpoint or anywhere else; a secret set once is
write-only from then on. The writing
methods demand `Content-Type: application/json` (415 otherwise) and refuse a
request whose `Origin` does not match the request's own `Host` (403), so a
page open in the operator's own browser cannot overwrite keys on the loopback
port.

**Settings are one registry.** Every setting — the API keys, the models, the
run budget, the tool limits, and the operational limits below — is one entry
in `internal/settings`' registry, carrying its type, default, validation
bounds, description, and whether it needs a restart. Validation lives in the
registry and is enforced in Go on every write path, so a direct `PUT` and the
screen reject the same values with the same message.
`GET /api/settings` serves the registry itself: each entry names its group
(the screen's heading), type, default, description, and flags, plus whether
the stored value is an override. Settings flagged "requires a restart" — the
worker pool size, the model-concurrency ceilings, the results retention, and
the events paging bounds — are read once at startup or baked into a queue
definition; the screen marks them, because a setting
that silently does nothing until an unrelated restart is worse than one that
cannot be changed at all.

Two different recoveries share one endpoint. A dropped connection is
`EventSource`'s own reconnect, which sends `Last-Event-ID` and resumes at the
next sequence number. A page reload has no memory of a sequence number and
replays the session from the start. Both end up with the same transcript and
neither can gap, because the per-session `seq` is dense and monotonic.

A session that has already finished serves its whole history and then closes
the stream. Compaction is the case worth naming: it marks a session
`compacted` without appending a terminal event to that session's own log, so
the stream ends on session status rather than on an event kind.

The run lives in Go and is driven by the queue, so closing the tab has never
had any bearing on it.

The origin and content-type guards above are what stands between a page in the
operator's browser and the write endpoints, and they are not much. Nothing here
makes the service safe to expose: transcripts carry workspace paths, file
contents, and command output, and the write surface now reaches the store.
Treat the port as sensitive and bind it to loopback by default.

It grows less defensible with every endpoint added — see above for what
authentication has to settle.

WebSocket buys nothing here; the session surface is a one-way stream and the
settings writes are ordinary fetch calls.

### 4.3 Streaming from DeepSeek

Always stream, including where incremental display is not needed. DeepSeek can
hold a request up to ten minutes before inference starts. During the wait a
streaming request emits `: keep-alive` comments and a non-streaming request emits
blank lines. Streaming distinguishes a slow queue from a dead connection.

Implementation notes:

- Read SSE with `bufio.Reader.ReadString('\n')`, or a `Scanner` with an enlarged
  buffer. A single delta line can exceed the 64KB default `Scanner` token limit.
- Drop lines starting with `:`, but let them reset the idle watchdog.
- Do not set `http.Client.Timeout`; it caps the entire stream. Put limits on the
  transport (`DialContext`, `TLSHandshakeTimeout`, `ResponseHeaderTimeout`) and
  drive cancellation from the request context plus an idle watchdog sized above
  the keep-alive interval.
- Terminate on `data: [DONE]`.
- Send `stream_options.include_usage`. Without it a streamed response carries no
  token accounting at all.

On tool-call deltas: the vendored streaming chunk schema lists only
`delta.content`, `delta.reasoning_content`, and `delta.role`, and every
tool-call sample in the docs is non-streaming. Measurement on 2026-08-09 settled
it ([OBSERVED.md](OBSERVED.md)) — DeepSeek emits OpenAI's indexed incremental
form, and `arguments` fragment mid-token. The assembler is keyed by `index` and
accumulates `id`, `name`, and `arguments`.

### 4.4 Request shape

Details that do not appear in the API reference and that a generic
OpenAI-compatible client gets wrong. All are drawn from DeepSeek's published
integration configurations; sources in [VALIDATION.md](VALIDATION.md).

- Never send `tool_choice`. Thinking mode accepts `auto` and `none` and rejects
  `required` and named-tool forcing, so no tool can be forced while thinking is
  on. `auto` is the default when tools are present, so omitting it costs
  nothing.
- Send `max_tokens`, not `max_completion_tokens`.
- Set `max_tokens` explicitly on every request. Max output is 384K and no
  default is documented for V4.
- Use the `system` role. `developer` is rejected.
- Assistant messages with `tool_calls` carry `""` rather than `null` content
  (§3.1).

### 4.5 Concurrency, scheduling, and retries

Concurrent sessions are goroutines in one process, not child processes. One
binary owns the SQLite handle, the store-backed queue, the SSE hub, and the
per-model rate limiter, and each running session is a goroutine holding only
its own state.

    harness (one process)
      work_queue table   ──▶ claim loop ──▶ worker pool, size N
      session goroutine × N   each owns: message buffer, churn state, workspace
      store writer goroutine  serialises every append to SQLite
      HTTP server + SSE hub   fan-out to browser subscribers

The session runner sits behind an interface. Moving execution to a subprocess or
another host later changes what implements that interface and leaves the loop,
the tools, and the store untouched.

Rules that concurrency imposes:

- A panic in one session must not take the process down. Each session goroutine
  recovers, writes an `error` event, publishes a failed result, and exits.
- Cancellation is a context per session, derived from the process context and
  from the request deadline. A cancelled session still writes its terminal
  events.
- Two sessions never share a workspace. A queue-driven run gets its own
  directory, named for its session id (§4.10). Concurrent `Edit` and `Bash`
  calls against one directory interleave, and neither model can see why its
  file changed under it.
- Per-session state stays per-session. The churn diagnostic's previous-request
  hashes (§4.9, [CACHE.md](CACHE.md)) are the easy thing to accidentally share.

SQLite runs in WAL mode with a busy timeout. Writes funnel through a single
writer goroutine fed by a channel, so `SQLITE_BUSY` never arises from our own
concurrency; readers use a separate read-only connection pool. Event appends are
batched per turn where they arrive faster than a transaction per event is worth.

Two deliberate exceptions to that batching, both about what a person watching a
run can see:

- **`turn_started` is committed on its own, before the request goes out.** A
  batch is stamped with one instant, so committing it with the rest gave it the
  time the model *finished* — every timestamp in a sub-turn was then off by the
  duration of the model call, and the transcript's live turn had nothing to
  measure its age from. One extra transaction per sub-turn buys a timestamp that
  means what it says.
- **Model output is streamed to the SSE hub without being stored** (`hub.Frame`,
  `hub.LiveDelta`). The sub-turn's `reasoning_delta` and `content_delta` events
  are still committed once, in the batch, when the response completes; the hub
  additionally carries the same text as it arrives, coalesced to ~10 frames a
  second, so the browser can render it live. The log's shape is unchanged and a
  reconnect rebuilds from it alone — see [DATA-API.md](DATA-API.md)'s events
  section for the frame contract and why it carries no id.

A per-model semaphore sits under the account limits: 500 concurrent for pro,
2500 for flash, counted account-wide rather than per key. The queue makes these
reachable in a way a single-user harness never did. Size the worker pool from
the semaphore rather than the other way around, and let the queue hold the
backlog.

Retry 429, 500, and 503 with exponential backoff and jitter. Do not retry 400,
401, 402, or 422 — those are bugs or an empty account, and a retry burns a turn.
Surface 402 distinctly: it means the balance is gone, not that the harness broke.
A 402 stops the pool rather than failing each queued request in turn, since
every one of them will hit the same wall.

### 4.6 Tools and permission policy

Specified in [TOOLS.md](TOOLS.md). The set is `Read`, `Write`, `Edit`, `Bash`,
`Glob`, `Grep`, `List`, `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`,
`Task`, and `WebFetch` — the vocabulary of the harnesses DeepSeek names as its
V4 agent optimisation targets — plus `Complete`, which is ours, and the vision
path — `Screenshot`, `Glance`, `Ground`, `Detect`, and `Crop` — because
DeepSeek cannot see images.

Four points from that document bear on the rest of this design:

- Schemas match the trained-in shape and are not strict-mode by default.
  Arguments are validated in Go and a bad one returns an error through the tool
  result channel for the loop to recover from.
- Tool results append in `tool_calls` array order regardless of completion
  order. DeepSeek emits parallel tool calls and they cannot be disabled, so
  ordering is what protects the prefix (§3.2).
- `Complete` carries the machine-readable result back to the requester. It
  cannot be forced, because thinking mode rejects `tool_choice: required`
  ([OBSERVED.md](OBSERVED.md)), so the harness asks for it in the system prompt
  and tolerates its absence by falling back to the final assistant text.
- Permission is a policy, not a prompt.

That last point is where this design departs from an interactive harness. The
work request must name a permission mode and may add deny patterns (§4.10). The
session holds that policy for its whole life. A tool call is evaluated against
it in Go and either runs or returns a denial through the tool result channel,
which the model reads and routes around. Every decision is synchronous, so a
session never waits on anything but the API and its own tools.

`tools.Resolver` is the seam an interactive caller would use: a resolver the
policy consults for a call it would otherwise deny, returning a synchronous
approve/deny answer instead of a denial. No caller registers one today —
every session is queue-driven and runs with `Policy.Resolver` nil — so the
seam exists in the type without being exercised. One decision point, and no
approval state in the event log.

### 4.7 Model routing and thinking settings

Specified in [MODELS.md](MODELS.md). The defaults are `deepseek-v4-pro` at
`high` effort for the main loop and `deepseek-v4-flash` for subagents and
mechanical side work. That follows DeepSeek's recommended Claude Code
configuration except on effort, where measurement put `max` at 2.1× the
wall-clock for no measured gain.

Points that bear on the rest of this design:

- Side work runs in its own conversation rather than appended to the main one,
  which keeps the main prefix stable and avoids mixing per-model caches.
- Model and effort are chosen at session creation, from the work request,
  and fixed for the session's life. Switching mid-session is a full
  cache miss, and no UI control exists to price that choice — a caller who
  wants a different model sends a different request.
- The effort mapping is not identity: `medium` and `xhigh` both run as `high`.
  Read the vendored thinking-mode guide rather than a compiled-in table.
- Thinking mode silently ignores `temperature` and `top_p`. The harness rejects
  them at request validation rather than sending values that do nothing.

### 4.8 Persistence

SQLite through `modernc.org/sqlite` — pure Go, no cgo, so the binary stays static
and cross-compiles. WAL mode, one writer goroutine (§4.5).

Tables:

    sessions        id, parent_id, model, effort, workspace, permission_mode,
                    system_prompt, tool_schema, status, created_at, finished_at,
                    job_type, parent_agent_type, parent_agent_id, parent_is_user
    events          session_id, seq, kind, payload, created_at   PK (session_id, seq)
    work_requests   request_id PK, session_id, status, result, received_at,
                    finished_at, delivery_count
    workspace_leases  workspace PK, session_id, acquired_at, heartbeat_at

`sessions` stores the rendered system prompt and tool schema frozen at creation,
so a harness upgrade cannot change the prefix of a resumable session
([CACHE.md](CACHE.md)). Resume replays the event log.

The store is the record. Every raw conversation lands there in full — reasoning
included, verbatim, never truncated (§3.1).

Disk mirror. The database is authoritative and awkward to read over someone's
shoulder, so each session also writes a directory:

    <data_dir>/sessions/<yyyy-mm-dd>/<session_id>/
      session.json      metadata, including the frozen system prompt and tools
      events.jsonl      one JSON object per event, appended in seq order
      request.json      the originating work request, when there was one

The mirror is derived, not a second source of truth. Write to SQLite inside the
transaction first, then append to disk; a failed disk write logs and does not
fail the run — the database stays complete regardless, and a mirror directory
that fell behind a crash between the two writes stays behind, with nothing
that rebuilds it after the fact.

HTTP capture. Beside the mirror, the raw wire traffic lives under

    <data_dir>/http/<yyyy-mm-dd>/<session_id>/exchanges.jsonl.gz

one gzipped JSON line per HTTP exchange. This tree is primary, not derived —
unlike the mirror above, which merely restates what the database already
holds, this is the only record of what actually crossed the wire, so there is
nothing to rebuild it from if it is lost.

The React build embeds through `embed.FS`. One binary, no runtime assets.

### 4.9 Cost accounting

Prices load from config, never from code, because they change on DeepSeek's
schedule rather than ours. That was the argument in the abstract on 2026-08-06,
when DeepSeek warned of a significant increase without naming a rate or a date.

The 2026-08-13 GA release named both. From 16:00 UTC on 2026-08-16, billing
splits into peak and off-peak, peak being 01:00-04:00 and 06:00-10:00 UTC, with
off-peak at half the peak rate. Every rate rises: pro output goes from $0.87/M
to $1.98/M off-peak and $3.96/M at peak, and cache hits rise six to twelve fold
on both models. `quick_start/pricing.md` carries both tables.

The table carries the split as a `rate_schedule` block: an `effective_at`, the
peak windows, and a peak/off-peak rate pair per model. Before `effective_at` the
flat rates apply; from it, a model the schedule names is priced by the window
its request fell in. A model in neither half — every Gemini and Kimi entry —
keeps its flat rate, because those providers do not do this.

Two decisions inside that are worth stating.

**The windows are data, not constants.** They are DeepSeek's to change, for the
same reason the rates are, and a schedule change that needed a Go release to
take effect would mean the harness billed its own figures wrong until somebody
noticed. The failure mode is quiet in a particular way: a schedule that never
matches prices every hour off-peak and simply looks like a cheap week. So the
loader validates the block rather than tolerating it — an unparseable window, a
missing half, or a model priced in one half and not the other is a load error.

**The clock is UTC and takes an instant from the caller.** UTC because that is
what DeepSeek bills on; an operator's own timezone changes nothing about what a
token costs, only which of their working hours are dear — at UTC+10 the windows
land at 11:00-14:00 and 16:00-20:00, most of a working day, and nothing on the
costing path itself ever consults local time. The instant is a
parameter rather than `time.Now()` because the two differ and the difference is
billable: a sub-turn that starts at 03:58 UTC and returns at 04:03 has left the
peak window by the time its usage is recorded. The runner passes the moment the
request was sent. A zero time is refused rather than defaulted, because it would
otherwise compare as before every `effective_at` and quietly bill the pre-split
rate for ever.

Cost is computed once, when the usage event is committed, and stored on it. That
is what makes a historical figure stable: nothing recomputes an old session at
today's rates, and the tier that applied is recorded alongside the figure
(`rate_tier`) so a dear sub-turn can be told from a badly timed one.

Track per turn and per session: cache-hit input tokens, cache-miss input tokens,
output tokens, reasoning tokens, and derived cost. A work request's result
carries the same figures, so a caller can price its own job (§4.10).

### 4.10 Work ingress and the durable work queue

Requests arrive on a durable work queue — the `work_queue` table in serve's own
SQLite file — and results live on the request's `work_requests` row. An agent
run takes minutes, so core request/reply does not fit: the requester would have
to hold a connection open for the whole run and would lose the result to any
disconnect. The queue row decouples the two sides, and the requester can collect
a result long after it stopped listening, by reading the row back over
`GET /api/requests/{request_id}` (docs/DATA-API.md).

    Table work_queue   one row per enqueued request, deleted on ack
                       claim   one UPDATE ... RETURNING over rows that are
                               visible, unleased (or lease expired), and under
                               the delivery ceiling, in (visible_at_ms, id)
                               order — FIFO redelivery
                       lease   60s (queue.LeaseDuration), extended by the
                               pool's heartbeat every 20s

The pool is the flow controller: it claims at most one row per free slot
(`Size` minus the slots in flight), so the queue never hands it more than it
can work on, and the backlog stays in the table — visible (`GET /api/queue`)
and surviving a restart in the same `harness-data` volume that holds the
sessions.

The queue is co-located with `serve`: it lives in the same SQLite file the
store writes, and the process that consumes it is the process that enqueues
through it. A second `harness serve` on another machine cannot consume it.
That is a deliberate constraint. If horizontal scaling is ever wanted, the
answer is a Postgres-backed store — the queue's contract is the claim query,
not a particular store — not re-adding a broker.

Request body:

    {
      "request_id":        "uuid",              required, the idempotency key
      "prompt":            "...",               optional; a browser start may
                                                omit it and let the operator
                                                type the first message into
                                                the session (docs/RUN-CONTROL.md)
      "repos":             [ { "url": "https://github.com/org/app.git",
                               "branch": "main" } ],   required, at least one
      "model":             "deepseek-v4-pro",   optional, config default otherwise
      "effort":            "max",               optional
      "permission_mode":   "readonly" | "full",   required
      "deny":              ["git push", "..."], optional, added to the mode's denials
      "result_schema":     { },                 optional JSON Schema for Complete
      "max_sub_turns":     400,                 optional
      "deadline_ms":       3600000              optional
      "job_type":          "implementation",    optional, implementation (default) or orchestration
      "parent_is_user":    true,                set by the producer, never by a caller
      "parent_agent_type": "claude-code",       optional, the launching agent's kind; empty when parent_is_user
      "parent_agent_id":   "abc123",            optional, the launching agent's session id, or the operator's name when parent_is_user
    }

The browser is one of two producers. `POST /api/runs` (docs/RUN-CONTROL.md)
accepts this body over HTTP — `request_id` optional there and generated when
absent, because a browser form has no idempotency key to offer — validates it
with the queue's own `Request.Validate`, and enqueues it through the
`RunPublisher` seam; a caller that supplies a `request_id` gets the same
deduplication every other producer gets. `deepseek_agent` is the other, and
both share the one marshal-and-enqueue path, `queue.Queue.Enqueue`:
`deepseek_agent` runs in the same process and enqueues directly, where the
browser's request is enqueued only once the HTTP validation above has passed
it.

The queue's old form had one ingress property HTTP does not: a NATS client in
any language could publish the same JSON body directly to the stream, without
the service being up. That is gone — `POST /api/runs` is the replacement
(HTTP, validated with the same `Request.Validate`, provenance stamped
server-side), and what is genuinely lost is publishing while the service is
down. Nothing in this repo depends on that, and `restart: unless-stopped`
covers the operational case.

What each producer stamps onto the provenance fields, because a calling agent
cannot be trusted to report its own provenance:

- `POST /api/runs` — `parent_is_user: true` plus the `identity.operator` name
  in `parent_agent_id` (an unset or malformed name degrades to an unnamed
  person, never a failed start); any provenance the body sent is overwritten
  before validation, not rejected.
- `deepseek_agent` — `parent_is_user: false`; `parent_agent_type` is
  producer-stamped from the MCP client's own `clientInfo` — the name the
  client library itself sends in the initialize handshake, normalised to the
  agentmeta grammar — and whatever kind the tool input asserted is ignored;
  `parent_agent_id` stays whatever the calling agent asserted, best-effort.

`parent_is_user` is producer-set and therefore trustworthy: the two producers
disagree by construction, `true` from the browser and `false` from MCP, and
neither reads it off the caller. `parent_agent_type` is producer-stamped on
the MCP path (from the client's `clientInfo`) and cleared entirely by
`POST /api/runs`. Only `parent_agent_id` stays caller-asserted, on the MCP
path, which is why it is the one field a caller can get wrong.

Result body:

    {
      "request_id": "...", "session_id": "...",
      "status":     "ok" | "failed" | "denied" | "timeout",
      "result":     { },        from Complete, null when it was never called
      "text":       "...",      final assistant message, always present
      "error":      { "code": "...", "message": "..." },
      "usage":      { cache hit, cache miss, output, reasoning, cost_usd,
                      price_table_date },
      "sub_turns":       n, "started_at": "...", "finished_at": "...",
      "complete_status": "done" | "gave_up" | ""
    }

The result is the row: `Pool.finish` records it with one guarded `UPDATE` on
`work_requests` and then acks — deletes the queue row — and the caller reads
it back over `GET /api/requests/{request_id}`. There is no separate
publication to subscribe to and nothing to deduplicate: the row is the single
source of the result.

Every run works in a directory of its own: the worker creates
`<workspace root>/<session id>` — with a `scratch/` subdirectory for files
that are not part of the deliverable, sibling to the clones — and clones each
entry of `repos` into it, checking out `branch` or `main`. A repository URL
must name an http(s), ssh, git, or `user@host:path` remote; `ext::` and local
paths are refused, because git treats the first as a command to run and the
second would copy the harness's own filesystem into a workspace a readonly run
can read. Two entries whose URLs end in the same name are refused rather than
one shadowing the other. Nothing is shared between runs and nothing is reused
across attempts, so a redelivery that still may run — one whose attempt died
before its session existed — clones afresh rather than inheriting a
half-finished tree.

The session row exists from before that preparation starts: the worker
creates it as `creating` (Runner.Create) with the workspace path it is about
to clone into, and `Runner.Run` promotes it to `running` once the workspace
is ready. The preparation window — repositories cloning, Node dependencies
installing, which takes minutes for a large clone — is therefore visible on
the session list's in-flight side (§5.8) and stoppable: a stop during a clone
marks the `creating` row cancelled instead of finding no row to mark, and a
preparation that fails leaves the row `failed` with an error event saying
why, rather than nothing at all.

What each terminal status means. `ok` is a run that finished on its own.
`failed` covers a validation rejection, a workspace that could not be built
(`error.code: workspace_setup`, typically a clone that was refused or a branch
that does not exist), an unrecoverable API error, and a panic. `timeout` is a
run that hit its `deadline_ms` or `max_sub_turns`. `denied` is unused: it
described a request whose workspace stayed leased to another session, which a
per-run directory makes impossible.

`complete_status` mirrors Complete's own `status` argument and is independent
of `status` above: it says how the model characterised finishing, not whether
the run finished. Empty is normal — `Complete` cannot be forced (§4.6) — and
must not be read as failure. A caller that only checks `status: "ok"` cannot
tell a finished task from one the model gave up on and reported as such;
`complete_status: "gave_up"` is that distinction.

`cancelled` is an operator's stop, not a shutdown. `POST /api/sessions/{id}/stop`
sets it (docs/RUN-CONTROL.md): a healthy run answers a cancelled context at its
next check point, a wedged one is force-finished after the grace period, and
the result carries `error.code: "cancelled"` with the operator's reason. It is
distinct from `timeout`, which is deadline-driven with no operator involved. A
graceful shutdown still drains in-flight work rather than cutting it off,
because an agent run costs minutes and a restart is not a reason to waste one.
A process that dies outright leaves its message unacked, and redelivery covers
it — for the request that never attached a session. Once a session exists, the
request is single-use and a redelivery fails it rather than re-runs it (below),
which is the price of a hard kill: graceful shutdown is the supported way out
of a run, and it never wastes one.

`result_schema` is validated in Go against the `Complete` arguments. A failing
payload returns a validation error through the tool result channel and the model
retries. The schema travels in the opening user message and never in the system
prompt or the tool definition, both of which are shared and frozen (§3.2).

Acknowledgement discipline:

- Heartbeat the lease every 20 seconds while a run holds a row, so an
  hour-long run does not trip the 60-second lease expiry.
- Publish-then-ack collapses into one write: the result *is* the row, so
  `finish`'s guarded `UPDATE` on `work_requests` is the publish, and the ack
  that follows deletes the queue row. A crash between the two redelivers the
  request once its lease expires, and the redelivery reads the now-terminal
  row back and acks without running anything — the crash window the stream
  design had to describe, with its separate result stream and its
  `Nats-Msg-Id` dedup, is closed.
- `Nats-Msg-Id` dedup is gone with the streams: `request_id` is the primary
  key, and the guarded `UPDATE ... WHERE request_id = ? AND session_id = ?`
  already prevents a stale attempt overwriting a newer result.
- Delete a malformed request's row after recording a `failed` result. It will
  never parse, and redelivering it burns the pool.
- `Nak` with a delay when the failure is transient and retries are exhausted:
  the row's `visible_at_ms` moves to now + delay and its lease is released.
  On the last delivery attempt, record `failed` and delete the row.
- The delivery ceiling bounds how many times one request may be delivered,
  from `worker.max_delivery_attempts` (default 5, restart-required). Its job
  changed with single-use requests: it is no longer the main defence against
  runaway retries — that is the session id, which stops a request that ever
  produced a session from running again — but a **backstop for requests that
  die before their session exists**. Those are the only requests redelivery
  still claims, and one of them that keeps dying during preparation (a
  workspace root that stays unwritable, say) would otherwise be redelivered
  forever, each attempt burning a pool slot. The two mechanisms are not
  redundant: the ceiling caps pre-session attempts, the session id caps
  everything after the first session. The store enforces the ceiling —
  `NakWork` deletes a row that has reached it and `ClaimWork` never returns
  one at or over it — and the pool must know the number so the final attempt
  can record a terminal result instead of the request silently ceasing to
  exist (`MaxDeliveryAttempts` on the pool equals the queue's
  `MaxDeliveries`).
- A run wedged inside a tool call is not reaped by `deadline_ms`. The run
  context carries the deadline, so a loop that checks it stops; a tool call
  blocked on something that ignores cancellation does not, and the run holds
  its slot past its deadline. The delivery ceiling bounds the damage — the
  request stops coming back — but it does not end the wedged attempt.

Idempotency is a row, not a convention. `request_id` is the primary key of
`work_requests`, and the row's `session_id` column is the discriminator that
decides whether a delivery may run:

- **No session id yet** — the attempt died in the instant after claiming the
  request, before it attached its session id. Nothing happened that matters (no
  clone, no file write, no command, no money spent), so a redelivery may claim
  the row and run afresh.
- **Session id set** — the request is **single-use**. The id is attached the
  moment the session row exists — the row is created as `creating` before the
  workspace is built, so an attempt that dies during preparation leaves a
  `creating` row behind, and the run is visible and stoppable from the moment
  it is claimed — and before the run does anything side-effecting, so a row
  that carries one is proof an attempt actually started. An agent run is not
  idempotent: it clones repositories, writes files, runs commands,
  pushes branches, opens pull requests, and spends money, and re-running one
  blind repeats all of that against a workspace and a branch that have moved
  on. The worker therefore never runs a session for such a request again,
  whatever its status. It closes the abandoned session instead — the row phase
  1's `CloseSession` was built for, and the one that until now never got
  closed, so abandoned sessions sat `running` in the list forever — publishing
  a `failed` result telling the caller the run was abandoned, that work
  requests are single-use, and that republishing under a new `request_id` is
  how to retry, and terminates the message, because no further delivery can
  help. `CloseSession` closes a `creating` row exactly like a `running` one, so
  an attempt that died mid-clone is closed the same way. The abandoned
  session's transcript still survives for review, now better than
  before: it is closed, not left `running` (or `creating`) forever.
- **Terminal row** — a finished request republishes its stored result and
  acks without running anything.

The trade-off being accepted is explicit: a hard kill mid-run **loses the run
rather than retrying it**. Graceful shutdown drains in-flight work, so this
only bites on SIGKILL, OOM, or a machine reboot — the cases where the process
is gone and cannot finish anyway. A false redelivery (a heartbeat that failed
to land while the original attempt was still alive) is guarded the same way
the operator-facing close is: the spent path passes a real idle threshold to
`CloseSession`, and a session whose most recent event is newer than it is
refused as still active, leaving the row alone and letting the message fall
through to the duplicate-handling path instead.

Progress is the SSE stream's job, not the queue's: turn-level events fan out
through the hub (§4.2), rate-limited to at most one message per second, and
full fidelity lives in the event log, on disk, and on the SSE stream. The
queue carries a request in and a terminal result out, nothing in between.

### 4.11 Skills

A repository can ship Agent Skills: a directory per skill holding a `SKILL.md`
whose YAML frontmatter carries a name and a description. `internal/skills`
scans each cloned repository at session start, under `.claude/skills/`,
`.deepcode/skills/` and `.agents/skills/` — the third being the vendor-neutral
spelling DeepSeek's own agent and Gemini's hosted environment have both
converged on — and renders the names and descriptions it finds into the opening
user message ahead of the task. The model reads a skill's body with
`Read` when it decides one applies.

Three consequences follow from §3.2. The catalogue goes in the opening message,
never the system prompt, so a repository's skills cannot disturb the cached
head. No `Skill` tool exists, because a twentieth tool definition would enlarge
that head for every session to duplicate what `Read` already does. An empty
catalogue renders to nothing, leaving the opening message byte-identical to a
run with no skills.

The missing tool has one cost, and it is paid in the browser rather than in the
cached head: with no `Skill` call and no event marking the moment, a skill being
taken looks exactly like a file being read, on one row out of the hundreds a run
produces. The session pages match each `Read` target against the catalogue's own
paths and badge the row `skill` (`web/src/api/skillCatalogue.ts`). Matching the
catalogue rather than the file name is what keeps that honest in a repository
whose subject matter is skills — a run editing `assets/skill-packs/` reads
`SKILL.md` files all day without ever taking one as instructions.

Only the description reaches the model up front, capped in length and in count,
with anything dropped stated in the catalogue rather than silently omitted.
Discovery never fails a run: an unreadable directory, a malformed `SKILL.md`,
and a skill with no description each yield no entry and no error.

A skill that instructs the agent to run a bundled script inherits the session's
permission mode. Under `readonly` that call is refused at execution. Under
`full` it runs, as any code in a cloned repository does.

**Packs are the same mechanism with the default inverted.** A pack
(`assets/skill-packs/<name>`) is a tree of skills installed by the same call,
into the same workspace directory, and discovered by the same scan — but only
into a workspace whose request named it (`queue.Request.SkillPacks`). Off is
the default on both producers: the browser's start form and the MCP launch
tool.

The reason for the inversion is the cost model above, not caution. A
description is small, but it rides in the opening message of *every* request of
the run carrying it, and the catalogue is capped — so a shipped bundle of
sixteen Unity skills would tax a run about a Go service for its whole life and
crowd out the skills the repository itself ships. Opting in is what makes a
large body of subject-specific skills shippable at all.

The reason it *can* be per-request is §3.2 again, read the other way. The
catalogue is in the opening message, so two runs whose packs differ still share
a byte-identical system prompt and tool array; only their first user message
diverges, which it already does — it carries the workspace path and the task.
This is exactly why an MCP server's tools cannot work the same way
(docs/MCP.md): those go in the frozen head, so they must be global, and a
per-run choice there would fragment the cross-session cache.

Validation of pack names lives in `internal/skills` rather than with the
embedded bytes, so `internal/queue` can reject an unknown name without
importing the assets; a test pins the two lists equal. An unknown name is
refused rather than ignored, because a typo that silently installs nothing
surfaces later as the model not knowing something, which is the hardest kind of
bug to trace back to its cause.

`session_started` carries the rendered catalogue alongside the opening message,
as an exact substring of it. That is what lets the browser lift the catalogue
into its own collapsed panel without parsing prose, and it is the one event
that yields two blocks, which is why the transcript keys blocks on sequence
number and type together.

## 5. Frontend

The browser shows a list of sessions (§5.8), the transcript of any one of them
— live or historical (§5.9, §5.10) — and the settings screen: the registry
(§4.2) rendered grouped, with each entry's default, its validation bounds,
whether the current value is a default or an override, and the restart markers.
It also controls a run (docs/RUN-CONTROL.md "The frontend"): a start form on
the session list (prompt, repos, a model, a thinking effort, and permission
mode), and a steer input and a stop control on the transcript screen, both
visible only while the session is running, with the steer's pending/delivered
states carried by the fold. There is still no approve button: a tool-call
decision is never a question the loop asks the browser (§4.6).

Stop, steer, and start are each an acceptance, not an outcome — the browser
gets back a 202 or a store write, not the result, and the SSE stream the
screens are already connected to is what says what actually happened. That
keeps the frontend out of optimistic updates, a command queue, and
reconciliation between local intent and server state:

- **No optimistic updates or reconciliation.** The settings screen re-fetches
  after every write rather than guessing at the new value, which is honest
  and cheap while a screen has one write in flight at a time. A screen with
  several concurrent writes needs more than that, and run control follows the
  same re-fetch pattern rather than spreading it.
- **No command queue.** A request that starts, steers, or stops a run is
  answered by the SSE stream, not by the response to the request.
- **The frame budget below is unaffected.** It is about rendering deltas, not
  about what the browser is allowed to send, and none of it changes.

### 5.0 What reaches the browser

The backend accumulates a sub-turn's reasoning and content in Go and commits
one `reasoning_delta` and one `content_delta` when the sub-turn ends, so the
browser receives a whole turn at once rather than text at token rate. Measured
on a four-sub-turn run, every sub-turn's events arrived in a single burst with
seven-second gaps of silence between them. `tool_stdout` is the exception and
does stream live.

Both folds accumulate deltas by concatenation and would handle genuinely
incremental events unchanged, so the gap is in `session/turn.go` alone. The
sections below are written for the streaming case, which is what the frontend
is built for and what the perf harness exercises.

### 5.1 The performance problem, stated

Two text channels arrive as deltas at token rate. A long session accumulates
hundreds of blocks, some of them large tool outputs and diffs. The naive shape —
one `setState` per delta, markdown and highlighting re-run every render —
re-parses the whole transcript tens of times a second.

Block count is not the problem. Per-token re-render of the whole tree is.

### 5.2 Streaming text stays out of React state

Deltas accumulate in a mutable buffer outside React. The live tail is the only
hot region.

- An external store holds the transcript; components subscribe with
  `useSyncExternalStore`.
- Deltas append to a plain string buffer and set a dirty flag. A
  `requestAnimationFrame` loop flushes it, so React sees at most one update per
  frame regardless of token rate.
- Completed content freezes. Top-level blocks (opening, skills, `run_finished`,
  `error`) become immutable values wrapped in `React.memo`, keyed by block id,
  and never re-render again; a sub-turn card freezes at group granularity — its
  children array is reference-stable from the moment its last tool result
  lands, and the memoised card bails out on it (§5.9).

The freeze carries most of the win. In a 142-sub-turn session, 141 cards are
inert.

### 5.3 Parse on completion, not during

A streaming block renders as plain preformatted text — no markdown parse, no
highlighting, no diff computation. On completion it parses and highlights once,
then swaps in.

Highlighting on completion, on the main thread, is adequate. A worker is the
answer only if measurement says so.

### 5.4 Diffs and large outputs

Go computes diffs and sends structured line arrays. The browser renders a table.
No diff algorithm runs in a render pass.

Tool outputs collapse by default to a head and tail preview with an expand
control. A 5,000-line file read is a disclosure problem, not a virtualisation
problem.

### 5.5 Virtualisation

Measured, and the measurement splits the question in two. The
harness lives in `web/src/perf`; run it against a synthetic feed at a fixed
rate with N blocks mounted.

Delta commits are flat. From 50 to 8000 blocks the mean stays near 0.5ms,
which is the freeze working: the hot path at token rate does not care how long
the transcript is.

Appending a block is linear in block count, and no amount of per-block
memoisation removes it — React walks every keyed child to decide each one can
bail out. At 500 blocks an append commit averaged 25ms with a 171ms worst
case, already past a 60fps budget; at 8000 it averaged 277ms.

So the original reasoning holds for the channel it was about and fails for the
other one. Virtualisation is still out for v1, and a session that grows into
the high hundreds of blocks will stutter on append. Revisit with the numbers
above rather than from first principles.

**The streaming reveal.** The live turn's prose renders one span per `live`
frame rather than one text node, so each frame can settle in where it landed
(`components/ui/StreamText.tsx`). That puts a per-flush cost on the hot path
that grows with the number of frames the current sub-turn has accumulated, so
it was measured the same way — `?live=1` interleaves frames with the committed
feed, and `?liveburst=N` pushes the chunk count past what the synthetic turn
would otherwise reach.

At 1000 blocks mounted, delta commit mean against chunks held by the live turn:

| chunks | roughly | delta mean | p95 |
| --- | --- | --- | --- |
| 5 | half a second of prose | 0.34ms | 0.5ms |
| 50 | 5s | 0.35ms | 0.5ms |
| 200 | 20s | 0.50ms | 0.7ms |
| 500 | 50s | 0.75ms | 1.7ms |
| 1000 | 100s | 1.04ms | 1.8ms |

Linear, at roughly 0.7µs per chunk, and the whole curve sits inside a 16ms
frame. The server coalesces live output to at most ten frames a second per
channel (`internal/session/turn.go` `liveFlushInterval`), so the right-hand
rows are a sub-turn that streams unbroken prose for a minute or more without
calling a tool — past anything observed. The reveal is not what will make this
screen stutter; appends still are.

### 5.6 Reasoning display

Reasoning is voluminous and mostly skimmed. It renders in a panel that expands
while streaming and collapses on completion, showing elapsed time and token
count. Display state has no bearing on storage — the full text stays in the store
because it goes back to the API.

### 5.7 Stack

Vite, React, TypeScript, shadcn/ui on Tailwind v4. The component layer is
`accordion`, `badge`, `button`, `card`, `collapsible`, `input`, `toggle`,
`toggle-group`, and `tooltip` (in `web/src/components/ui/`), themed from
the token set ported into the theme block in `web/src/styles.css`.
`ScrollArea` and `DataTable` are absent —
the rail and the plan column are plain sticky elements, and the diff table
renders inside the transcript. Everything
shadcn has no opinion about — the transcript block styles, the diff table,
and the status and diff tokens — is plain CSS in `web/src/styles.css`.
Three screens, so no router library — `App.tsx`
parses the pathname (`/`, `/sessions/:id`, `/settings`) and navigates with
`history.pushState`/`popstate`, and the static handler falls back to
`index.html` so a direct link or reload lands on the right screen — and no
data layer beyond the SSE client, the store, and the settings fetch calls
(§4.2).

### 5.8 Session list

Several sessions run at once, so the list is a first-class screen rather than a
drawer. It subscribes to `GET /api/stream`, which carries session-level state
changes only and stays quiet while transcripts are loud.

Quiet in frequency, not in volume — which is why that feed carries a
projection of a session row rather than the row. A state publish happens on
every sub-turn of every running session, and every subscriber gets a whole
row; anything on it that the list does not draw is paid for once per sub-turn
per session per open tab. So `internal/hub` has two shapes: `SessionState`,
the full row, and `ListRow`, the fields this screen renders, with `ListRowOf`
the one place they meet. Measured over a live harness the row averaged 18KB
and reached 48KB, of which 9.9KB was `recent_tool_calls` — a rolling roll of
raw tool arguments, a `Write`'s whole file body among them, that the card
stopped rendering and that this feed never redacted. The projection also caps
`task`, the other four-kilobyte field, because it is immutable and the card
shows it in a three-line clamp.

The counterpart is the session's own stream. `GET /api/sessions/{id}/stream`
now carries `state` frames — the whole `SessionState`, republished on every
change — alongside its events and live deltas, named and id-less like a live
delta so they neither reach the client's event fold nor move `Last-Event-ID`.
A caller watching one session has asked about that session and gets every
change to its row; the list is the opposite case, many sessions and a fraction
of each. Before this the detail screen re-fetched `GET /api/sessions/{id}`
only when its stream connection flipped, so its cost, cache and sub-turn
figures sat frozen for the length of a run.

The screen splits the
list in two. In-flight sessions render as
collapsible plan cards: the summary carries the job's description — the
session's `task`, clamped to three lines — and the trigger answers what the
session is doing, the `in_progress` item's activeForm, and how far in it is,
the completed ratio; expanded, it shows the whole plan and the actions row
(Stop). The summary itself opens the session page; the caret is its own small
toggle button, sibling of the summary, so toggling the plan never navigates.
A session that never wrote a plan shows a card with no plan section rather than
an empty one, and the disclosure state lives above the cards so it survives a
list update.

Finished sessions stay a dense table — status, session id with a one-line
subtitle, model, elapsed time, sub-turn count, cache-hit rate, running cost,
and the originating request id where there is one. The subtitle carries the
plan ratio ("11 of 11 plan items") and the model's own summary, so scanning the
list does not require opening each transcript. The plan is persisted with the
session — a `plan` column on the sessions table, written whenever `TaskCreate`
or `TaskUpdate` executes and carried on `hub.SessionState` — so the finished
table's ratio survives the run and the list never re-walks the event log to
derive it. The description rides the same way: a `task` column on the sessions
table, written once at creation from the run's prompt (the same value the
`session_started` payload's `Task` field carries), so the in-flight card can
say what the job is without reading the event log either. A row written before
the column existed reads back with the empty string and the card renders no
description.

### 5.9 The sub-turn is the unit

The transcript renders one card per sub-turn: reasoning,
assistant text, tool calls and their results in one body, with the
usage block absorbed into the card header instead of a fifth sibling block.
The grouping is a display-side view over the fold's `blocks` array —
`SubTurnGroupState` in `web/src/api/groups.ts` — computed incrementally in the
same style `FoldState.pushBlock` uses: append to the last group, or start a new
one on the next `assistant` block. The `Block` union and the event fold are
untouched, so `fold.ts` stays in shape agreement with `internal/fold`;
`opening`, skills, `run_finished`, and `error` stay top-level, and a starved
retry's first usage stays a loose block rather than leaking into the previous
turn's header.

The §5.2 freeze now operates at group granularity. A card cannot freeze until
its last tool result lands, so the tail group's children keep growing while its
results stream in; the moment the last one freezes, the group holds an
unchanged children array and the memoised card bails out forever. `SubTurnList`
is memoised on the items array itself, so a live-only delta never re-renders
the transcript.

Measured with `web/src/perf` on the sweep §5.5 used (50..2000 blocks at 60
events/s), before and after the grouping:

- Delta commits stay flat in group count — the same property §5.5 measured for
  blocks. Delta commit means run 0.02–0.24 ms both before and after.
- Append commits, the O(n) reconciliation walk §5.5 measured, fall because the
  walk now runs over groups instead of blocks and every earlier group bails
  out. At 2000 blocks the append mean dropped 6.66 ms -> 1.43 ms and the max
  8.9 ms -> 2.3 ms.

The transcript's browsing controls stack on top of the card.
A Compact/Full toggle collapses every card to
its header line; a card containing a failed result or a denial stays open in
both modes, because an error you have to expand to find is an error you miss.
Tool call headers are built from the call the fold already keeps in
`toolCallsById`, showing the target rather than the raw arguments JSON —
`Edit`/`Write` show the path and the `+n −n` from the diff, `Bash` the
command, `Read`/`Grep` the path or pattern, `Task` the description plus the
child session's turn count and cost. A path is shown relative to the session's
workspace root: the `…/workspaces/<session id>/` prefix is the same tens of
characters on every row of a long run — more since the root became the host's
own absolute path (docs/WORKTREES.md, "Path parity") — and it pushed the part
that differs off the end of the line. A command keeps its absolute paths, because a command is
a literal someone may want to run. The opening block collapses to one summary
line (word count and the files it names); filter chips over the cards
(All/Edits/Bash/Errors/Churn) read their counts off the same pass that builds
the groups — the chips are now the child transcript's and the perf harnesses'
only, the session pages themselves neither filter nor search a transcript,
and the watch rail's coloured squares are what index a long run instead; and a
cache-churn banner above the transcript links to the first
sub-turn whose usage carried `churn_point_index`. Density is a plain string
prop on the memoised card/list chain, so the group-level bailouts above
survive every live-only delta — toggling it is the one all-cards re-render.

Scroll height on the synthetic 142-sub-turn feed (391 blocks) that
`web/src/perf` mounts: Full 7,880 px, Compact 6,973 px — about 12% shorter in
Compact on the same feed. A real session,
`sess-f93b37beb37098b5637832e829c37d92`, measured 86,674 px across 974 block
elements; that figure uses a different corpus and measurement method, so it is
not comparable to the synthetic-feed numbers above. Collapsing a card on the
real session would hide far more text than it does on the synthetic feed, so
the real saving is likely larger, but it has not been measured directly.

### 5.10 The timeline rail

A sticky left column on the transcript screen: one entry per sub-turn — the number and one
glyph per tool call, coloured by family, a failed result or a denial
overriding to red — grouped under the plan item that was `in_progress` when
the sub-turn ran. The boundary is free: every `TaskCreate` or `TaskUpdate`
call in the event stream starts a phase, and the fold already applies those
calls, so the rail groups sub-turns without walking the session's history
itself. Each entry is an anchor to its card; one `IntersectionObserver` watches the group containers and
marks the current entry. The column is plain sticky CSS — `ScrollArea` stays
out — scrolling its own content with `overflow`.

The observer is one instance for the whole rail, and that is measured rather
than asserted: the perf harness replaces `window.IntersectionObserver` with a
counting subclass at module load, before any component mounts, so whatever the
screen constructs during the run is what gets reported.

Measured on the synthetic 142-sub-turn feed (391 blocks), in a production
build:

- IntersectionObserver instances: 1 for 142 sub-turns. A dev build reports 2 —
  StrictMode mounts, unmounts and remounts the effect.
- Rail: 8 phases, 142 entries.
- Scrolling the whole transcript: 35 frames, mean 8.13 ms, max 9.40 ms, none
  over 16.7 ms, 15 marker updates.
- Commit cost (dev build — React Profiler's `onRender` is a no-op in a
  production build): scroll 17 commits at mean 1.059 ms / max 1.500 ms; live
  append 4 commits at mean 0.550 ms / max 1.300 ms.
- Compact-mode scroll height on the same feed: 7,357 px.

### 5.11 Measured, end to end

The bundle embedded in the Go binary:

    CSS   86.70 kB (gzip  16.78 kB)
    JS   584.88 kB (gzip 177.48 kB)

The frontend test suite has 259 tests. The per-feature numbers live with their
features: the sub-turn grouping and the density toggle in §5.9, the timeline
rail in §5.10. Every scroll-height figure in §5 was measured on the synthetic
feed that `web/src/perf` mounts; the real session
`sess-f93b37beb37098b5637832e829c37d92` measured 86,674 px across 974 block
elements, a figure that is not comparable to the synthetic-feed numbers (§5.9).
