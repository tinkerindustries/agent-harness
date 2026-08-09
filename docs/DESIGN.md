# deepseek-harness — technical design

A coding harness: an agent loop that reads and writes files in a workspace, runs
commands, and iterates until a task is done. Go owns the loop and the tools.

The harness runs as a service. Work requests arrive on a NATS JetStream queue,
execute as one of several concurrent agent sessions inside a single Go process,
and return a result to a JetStream results stream (§4.10). A CLI drives the same
loop for interactive use. A read-only React frontend shows what the sessions are
doing.

One DeepSeek behaviour drives most of the decisions below: the prompt cache is
worth 50–120× on input tokens, and hits are blocked at 128 tokens of common
prefix. Section 3.2 covers it. Section 3.1 covers reasoning replay, which the
docs present as mandatory and which measurement shows is not.

## 1. Scope

In scope for v1: concurrent agent sessions in one process, NATS JetStream
ingress and result publication, the eleven tools in [TOOLS.md](TOOLS.md), a
declarative per-request permission policy, flash-backed subagents via `Task`, an
append-only event log in SQLite mirrored to disk for review, cost and cache
accounting, a read-only browser transcript with a live plan panel driven by
`TodoWrite`, and session resume.

Out of scope for v1: any write path from the browser, auth and multi-user
identity, remote or containerised workspaces, an editor pane, FIM inline
completion, MCP, prefix completion, background shells, edit checkpointing and
rollback. Each is additive against this architecture.

The read-only browser is the constraint with the widest reach. No control in the
UI can approve a tool call, so approval cannot be a question the loop asks a
human and waits on. Section 4.6 covers what replaces it.

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

Input is text only. Both models declare `input_modalities: ["text"]`, the
Anthropic table marks image and document blocks unsupported, and the Responses
API replaces image parts with placeholder text. No screenshots, no image paste,
no visual diffing.

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
$0.003625/M against $0.435/M for pro. That is 50× and 120×.

An agent loop re-sends the whole conversation every sub-turn. A byte-stable
prefix means nearly all of it hits cache. Any change to the prefix means
everything after the change misses.

Rules that follow:

- The system prompt is fixed for the life of a harness version and shared by
  every session running against a given model. No clock, no cwd, no git status,
  no changed-file list, and nothing drawn from a work request.
- Tool definitions are fixed for the life of a session and serialised in a stable
  order.
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
gate execution rather than tool availability, so the tool array never varies.

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
browser, what persists, what a NATS progress message summarises, and what the
DeepSeek `messages` array folds from. One source of truth, four consumers. The
frontend folds the same log into its view model.

Events: `session_started`, `turn_started`, `reasoning_delta`, `content_delta`,
`tool_call`, `tool_denied`, `tool_stdout`, `tool_result`, `usage`,
`turn_finished`, `run_finished`, `error`. Each carries a per-session monotonic
sequence number.

There is no approval event. A permission decision resolves synchronously inside
the tool call from a policy the session already holds (§4.6), so the loop never
blocks on a human.

### 4.2 Transport

SSE down, nothing up. The browser subscribes to
`GET /api/sessions/{id}/events` and to a session-list stream, and that is the
whole surface. Traffic is one-way — a firehose down, no clicks up — so SSE fits,
and `Last-Event-ID` gives replay without extra protocol.

The v1 HTTP API is read-only in the strict sense: it serves `GET` and `HEAD`,
and every other method returns 405. Nothing a browser does can start, steer, or
stop a run. Work enters over NATS or the CLI.

    GET /api/sessions                    list, newest first, with status and cost
    GET /api/sessions/{id}               metadata
    GET /api/sessions/{id}/events        historical page, ?from=<seq>&limit=
    GET /api/sessions/{id}/stream        SSE, honours Last-Event-ID
    GET /api/stream                      SSE of session-level state changes

A reload mid-run reconnects and replays from the last sequence number. The run
lives in Go and is driven by NATS, so closing the tab has never had any bearing
on it.

Read-only removes CSRF and command-injection surface, and it does not make the
service safe to expose. Transcripts carry workspace paths, file contents, and
command output. Treat the port as sensitive and bind it to loopback by default.

WebSocket buys nothing here; the client never sends at all.

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
binary owns the SQLite handle, the NATS connection, the SSE hub, and the
per-model rate limiter, and each running session is a goroutine holding only its
own state.

    harness (one process)
      NATS pull consumer  ──▶ dispatcher ──▶ worker pool, size N
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
- Two sessions never share a workspace. The store holds a workspace lease keyed
  by resolved absolute path; a request for a leased workspace waits or fails
  fast, chosen per request. Concurrent `Edit` and `Bash` calls against one
  directory interleave, and neither model can see why its file changed under it.
- Per-session state stays per-session. The churn diagnostic's previous-request
  hashes (§4.9, [CACHE.md](CACHE.md)) are the easy thing to accidentally share.

