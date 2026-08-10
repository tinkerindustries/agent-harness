# deepseek-harness — technical design

A coding harness: an agent loop that reads and writes files in a workspace, runs
commands, and iterates until a task is done. Go owns the loop and the tools.

The harness runs as a service. Work requests arrive on a NATS JetStream queue,
execute as one of several concurrent agent sessions inside a single Go process,
and return a result to a JetStream results stream (§4.10). A CLI drives the same
loop for interactive use. A React frontend shows what the sessions are doing
and, on the settings screen, configures the harness's keys (§4.2).

One DeepSeek behaviour drives most of the decisions below: the prompt cache is
worth 50–120× on input tokens, and hits are blocked at 128 tokens of common
prefix. Section 3.2 covers it. Section 3.1 covers reasoning replay, which the
docs present as mandatory and which measurement shows is not.

## 1. Scope

In scope for v1: concurrent agent sessions in one process, NATS JetStream
ingress and result publication, the twelve tools in [TOOLS.md](TOOLS.md), a
declarative per-request permission policy, flash-backed subagents via `Task`, an
append-only event log in SQLite mirrored to disk for review, cost and cache
accounting, a read-only browser transcript with a live plan panel driven by
`TodoWrite`, and session resume.

Out of scope for v1: any write path from the browser, auth and multi-user
identity, remote or containerised workspaces, an editor pane, FIM inline
completion, MCP tools inside the agent loop, prefix completion, background
shells, edit checkpointing and rollback. Each is additive against this
architecture.

"MCP tools inside the agent loop" names one direction specifically: the
harness *consuming* MCP tools as part of its own DeepSeek tool array, which
would put a variable, request-dependent set of tool definitions in front of
the frozen cached prefix (§3.2). The other direction is in scope and shipped:
an MCP server that lets an external agent harness launch and collect
deepseek-harness runs by publishing to the WORK stream below. It is a
separate process (`harness mcp`) that never touches the system prompt or the
tool array DeepSeek sees.

The browser's inability to control a run is the constraint with the widest
reach. No control in the UI can approve a tool call — the run surface is
read-only (§4.2), settings being the one write and not a run control — so
approval cannot be a question the loop asks a human and waits on. Section 4.6
covers what replaces it.

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
no image paste, no visual diffing inside the loop. Vision is a tool instead:
`ReviewScreenshot` sends the agent's screenshots to Google Gemini and returns
the findings, so a screenshot the agent captures itself can still be reviewed
([TOOLS.md](TOOLS.md)).

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
DeepSeek `messages` array folds from. One source of truth, four consumers.

The frontend runs its own fold over the same log. It mirrors the Go fold in
shape — pure, append-only, a switch on event kind — and not in output: one
produces a `messages` array for the API, the other produces display blocks.

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
whole session surface. Traffic on it is one-way — a firehose down, no clicks up —
so SSE fits, and `Last-Event-ID` gives replay without extra protocol. The
settings endpoints below are the one place a click goes up, and they are plain
fetch calls, not SSE.

The v1 HTTP API is read-only with respect to runs: no endpoint starts, steers,
or stops a run, and nothing a browser does can reach the loop. Work enters over
NATS or the CLI. Settings are the single exception — an operator configures the
harness's keys from the same browser surface — and the run surface stays
untouchable from a browser.

    GET /api/sessions                    list, newest first, with status and cost
    GET /api/sessions/{id}               metadata
    GET /api/sessions/{id}/events        historical page, ?from=<seq>&limit=
    GET /api/sessions/{id}/stream        SSE, honours Last-Event-ID
    GET /api/stream                      SSE of session-level state changes
    GET /api/settings                    every setting, grouped, with its type, default, description, and the secret/restart flags
    PUT /api/settings/{key}              set a key, JSON body {"value": "..."}
    DELETE /api/settings/{key}           unset a key

