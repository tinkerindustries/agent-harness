# agent-harness — technical design

A coding harness: an agent loop that reads and writes files in a workspace, runs
commands, and iterates until a task is done. Go owns the loop and the tools.

The harness runs as one process hosting one session for a parent
application, over stdin and stdout. The parent spawns it, owns the working
directory, supplies the credentials, and reads the session's whole event
stream back (§4.2, §4.10).

One DeepSeek behaviour drives most of the decisions below: the prompt cache is
worth 50–120× on input tokens, and hits are blocked at 128 tokens of common
prefix. Section 3.2 covers it. Section 3.1 covers reasoning replay, which the
docs present as mandatory and which measurement shows is not.

## 1. Scope

One coding session hosted for a parent process over stdin and stdout, the
twenty tools in [TOOLS.md](TOOLS.md), a declarative per-request permission
policy, flash-backed subagents via `Task`, an append-only event log in SQLite
mirrored to disk for review, cost and cache accounting, session resume, and
run control from the parent — starting, steering, continuing, and stopping a
run (§4.2).

Not built: a server of any kind, a work queue, a worker pool, a browser
surface, auth and multi-user identity, remote or containerised workspaces,
an editor pane, FIM inline completion, prefix completion, background shells,
edit checkpointing and rollback. Each is additive against this architecture.

MCP is in scope in one direction: the harness *consuming* tools from MCP
servers an operator has registered, which join its own tool array
([`MCP.md`](MCP.md)).

That was ruled out here for a long time, and the objection was a real one
worth recording rather than deleting: a set of tool definitions that varied
per request would sit in front of the frozen cached prefix (§3.2) and cost
the shared cache on every call. What makes it tractable is that the set does
not vary per request. Configuration is global and stored, a session's array
is resolved once at `Run` from a *stored* snapshot of each server's tools and
frozen on the session row, and the snapshot is only ever rewritten by a
successful probe — so an unreachable server contributes what it contributed
last time instead of silently reshaping the head. The prefix moves when an
operator changes the configuration, which is a deliberate act, and not
otherwise.

Nothing can approve a tool call, so approval is never a question the loop
asks a human and waits on. Section 4.6 covers what replaces it. That holds
even though the parent can start, steer and stop a run (§4.2): none of those
give it a way to answer a mid-run tool-call decision, so §4.6's policy is not
merely a workaround — it is what keeps an unattended run from stalling on a
question nobody is there to answer.

## 2. Which API surface

Native OpenAI format, `https://api.deepseek.com`, `POST /chat/completions`. No
`/v1` prefix and no `/beta` unless a beta feature is actually in use — every
integration configuration DeepSeek publishes uses the plain host.

The Anthropic-format endpoint at `/anthropic` is a compatibility shim. It ignores
`cache_control`, `anthropic-beta`, `anthropic-version`, `top_k`, and
`thinking.budget_tokens`; it does not support document or `redacted_thinking`
content blocks (image blocks were added to its compatibility table in the
2026-08-27 mirror refresh — [DEEPSEEK-VISION.md](DEEPSEEK-VISION.md) §8); and
it cannot reach FIM, prefix completion, or strict tool mode. `CLAUDE.md` says
to exercise DeepSeek's behaviour directly, which points at the native
endpoint.

The cost of this choice is server-side web search, which DeepSeek serves only
through the Anthropic format — the OpenAI-format API accepts `type: "function"`
tools and nothing else. We build `WebFetch` ourselves instead. TOOLS.md prices
that trade-off in full.

The Anthropic endpoint was also credited with handling thinking-block replay
itself, sparing callers a documented 400. Measurement since shows that 400 does
not fire on the native endpoint either (§3.1), so the advantage is moot.

