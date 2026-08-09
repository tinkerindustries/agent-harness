# deepseek-harness — technical design

A coding harness: an agent loop that reads and writes files in a workspace, runs
commands, and iterates until a task is done. Go owns the loop and the tools. A
React frontend shows the transcript and gates dangerous actions.

One DeepSeek behaviour drives most of the decisions below: the prompt cache is
worth 50–120× on input tokens, and hits are blocked at 128 tokens of common
prefix. Section 3.2 covers it. Section 3.1 covers reasoning replay, which the
docs present as mandatory and which measurement shows is not.

## 1. Scope

In scope for v1: single user, local workspace, streaming transcript, the ten
tools in [TOOLS.md](TOOLS.md), a live plan panel driven by `TodoWrite`,
flash-backed subagents via `Task`, permission modes with diff review, cost and
cache accounting, and session resume.

Out of scope for v1: auth and multi-user, remote or containerised workspaces, an
editor pane, FIM inline completion, MCP, prefix completion, background shells,
edit checkpointing and rollback. Each is additive against this architecture.

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

- The system prompt is fixed for the life of a session. No clock, no cwd, no git
  status, no changed-file list inside it.
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

[CACHE.md](CACHE.md) covers the tactics and the churn diagnostic.

## 4. Backend

### 4.1 Event-sourced session

A session is an append-only log of events. The log is what streams to the
browser, what persists to disk, and what the DeepSeek `messages` array folds
from. One source of truth, three consumers. The frontend folds the same log into
its view model.

Events: `turn_started`, `reasoning_delta`, `content_delta`, `tool_call`,
`tool_approval_required`, `tool_stdout`, `tool_result`, `usage`, `turn_finished`,
`error`. Each carries a per-session monotonic sequence number.

### 4.2 Transport

SSE down, POST up. The browser subscribes to
`GET /api/sessions/{id}/events` and sends commands to `/prompt`, `/approve`, and
`/cancel`. Traffic is asymmetric — a firehose down, occasional clicks up — so SSE
fits, and `Last-Event-ID` gives replay without extra protocol.

A reload mid-run reconnects and replays from the last sequence number. The run
lives in Go, so closing the tab does not kill the agent.

WebSocket buys nothing here; the client never sends at rate.

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
tool-call sample in the docs is non-streaming. Whether DeepSeek emits incremental
`tool_calls` fragments in OpenAI's shape — indexed, with `arguments` arriving in
pieces — is not settled by the documentation. Write the assembler for the
incremental form, keyed by `index`, accumulating `id`, `name`, and `arguments`.
That code also handles a whole-tool-call-in-one-chunk stream correctly. Observe
the real shape at step 2 of the plan and simplify if it turns out to be simpler.

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

### 4.5 Concurrency and retries

A per-model semaphore, sized under the account limits: 500 concurrent for pro,
2500 for flash, counted account-wide rather than per key. A single-user harness
will not approach these; parallel side work can.

Retry 429, 500, and 503 with exponential backoff and jitter. Do not retry 400,
401, 402, or 422 — those are bugs or an empty account, and a retry burns a turn.
Surface 402 distinctly: it means the balance is gone, not that the harness broke.

### 4.6 Tools

Specified in [TOOLS.md](TOOLS.md). The set is `Read`, `Write`, `Edit`, `Bash`,
`Glob`, `Grep`, `List`, `TodoWrite`, `Task`, and `WebFetch` — the vocabulary of
the harnesses DeepSeek names as its V4 agent optimisation targets.

Three points from that document bear on the rest of this design:

- Schemas match the trained-in shape and are not strict-mode by default.
  Arguments are validated in Go and a bad one returns an error through the tool
  result channel for the loop to recover from.
- Tool results append in `tool_calls` array order regardless of completion
  order. DeepSeek emits parallel tool calls and they cannot be disabled, so
  ordering is what protects the prefix (§3.2).
- Approval is an event and the loop blocks on it, so it survives a reload.

### 4.7 Model routing and thinking settings

Specified in [MODELS.md](MODELS.md). The defaults follow DeepSeek's own
recommended Claude Code configuration: `deepseek-v4-pro` at `max` effort for the
main loop, `deepseek-v4-flash` for subagents and mechanical side work.

Points that bear on the rest of this design:

- Side work runs in its own conversation rather than appended to the main one,
  which keeps the main prefix stable and avoids mixing per-model caches.
- Model is selectable per session and switchable mid-session. A switch is a full
  cache miss across the conversation, so the UI prices it at the click.
- The effort mapping is not identity and pro is due to change during August
  2026. Read the vendored thinking-mode guide rather than a compiled-in table.
- Thinking mode silently ignores `temperature` and `top_p`. The UI hides them
  while thinking is enabled.

### 4.8 Persistence

SQLite through `modernc.org/sqlite` — pure Go, no cgo, so the binary stays static
and cross-compiles. One table of events keyed by `(session_id, seq)`, one of
session metadata. Resume replays the log.

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
output tokens, reasoning tokens, and derived cost.

## 5. Frontend

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
counts. Step 8 of the plan measures; the measurement decides.

### 5.6 Reasoning display

Reasoning is voluminous and mostly skimmed. It renders in a panel that expands
while streaming and collapses on completion, showing elapsed time and token
count. Display state has no bearing on storage — the full text stays in the store
because it goes back to the API.

### 5.7 Stack

Vite, React, TypeScript. No component framework. Plain CSS with custom
properties. One screen plus a session list, so no router library initially.