`GET` and `HEAD` are served on every path. The writing methods are allowed on
the settings endpoints only: a POST to `/api/sessions` still 405s with an
`Allow` header naming what the path actually permits, and the settings
collection path itself (`/api/settings` without a key) has no write route.
Secret keys are masked in `GET /api/settings` to at most their last four
characters, exactly as `harness config list` masks them, and the full value
never leaves the process over HTTP — there is no reveal parameter. The writing
methods demand `Content-Type: application/json` (415 otherwise) and refuse a
request whose `Origin` does not match the request's own `Host` (403), so a
page open in the operator's own browser cannot overwrite keys on the loopback
port.

**Settings are one registry.** Every setting — the API keys, the models, the
run budget, the tool limits, and the operational limits below — is one entry
in `internal/settings`' registry, carrying its type, default, validation
bounds, description, and whether it needs a restart. Validation lives in the
registry and is enforced in Go on every write path, so `harness config set`,
an HTTP `PUT`, and the screen reject the same values with the same message.
`GET /api/settings` serves the registry itself: each entry names its group
(the screen's heading), type, default, description, and flags, plus whether
the stored value is an override. Settings flagged "requires a restart" — the
worker pool size, the model-concurrency ceilings, the results retention, and
the events paging bounds — are read once at startup or baked into the
JetStream stream; the CLI and the screen both mark them, because a setting
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

The run lives in Go and is driven by NATS, so closing the tab has never had any
bearing on it.

Read-only-with-respect-to-runs shrinks the CSRF and command-injection surface,
and the settings writes carry the origin and content-type guards above, but
none of that makes the service safe to expose. Transcripts carry workspace
paths, file contents, and command output. Treat the port as sensitive and bind
it to loopback by default.

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
`Complete`, which is ours, and `ReviewScreenshot`, which sends screenshots to
Gemini because DeepSeek cannot see images.

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
  cache miss, and no UI control exists to price that choice — a caller who
  wants a different model sends a different request.
- The effort mapping is not identity and pro is due to change during August
  2026. Read the vendored thinking-mode guide rather than a compiled-in table.
- Thinking mode silently ignores `temperature` and `top_p`. The harness rejects
  them at request validation rather than sending values that do nothing.

### 4.8 Persistence

SQLite through `modernc.org/sqlite` — pure Go, no cgo, so the binary stays static
and cross-compiles. WAL mode, one writer goroutine (§4.5).

Tables:

    sessions        id, parent_id, model, effort, workspace, permission_mode,
                    system_prompt, tool_schema, status, created_at, finished_at,
                    job_type, parent_agent_type, parent_agent_id
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
fail the run. `harness export` rebuilds any session's directory from the
database, which is also the repair path after a crash between the two writes.

HTTP capture. Beside the mirror, the raw wire traffic lives under

    <data_dir>/http/<yyyy-mm-dd>/<session_id>/exchanges.jsonl.gz

one gzipped JSON line per HTTP exchange. This tree is primary, not derived:
nothing rebuilds it, `harness export` does not produce it, and it is the only
record of what actually crossed the wire.

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
      "request_id":        "uuid",              required, the idempotency key
      "prompt":            "...",               required
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
      "parent_agent_type": "claude-code",       optional, the launching agent's kind, or "user"
      "parent_agent_id":   "abc123",            optional, the launching agent's session id
    }

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

Every run works in a directory of its own: the worker creates
`<workspace root>/<session id>` and clones each entry of `repos` into it,
checking out `branch` or `main`. A repository URL must name an http(s), ssh,
git, or `user@host:path` remote; `ext::` and local paths are refused, because
git treats the first as a command to run and the second would copy the
harness's own filesystem into a workspace a readonly run can read. Two entries
whose URLs end in the same name are refused rather than one shadowing the
other. Nothing is shared between runs and nothing is reused across attempts, so
a redelivery clones afresh rather than inheriting a half-finished tree.

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