SQLite runs in WAL mode with a busy timeout. Writes funnel through a single
writer goroutine fed by a channel, so `SQLITE_BUSY` never arises from our own
concurrency; readers use a separate read-only connection pool. Event appends are
batched per turn where they arrive faster than a transaction per event is worth.

A per-model semaphore sits under the account limits: 500 concurrent for pro,
2500 for flash, counted account-wide rather than per key. The queue makes these
reachable in a way a single-user harness never did. Size the worker pool from
the semaphore rather than the other way around, and let JetStream hold the
backlog.

Retry 429, 500, and 503 with exponential backoff and jitter. Do not retry 400,
401, 402, or 422 — those are bugs or an empty account, and a retry burns a turn.
Surface 402 distinctly: it means the balance is gone, not that the harness broke.
A 402 stops the pool rather than failing each queued request in turn, since
every one of them will hit the same wall.

### 4.6 Tools and permission policy

Specified in [TOOLS.md](TOOLS.md). The set is `Read`, `Write`, `Edit`, `Bash`,
`Glob`, `Grep`, `List`, `TodoWrite`, `Task`, and `WebFetch` — the vocabulary of
the harnesses DeepSeek names as its V4 agent optimisation targets — plus
`Complete`, which is ours.

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
work request names a permission mode and may add deny patterns (§4.10). The
session holds that policy for its whole life. A tool call is evaluated against
it in Go and either runs or returns a denial through the tool result channel,
which the model reads and routes around. Every decision is synchronous, so a
session never waits on anything but the API and its own tools.

The CLI is the one interactive caller, and it plugs a terminal prompt into the
same seam by registering a resolver the policy consults for calls it would
otherwise deny. Queue-driven sessions register no resolver. One decision point,
two callers, and no approval state in the event log.

### 4.7 Model routing and thinking settings

Specified in [MODELS.md](MODELS.md). The defaults are `deepseek-v4-pro` at
`high` effort for the main loop and `deepseek-v4-flash` for subagents and
mechanical side work. That follows DeepSeek's recommended Claude Code
configuration except on effort, where measurement put `max` at 2.1× the
wall-clock for no measured gain.

Points that bear on the rest of this design:

- Side work runs in its own conversation rather than appended to the main one,
  which keeps the main prefix stable and avoids mixing per-model caches.
- Model and effort are chosen at session creation, from the work request or the
  CLI flags, and fixed for the session's life. Switching mid-session is a full
  cache miss, and with a read-only UI there is nobody to price that choice for.
  A caller who wants a different model sends a different request.
- The effort mapping is not identity and pro is due to change during August
  2026. Read the vendored thinking-mode guide rather than a compiled-in table.
- Thinking mode silently ignores `temperature` and `top_p`. The harness rejects
  them at request validation rather than sending values that do nothing.

### 4.8 Persistence

SQLite through `modernc.org/sqlite` — pure Go, no cgo, so the binary stays static
and cross-compiles. WAL mode, one writer goroutine (§4.5).

Tables:

    sessions        id, parent_id, model, effort, workspace, permission_mode,
                    system_prompt, tool_schema, status, created_at, finished_at
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
      transcript.md     rendered for reading, rewritten at turn boundaries
      request.json      the originating work request, when there was one

The mirror is derived, not a second source of truth. Write to SQLite inside the
transaction first, then append to disk; a failed disk write logs and does not
fail the run. `harness export` rebuilds any session's directory from the
database, which is also the repair path after a crash between the two writes.

The React build embeds through `embed.FS`. One binary, no runtime assets.

### 4.9 Cost accounting

Prices load from config, never from code. The pricing page carries an explicit
notice that rates are about to rise significantly, so a compiled-in table goes
stale on their schedule rather than ours.

That rise stopped being hypothetical on 2026-08-06, when DeepSeek formally
warned of a significant API price increase without naming a rate or a date.
Rates verified against the live pricing page on 2026-08-09 still match the
figures in MODELS.md, so the change had not landed as of then.

The price table therefore carries its own capture date, and the cost readout
shows it. A cost figure computed from a stale table is worse than no figure,
because it looks authoritative.

Track per turn and per session: cache-hit input tokens, cache-miss input tokens,
output tokens, reasoning tokens, and derived cost. A work request's result
carries the same figures, so a caller can price its own job (§4.10).

### 4.10 Work ingress over NATS JetStream

Requests arrive on a JetStream work queue and results go to a separate stream.
An agent run takes minutes, so core request/reply does not fit: the requester
would have to hold a connection open for the whole run and would lose the result
to any disconnect. Two streams decouple the two sides, and the requester can
collect a result long after it stopped listening.

    Stream WORK      subjects harness.work.request.*
                     retention WorkQueue
                     consumer  durable pull, AckExplicit,
                               AckWait 60s, MaxAckPending = pool size

    Stream RESULTS   subjects harness.work.result.>
                     retention Limits, MaxAge 7d
                     harness.work.result.<request_id>.accepted
                     harness.work.result.<request_id>.progress
                     harness.work.result.<request_id>.final