Input is text only for one of DeepSeek's two models. The Codex model
catalogue (`third_party/deepseek-docs/quick_start/agent_integrations/codex.md`)
declares `input_modalities: ["text"]` for `deepseek-v4-pro`, and
`input_modalities: ["text", "image"]` for `deepseek-flash` — the same
catalogue, not two separate sources. For the text-only model, the Responses
API also replaces image parts with placeholder text. No screenshots reach
`deepseek-v4-pro`, no image paste, no visual diffing inside the loop.
`deepseek-flash` is the exception: it reads images directly, at the
resolution and token cost [DEEPSEEK-VISION.md](DEEPSEEK-VISION.md) records,
through the same tool-message image shape Kimi and Gemini use. For the model
that cannot see images, vision is a set of tools instead:
`Screenshot` captures a page, and `Glance`, `Ground`, `Detect`, and `Crop`
send images to Google Gemini and return a prose answer, a located pixel box,
or a local crop, so a screenshot the agent captures itself can still be
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
  no changed-file list, and nothing drawn from a create body.
- Tool definitions are fixed for the life of a session and serialised in a
  stable order. The array is chosen per model, through `provider.SeesImages`
  — every vision-capable model (Kimi K3, Gemini, and `deepseek-flash`, the
  only DeepSeek model this harness routes) gets fourteen tools without the
  six vision ones; every other model, including one `provider.ModelFor`
  rejects, gets the twenty-tool default (docs/KIMI-INTEGRATION.md decision
  5, docs/DEEPSEEK-VISION.md) — so there are two frozen heads, each shared
  by every session that resolves to it and each pinned by its own golden
  file. A session's head is chosen at creation from its model and never
  changes for the session's life.
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
within a session — the per-model split (twenty tools for DeepSeek's
non-vision pair, fourteen for every vision-capable model) is chosen once at
session creation and is part of the frozen head, not a per-request
variation.

Running many sessions at once makes the shared head worth more. Every session
sends the same rendered system prompt and the same tool array, so the first
request of the first session persists that head and every session afterwards
starts warm on it. The price is that nothing from a create body may appear in
the system prompt. Workspace path, task-specific instructions, and any result
schema go in the opening user message, where they append rather than divide.

[CACHE.md](CACHE.md) covers the tactics and the churn diagnostic.

## 4. Backend

### 4.1 Event-sourced session

A session is an append-only log of events. The log is what streams to the
parent, what persists, and what the DeepSeek `messages` array folds from. One
source of truth, three consumers.

Events: `session_started`, `turn_started`, `reasoning_delta`, `content_delta`,
`tool_call`, `tool_denied`, `tool_stdout`, `tool_result`, `usage`,
`turn_finished`, `run_finished`, `error`, `steer_message`, `steer_applied`,
`steer_withdrawn`. Each carries a per-session monotonic sequence number.