There is no `cancelled`. Nothing can cancel a run: the browser cannot steer
the loop (§4.2) and a graceful shutdown drains in-flight work rather than
cutting it off, because an agent run costs minutes and a restart is not a
reason to waste one. A process that dies outright leaves its message unacked,
and redelivery covers it.

`result_schema` is validated in Go against the `Complete` arguments. A failing
payload returns a validation error through the tool result channel and the model
retries. The schema travels in the opening user message and never in the system
prompt or the tool definition, both of which are shared and frozen (§3.2).

Acknowledgement discipline:

- Heartbeat `InProgress` every 20 seconds while a run holds a message, so an
  hour-long run does not trip the 60-second `AckWait`.
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

### 4.11 Skills

A repository can ship Agent Skills: a directory per skill holding a `SKILL.md`
whose YAML frontmatter carries a name and a description. `internal/skills`
scans each cloned repository at session start, under `.claude/skills/` and
`.deepcode/skills/`, and renders the names and descriptions it finds into the
opening user message ahead of the task. The model reads a skill's body with
`Read` when it decides one applies.

Three consequences follow from §3.2. The catalogue goes in the opening message,
never the system prompt, so a repository's skills cannot disturb the cached
head. No `Skill` tool exists, because a thirteenth tool definition would enlarge
that head for every session to duplicate what `Read` already does. An empty
catalogue renders to nothing, leaving the opening message byte-identical to a
run with no skills.

Only the description reaches the model up front, capped in length and in count,
with anything dropped stated in the catalogue rather than silently omitted.
Discovery never fails a run: an unreadable directory, a malformed `SKILL.md`,
and a skill with no description each yield no entry and no error.

A skill that instructs the agent to run a bundled script inherits the session's
permission mode. Under `readonly` that call is refused at execution. Under
`full` it runs, as any code in a cloned repository does.

`session_started` carries the rendered catalogue alongside the opening message,
as an exact substring of it. That is what lets the browser lift the catalogue
into its own collapsed panel without parsing prose, and it is the one event
that yields two blocks, which is why the transcript keys blocks on sequence
number and type together.

## 5. Frontend

The browser observes and, where runs are concerned, does not act — its one
write is the settings screen, and that cannot reach a run. It has no prompt
box, no approve button, and no cancel control, and the server would reject
them anyway (§4.2). What it shows is a list of sessions, the transcript of any
one of them — live or historical — and the settings screen for the harness's
settings: the registry (§4.2) rendered grouped and typed, with each entry's
default, whether the current value is a default or an override, and the
restart markers.

That subtraction removes most of the usual frontend work — no optimistic
updates, no command queue, no reconciliation between local intent and server
state. What remains is the hard part, which is rendering two high-rate text
channels without dropping frames.

### 5.0 What actually reaches the browser today

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

### 5.6 Reasoning display

Reasoning is voluminous and mostly skimmed. It renders in a panel that expands
while streaming and collapses on completion, showing elapsed time and token
count. Display state has no bearing on storage — the full text stays in the store
because it goes back to the API.

### 5.7 Stack

Vite, React, TypeScript, shadcn/ui on Tailwind v4. The component layer is
`badge`, `button`, `card`, `collapsible`, `input`, `toggle`,
`toggle-group`, and `tooltip` (in `web/src/components/ui/`), themed from
`design/tokens.css` with the variables ported into the theme block in
`web/src/styles.css`. `ScrollArea` and `DataTable` are deliberately absent —
the rail and the plan column are plain sticky elements, and the diff table
renders inside the transcript (docs/WEB-REDESIGN.md phase 1). Everything
shadcn has no opinion about — the transcript block styles, the diff table,
and the status and diff tokens — is plain CSS in `web/src/styles.css`.
Three screens and one write path, so no router library — `App.tsx`
parses the pathname (`/`, `/sessions/:id`, `/settings`) and navigates with
`history.pushState`/`popstate`, and the static handler falls back to
`index.html` so a direct link or reload lands on the right screen — and no
data layer beyond the SSE client, the store, and the settings fetch calls
(§4.2).

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