`MaxAckPending` set to the worker pool size makes JetStream the flow controller.
The harness pulls only what it can run and the backlog stays in the stream,
where it is visible and survives a restart.

`docker-compose.yml` runs the local server: `nats:2.10-alpine` with `--jetstream`
and a named volume for the store. Host ports come from the environment, because
one NATS on the default ports is a normal thing for a machine to already have.
The harness declares both streams and the consumer at startup and treats an
existing definition as satisfied, so an empty server converges rather than
needing a setup script.

Request body:

    {
      "request_id":      "uuid",              required, the idempotency key
      "prompt":          "...",               required
      "workspace":       "/abs/path",         required, must sit under a configured root
      "model":           "deepseek-v4-pro",   optional, config default otherwise
      "effort":          "max",               optional
      "permission_mode": "readonly" | "default" | "full",
      "deny":            ["git push", "..."], optional, added to the mode's denials
      "result_schema":   { },                 optional JSON Schema for Complete
      "max_sub_turns":   100,                 optional
      "deadline_ms":     1800000              optional
    }

Result body:

    {
      "request_id": "...", "session_id": "...",
      "status":     "ok" | "failed" | "denied" | "timeout" | "cancelled",
      "result":     { },        from Complete, null when it was never called
      "text":       "...",      final assistant message, always present
      "error":      { "code": "...", "message": "..." },
      "usage":      { cache hit, cache miss, output, reasoning, cost_usd,
                      price_table_date },
      "sub_turns":  n, "started_at": "...", "finished_at": "..."
    }

`result_schema` is validated in Go against the `Complete` arguments. A failing
payload returns a validation error through the tool result channel and the model
retries. The schema travels in the opening user message and never in the system
prompt or the tool definition, both of which are shared and frozen (§3.2).

Acknowledgement discipline:

- Heartbeat `InProgress` every 20 seconds while a run holds a message, so a
  30-minute run does not trip the 60-second `AckWait`.
- Publish the terminal result, then ack. Doing it in that order means a crash in
  between redelivers the request rather than losing it.
- Publish `final` with `Nats-Msg-Id` set to `<request_id>.final`, so the
  redelivered publish deduplicates inside the stream's window instead of
  producing a second result.
- `Term` a malformed request after publishing a `failed` result. It will never
  parse, and redelivering it burns the pool.
- `Nak` with a delay when the failure is transient and retries are exhausted.
  On the last delivery attempt, publish `failed` and `Term`.

Idempotency is a row, not a convention. `request_id` is the primary key of
`work_requests`. A redelivery whose row is terminal republishes the stored
result and acks without running anything. A row left `running` by a process that
died is taken over as a fresh session linked to the abandoned one, so the
transcript of the failed attempt survives for review.

Progress messages are turn-level, never token-level. Publishing content deltas
to JetStream would persist thousands of messages per run for no reader's
benefit. `progress` carries `turn_started`, `tool_call`, a truncated
`tool_result`, and `usage`, rate-limited to at most one message per second. Full
fidelity lives in the event log, on disk, and on the SSE stream.

## 5. Frontend

The browser observes and does not act. It has no prompt box, no approve button,
and no cancel control, and the server would reject them anyway (§4.2). What it
shows is a list of sessions and the transcript of any one of them, live or
historical.

That subtraction removes most of the usual frontend work — no optimistic
updates, no command queue, no reconciliation between local intent and server
state. What remains is the hard part, which is rendering two high-rate text
channels without dropping frames.

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
- Completed blocks freeze. They become immutable values wrapped in `React.memo`,
  keyed by block id, and never re-render again.

The freeze carries most of the win. In a 400-block session, 399 blocks are inert.

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

Not in v1. Frozen memoised blocks plus clamped output handle realistic session
sizes. Virtualisation interacts badly with variable heights, streaming growth,
and stick-to-bottom scrolling, and it costs more than it returns at these block
counts. Phase 5 of the plan measures; the measurement decides.

### 5.6 Reasoning display

Reasoning is voluminous and mostly skimmed. It renders in a panel that expands
while streaming and collapses on completion, showing elapsed time and token
count. Display state has no bearing on storage — the full text stays in the store
because it goes back to the API.

### 5.7 Stack

Vite, React, TypeScript. No component framework. Plain CSS with custom
properties. Two screens and no write path, so no router library and no data
layer beyond the SSE client and the store.

### 5.8 Session list

Several sessions run at once, so the list is a first-class screen rather than a
drawer. Each row shows status, model, workspace, elapsed time, sub-turn count,
running cost, and the originating request id where there is one. It subscribes
to `GET /api/stream`, which carries session-level state changes only and stays
quiet while transcripts are loud.

A denied tool call renders in the transcript as its own block, showing the call
and the policy that refused it. Denials are the main thing an operator wants to
find after a queue-driven run does less than expected, so they are not folded
into generic tool results.