The last three are the steering events (docs/RUN-CONTROL.md "Two event kinds,
not one"): `steer_message` records that an operator sent text at this
instant and carries no messages-array content, and `steer_applied` records
that the loop folded that text into a user message at this sub-turn
boundary — linked by the applied event's `source_seq`, which is what lets a
resumed run recompute which steers are outstanding from the log alone.
`steer_withdrawn` closes a steer the run ended without applying, by the same
`source_seq`, so no later run applies it (docs/RUN-CONTROL.md "A steer the
run never reached").

There is no approval event. A permission decision resolves synchronously inside
the tool call from a policy the session already holds (§4.6), so the loop never
blocks on a human.

### 4.2 Transport

Frames down the pipe, frames up it. The parent writes JSON-RPC requests on
this process's stdin and reads notifications and responses on its stdout.
The vocabulary is the OpenAI Responses API's — its REST methods on `POST
/responses`, and that surface's semantic server-sent events as notifications
— rather than one of this repo's invention, so one set of shapes runs from
the parent through the loop to the provider. [STDIO-PROTOCOL.md](STDIO-PROTOCOL.md)
is the wire reference and the contract with a process this repo does not
contain.

Nothing but protocol frames may reach stdout. A stray line there is an
unparseable frame to the parent and there is no recovering from it, so the
process pins Go's logger to stderr and reads no `.env` of its own.

Run control rides the same pipe. Starting is a create call; continuing names
the response it follows; steering is a store write the loop reads at its next
sub-turn boundary; stopping cancels the run's context in this process.
[RUN-CONTROL.md](RUN-CONTROL.md) specifies the three that touch a running
run. None of them needs a seam into the loop: the store is the boundary for
steering, and the other two are the process's own context.

A transcript carries two kinds of frame that are not committed events. `live`
is model output the backend has not committed yet — text at something like
token rate, coalesced on an interval and always flushed before a sub-turn
ends, never stored. The events are the authoritative record and arrive in one
burst per sub-turn. A consumer wanting both must not count the text twice.

The run lives in Go and is driven by the loop, so a parent that stops reading
does not stop it; the process exits when its stdin closes.

**Settings are one registry.** Every setting — the models, the run budget and
the tool limits — is one entry in `internal/settings`' registry, carrying its
type, default, validation bounds and description. Validation lives in the
registry and is enforced in Go on every write path. The API keys are
deliberately not settings: a `-state-dir` a parent keeps for resuming must
not become a file holding a plaintext key, so they stay closures over local
variables for the process's life.

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

#### Retrying a stream that dies mid-flight

`providerhttp.Transport.Do` retries a transient 429/500/503 with backoff
before a stream starts. It has no counterpart for a request that gets a 200
and then dies partway through the body: a TCP reset, a truncated read, the
idle watchdog firing. That gap ended a 23-sub-turn production run outright
([OBSERVED.md](OBSERVED.md), "A mid-stream TCP reset lost a 23-sub-turn
run"). `providerhttp.Transport.RetryStream` closes it, on two rules.

**A stream that has already produced output is never retried.** A reasoning
delta, a content delta, a tool-call delta, or a thought-signature delta may
already be live on the hub a browser is watching, or already queued into the
batch `internal/session` commits for the sub-turn. Reopening the request
after any of those would send a second, independent completion. Nothing
downstream could tell its text apart from the first attempt's — DeepSeek's
Chat Completions surface has no notion of resuming a stream from where it
left off. So once a stream has spoken, it fails exactly as it did before
this existed: loud, ending the sub-turn. A stream that produced nothing at
all is the only one RetryStream reopens, because nothing needs reconciling
with a second attempt's output.

**Only a failure that looks like a dead connection is retried.** A
connection reset, an unexpected EOF, any other `net.Error`, and the idle
watchdog's own sentinel all read as "the connection is gone", and get the
pre-stream 503's backoff schedule and retry budget. A decode error from a
malformed frame does not: the bytes on the wire would be identical on a
second attempt, so retrying would only resend them into the same bug.
`context.Canceled` and `context.DeadlineExceeded` are excluded by
`errors.Is`, checked directly against the error rather than against which
code path delivered it. The same incident's logs carried a cancellation that
reached the line-reader's error branch instead of the context-cancellation
branch beside it, so the exclusion has to catch both. The operator pressing
stop must never come back as a retried run.

`RetryStream` lives in `internal/providerhttp`, not in each provider's own
retry predicate or in `internal/session`: the gap is the same for all three
providers, whatever their frames look like. It takes the request-opener and
the frame pump as arguments. DeepSeek's and Kimi's `StreamChatCompletion`
hand it `Transport.PumpStream`; Gemini's hands it `pumpChatEvents`, a pump
reading a step-typed frame vocabulary neither of the other two shares, with
no adapter needed — a pump's signature is "read a body, write typed events",
whatever it decodes to get there. `internal/session` sees none of this: a
retried stream looks the same as one that never failed.

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

### 4.5 The session goroutine, and retries

A session is a goroutine in this process, not a child process. The binary
owns the SQLite handle, the event hub, and the loop; the running session is a
goroutine holding only its own state.

    harness stdio-session (one process)
      protocol reader        ──▶ one session goroutine per response chain
      store writer goroutine  serialises every append to SQLite
      hub                     fan-out to the protocol writer

The session runner sits behind an interface. Moving execution to a subprocess
or another host later changes what implements that interface and leaves the
loop, the tools, and the store untouched.

Rules the arrangement imposes:

- A panic in a session must not take the process down. The session goroutine
  recovers, writes an `error` event, publishes a failed result, and exits.
- Cancellation is a context per session, derived from the process context and
  from the request deadline. A cancelled session still writes its terminal
  events.
- The workspace is the parent's, and the parent names it per create call. Two
  sessions must never be pointed at one directory: concurrent `Edit` and
  `Bash` calls against it interleave, and neither model can see why its file
  changed under it. Nothing here enforces that — the parent owns the
  directory, so the parent owns the rule.
- Per-session state stays per-session. The churn diagnostic's
  previous-request hashes (§4.9, [CACHE.md](CACHE.md)) are the easy thing to
  accidentally share.

SQLite runs in WAL mode with a busy timeout. Writes funnel through a single
writer goroutine fed by a channel, so `SQLITE_BUSY` never arises from our own
concurrency; readers use a separate read-only connection pool. Event appends are
batched per turn where they arrive faster than a transaction per event is worth.

Two deliberate exceptions to that batching, both about what somebody watching
a run can see:

- **`turn_started` is committed on its own, before the request goes out.** A
  batch is stamped with one instant, so committing it with the rest gave it the
  time the model *finished* — every timestamp in a sub-turn was then off by the
  duration of the model call, and the transcript's live turn had nothing to
  measure its age from. One extra transaction per sub-turn buys a timestamp that
  means what it says.
- **Model output is streamed to the hub without being stored** (`hub.Frame`,
  `hub.LiveDelta`). The sub-turn's `reasoning_delta` and `content_delta` events
  are still committed once, in the batch, when the response completes; the hub
  additionally carries the same text as it arrives, coalesced to ~10 frames a
  second, so a client can render it live. The log's shape is unchanged and a
  reconnect rebuilds from it alone.

Retry 429, 500, 502, 503, and 504 with exponential backoff and jitter for
DeepSeek and Kimi K3: 502 and 504 are the reverse-proxy gateway in front of
the API answering its own fault rather than the model backend's, the same
class of error 500 and 503 already cover (docs/OBSERVED.md, "A gateway 502
ended a run at sub-turn 91"). Gemini retries only 429, 500, and 503, for
want of evidence that its Interactions API sits behind a comparable gateway
(internal/gemini/retry.go). Do not retry 400, 401, 402, or 422 — those are
bugs or an empty account, and a retry burns a turn. Surface 402 distinctly:
it means the balance is gone, not that the harness broke.

### 4.6 Tools and permission policy

Specified in [TOOLS.md](TOOLS.md). The set is `Read`, `Write`, `Edit`, `Bash`,
`Glob`, `Grep`, `List`, `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`,
`Task`, and `WebFetch` — the vocabulary of the harnesses DeepSeek names as its
V4 agent optimisation targets — plus `Complete`, which is ours, and the vision
path — `Screenshot`, `Glance`, `Ground`, `Detect`, `Transcribe`, and `Crop` —
sent to a model with no native vision capability: every model
`provider.SeesImages` does not name true, `deepseek-v4-pro` among them —
though this harness does not route that model at all
(docs/DEEPSEEK-VISION.md).

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
create body must name a permission mode and may add deny patterns (§4.10). The
session holds that policy for its whole life. A tool call is evaluated against
it in Go and either runs or returns a denial through the tool result channel,
which the model reads and routes around. Every decision is synchronous, so a
session never waits on anything but the API and its own tools.

A denial is unconditional: nothing consults the model or a person mid-call to
turn it into an approval. One decision point, and no approval state in the
event log.

### 4.7 Model routing and thinking settings

Specified in [MODELS.md](MODELS.md). The defaults are `deepseek-flash` at
`high` effort for the main loop and for subagents and mechanical side work.
That follows DeepSeek's recommended Claude Code
configuration except on effort, where measurement put `max` at 2.1× the
wall-clock for no measured gain.

Points that bear on the rest of this design:

- Side work runs in its own conversation rather than appended to the main one,
  which keeps the main prefix stable and avoids mixing per-model caches.
- Model and effort are chosen at session creation, from the create body,
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
      request.json      the originating create body, when there was one

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
output tokens, reasoning tokens, and derived cost. A run's result
carries the same figures, so a caller can price its own job (§4.10).

### 4.10 How a run starts

A create call is the whole of ingress. The parent names the model, the
prompt, the permission mode and `harness.cwd` — the directory the session
works in, which this process never chooses for itself, and which it does not
create, clone into or lease. A continuation names the response it follows and
inherits that directory; it cannot change it, because a chain of responses is
one session and one session is one workspace.

There is no queue and no idempotency row. The parent is the only caller, it
holds the response id it was given, and a repeat is its own to avoid — a
durable queue exists to survive a producer that cannot retry, and a parent
reading this process's stdout is not one.

What a hosted session gives up with the clone is worth stating: an agent in
`full` mode is loose in a repository somebody may be editing, with no per-run
copy to throw away. The permission mode is the whole of what this protocol
gives a parent to bound that with.

### 4.11 Skills

A repository can ship Agent Skills: a directory per skill holding a `SKILL.md`
whose YAML frontmatter carries a name and a description. `internal/skills`
scans the workspace at session start, under `.claude/skills/`,
`.deepcode/skills/` and `.agents/skills/` — the third being the vendor-neutral
spelling DeepSeek's own agent and Gemini's hosted environment have both
converged on — plus the workspace's own `skills/` directory, and renders the
names and descriptions it finds into the opening user message ahead of the
task. The model reads a skill's body with `Read` when it decides one applies.

Three consequences follow from §3.2. The catalogue goes in the opening message,
never the system prompt, so a repository's skills cannot disturb the cached
head. No `Skill` tool exists, because a twentieth tool definition would enlarge
that head for every session to duplicate what `Read` already does. An empty
catalogue renders to nothing, leaving the opening message byte-identical to a
run with no skills.

The missing tool has one cost, and it is paid by whoever reads the transcript
rather than in the cached head: with no `Skill` call and no event marking the
moment, a skill being taken looks exactly like a file being read, on one row
out of the hundreds a run produces. A client that wants to tell them apart
matches each `Read` target against the catalogue's own paths rather than
against the file name, which is what keeps it honest in a repository whose
subject matter is skills.

Only the description reaches the model up front, capped in length and in count,
with anything dropped stated in the catalogue rather than silently omitted.
Discovery never fails a run: an unreadable directory, a malformed `SKILL.md`,
and a skill with no description each yield no entry and no error.

A skill that instructs the agent to run a bundled script inherits the session's
permission mode. Under `readonly` that call is refused at execution. Under
`full` it runs, as any code in the workspace does.

Nothing installs skills into a workspace. The parent owns the directory, so
whatever it wants a session to have it puts there before the create call —
which is also why the catalogue can differ per run without costing anything:
it is in the opening message, so two runs with different skills still share a
byte-identical system prompt and tool array. This is exactly why an MCP
server's tools cannot work the same way ([MCP.md](MCP.md)): those go in the
frozen head, so they must be global, and a per-run choice there would fragment
the cross-session cache.

`session_started` carries the rendered catalogue alongside the opening message,
as an exact substring of it. That is what lets a client lift the catalogue into
a panel of its own without parsing prose, and it is the one event that yields
two blocks, which is why a transcript keys blocks on sequence number and type
together.
