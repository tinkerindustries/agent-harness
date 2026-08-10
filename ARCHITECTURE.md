# Architecture

Where things live and what they may depend on. Why they are that way is
[`docs/DESIGN.md`](docs/DESIGN.md), and § references below point into it.

## Overview

A work request names a prompt, one or more repositories, and a permission mode.
The harness clones those repositories into a directory of its own, runs an agent
loop that reads and writes files and runs commands in there, and publishes a
result. Requests arrive over NATS JetStream; results go back over a second
stream. A browser can watch, and can do nothing else.

One binary, `harness`, is every entry point. Two subcommands are long-running
services and the rest are one-shot CLI:

- **`harness serve`** — the worker pool, the SQLite store, and the read-only
  HTTP surface, in a single process. Concurrent sessions are goroutines, not
  child processes (§4.5).
- **`harness mcp`** — a separate process on its own port, letting an external
  agent harness launch and collect runs. It publishes to the same streams and
  reads the same HTTP API. It holds no database handle, because `serve` is the
  single writer.

```mermaid
flowchart LR
    caller[MCP client / CLI publish] -->|harness.work.request| WORK[(WORK stream)]
    WORK --> worker[internal/worker pool]
    worker --> ws[internal/workspace<br/>clone per session]
    worker --> session[internal/session<br/>agent loop]
    session <--> api[api.deepseek.com]
    session --> tools[internal/tools<br/>in the workspace]
    session --> store[(SQLite + disk mirror)]
    session --> hub[internal/hub]
    hub --> http[internal/httpapi<br/>SSE, GET/HEAD only]
    store --> http
    http --> web[web/ React]
    worker -->|result| RESULTS[(RESULTS stream)]
    RESULTS --> caller
```

One property shapes the arrangement: the head of every request — system prompt,
then tool array — is byte-identical across sessions and frozen for a session's
life, and the message array only ever grows at the end (§3.2). Packages are
split so that nothing on the request path can perturb that head. Skills render
into the opening user message rather than the system prompt. Permission changes
which tool calls *run*, never which tools are *offered*. A resumable session
stores the prompt and schema it was created with, so a harness upgrade cannot
rewrite its prefix.

## Codemap

### `cmd/harness`
Subcommand dispatch, flag parsing, and process wiring — one file per subcommand.
Composition happens here and nowhere else; no `internal` package constructs
another's dependencies.

### `internal/deepseek`
The API client. Request and response types as structs rather than maps, so
serialisation is byte-stable; SSE reading with an idle watchdog; the tool-call
assembler keyed by call index; retry classification. Knows nothing of sessions,
tools, or storage. Depends on: nothing internal. §4.3, §4.4.

### `internal/session`
The agent loop: sub-turn iteration, the system prompt, tool dispatch, ordering
of tool results, compaction, and resume. The widest dependency set in the repo,
deliberately — this is where everything meets. Consumed by `internal/worker` and
by the CLI's `run` and `resume`. §4.5, §4.6.

### `internal/tools`
Every tool the model can call: schemas matching the trained-in shape, argument
validation in Go, workspace confinement, per-tool timeouts and output caps, and
the permission policy that gates execution. Also owns the frozen tool
definitions the request head carries. The catalogue and its wording are
[`docs/TOOLS.md`](docs/TOOLS.md); a change here is a cache-prefix change.

### `internal/fold`
Folds the event log into the DeepSeek `messages` array. Pure, append-only, a
switch on event kind. Its counterpart is the frontend's own fold in
`web/src/api`, which walks the same log to produce display blocks. §4.1.

### `internal/store`
SQLite (`modernc.org/sqlite`, pure Go, WAL) plus the derived disk mirror under
`<data dir>/sessions/` and diff computation. The database is authoritative; the
mirror is rebuildable with `harness export`. The `settings` table holds the
harness's stored configuration — the DeepSeek API key among it — written and
read through `internal/settings`. Depends on: nothing internal. §4.8.

### `internal/hub`
In-process SSE fan-out: per-session transcript subscribers and a quieter
session-list subscriber set. Fed the same events a session appends. Touches no
JetStream — the browser reads the store and the hub, never NATS. §4.2, §5.8.

### `internal/httpapi`
The read-only HTTP surface and the static file server for the embedded
frontend. `GET` and `HEAD` only; everything else is 405. Serves the store and
the hub and cannot reach a running loop. §4.2.

### `internal/webassets`
`go:embed` of the built frontend, so the binary ships with no runtime assets.
`dist/` is Vite output and is not in git.

### `internal/queue`
JetStream wiring shared by every NATS caller: stream and consumer declaration,
the work request and result bodies, and the progress rate limiter. §4.10.

### `internal/worker`
The pool. Pulls a request, builds its workspace, runs it as a session,
publishes the result, then acks — in that order, so a crash redelivers rather
than loses. Owns acknowledgement discipline and idempotency against the
`work_requests` table. §4.10.

### `internal/workspace`
Prepares the per-session directory and clones the repositories a request names,
including the remote-URL restrictions that keep `ext::` and local paths out.
§4.10.

### `internal/skills`
Scans each cloned repository for `.claude/skills/` and `.deepcode/skills/` and
renders what it finds into a catalogue. Discovery never fails a run. Depends on:
nothing internal. §4.11.

### `internal/claudemd`
Scans the workspace and each cloned repository for a root `CLAUDE.md` and
renders their contents into the opening user message, ahead of the skill
catalogue and the task, capped per file and in total. Discovery never fails a
run. Depends on: nothing internal.

### `internal/mcp`
The MCP launch server: tools and resources over streamable HTTP, backed by the
WORK and RESULTS streams and the harness's read-only API. Imports `store` and
`hub` for their types only — it renders transcripts fetched over HTTP and opens
no database. It never touches the system prompt or the tool array.

### `internal/cache`
The prompt-cache churn diagnostic from [`docs/CACHE.md`](docs/CACHE.md):
predicts a sub-turn's cache miss from what the harness knows it appended and
names the first message that differs when prediction and reality diverge.
Diagnostic, not a request-path dependency.

### `internal/config`
Environment loading and `.env` parsing. Defaults follow
[`docs/MODELS.md`](docs/MODELS.md). Read configuration through here rather than
calling `os.Getenv` elsewhere.

### `internal/settings`
The names and resolver for the `settings` table: which keys exist
(`deepseek.api_key`, `google.api_key`), validation that a read or write names a
known key, and a resolver that reads through to the store on every call, so a
key changed by another process takes effect on the next request without
restarting anything. Depends on: the settings surface of `internal/store`
only — never `session`, `tools`, or `config`.

### `internal/pricing`
The price table, loaded from JSON at runtime and carrying its own capture date.
Depends on: nothing internal. §4.9.

### `web/`
The React frontend — two screens, no write path. Its own build and test cycle;
see [`web/CLAUDE.md`](web/CLAUDE.md) for the constraints on changing it. §5.

## How the pieces relate

Dependencies run one way, and Go's own `internal` visibility plus the absence of
cycles is the only enforcement — there is no import linter.

```
deepseek  pricing  skills   claudemd store    webassets     (no internal dependencies)
      ↑       ↑        ↑        ↑        ↑        ↑
   tools ─────┘        │        │        │        │
      ↑                │        │     fold        │
   queue               │        │        ↑        │       httpapi ← hub
      ↑                │        │        │        │
workspace          session ─────┘        │        │
      ↑                ↑                 │        │
   worker ─────────────┘                 │        │
                                         │        │
   mcp ──────────────────────────────────┘        │  (types only)
```

The edges that matter:

- **`internal/httpapi` imports neither `session` nor `worker`.** The read path
  reaches the store and the hub and stops there. Keeping it that way is what
  makes "no endpoint can start or steer a run" a structural fact rather than a
  policy.
- **`internal/session` is the only package that speaks to both the API client
  and the tools.** A change that needs both belongs there.
- **`internal/worker` is the only package that acks a JetStream message.**
- Nothing imports `cmd/`.

## Cross-cutting concerns

**Configuration.** Two sources. Runtime and deployment settings come through
`internal/config` from the environment, with `.env` loaded best-effort at
startup and real environment variables winning over it. Stored settings — the
DeepSeek API key today — live in the `settings` table and resolve through
`internal/settings`, so a key changed by one process takes effect in another
without a restart. Names and defaults are documented in `.env.example`.

**Pricing.** Never compiled in. The table loads from the path in
`DEEPSEEK_PRICE_TABLE` and carries a capture date that the cost readout shows,
because a cost computed from a stale table looks authoritative and is wrong.

**Permission.** A policy the session holds for its whole life, evaluated in Go
at the moment of a tool call. A denial returns through the tool result channel
for the model to route around. Two modes, `readonly` and `full`, plus optional
deny patterns from the request. The CLI is the one interactive caller and plugs
a terminal prompt into the same seam by registering a resolver; queue-driven
sessions register none, so a session never blocks on a human. §4.6.

**Persistence and observability.** Every event lands in SQLite inside a
transaction, then appends to the disk mirror; a failed mirror write logs and
does not fail the run. Reasoning is stored verbatim and never truncated,
because it goes back to the API.

## Invariants

Properties that must hold. Most are invisible in the code, which is why they
are here.

- **The request head is frozen.** System prompt and tool array are rendered at
  session creation, stored on the `sessions` row, and never recomputed. Adding a
  tool, reordering the array, or reworded prompt text is a cache-prefix change
  for every session (§3.2, [`docs/CACHE.md`](docs/CACHE.md)).
- **The message array is append-only.** Nothing edits or removes an earlier
  message. Compaction starts a new session rather than rewriting one.
- **`tool_choice` is never sent.** Thinking mode rejects `required` and
  named-tool forcing, so no tool can be forced; the system prompt asks instead.
- **Assistant messages carrying `tool_calls` have `""` content, never `null`.**
- **Tool results append in `tool_calls` array order**, regardless of which
  finished first. Parallel calls cannot be disabled, so ordering is what
  protects the prefix.
- **The skills catalogue goes in the opening user message, never the system
  prompt**, and there is no `Skill` tool — a twelfth tool definition would
  enlarge the frozen head to duplicate what `Read` already does (§4.11).
- **A `usage` event is one request, not one sub-turn.** The reasoning-starved
  retry (§4.5) sends a second request for the same sub-turn and the API bills
  both, so that turn commits two, sharing `sub_turn` and told apart by
  `attempt`. A cost total sums every event; anything wanting the turn's
  standing state — the cache detector on resume — takes the last.
- **The HTTP API serves `GET` and `HEAD` and nothing else.** No endpoint starts,
  steers, or stops a run.
- **`internal/mcp` opens no SQLite handle.** `harness serve` is the single
  writer.
- **`internal/webassets/dist` is build output.** Never hand-edit it; never
  commit anything there but `.gitkeep`.
- **The vendored mirror in `third_party/deepseek-docs/` is generated.** Refresh
  it by re-scraping, never by editing a file.

## Gotchas

**A `go build` with no frontend build first produces a binary that serves
nothing.** `go:embed` bakes in whatever is in `internal/webassets/dist` at
compile time, and git holds only a `.gitkeep` there. The directory itself is
tracked because `go:embed` will not compile against a missing one. Run
`npm --prefix web run build`, or build the image, which does it for you.

**`docker compose up -d` without `--build` restarts the old code.** The image
bakes both the frontend and the binary.

**`status: "ok"` does not mean the model succeeded.** `complete_status` is a
separate field mirroring what the model said about finishing: `gave_up` is a run
that completed cleanly and reported failure. Empty is normal, because `Complete`
cannot be forced. A caller checking only `status` cannot tell the two apart.

**The `denied` result status is unused.** It described a workspace-lease
conflict that a per-run directory made impossible. Don't build on it.

**Deltas reach the browser in bursts, not at token rate.** `session/turn.go`
accumulates a sub-turn's reasoning and content in Go and commits one event each
at the end; only `tool_stdout` streams live. Both folds handle genuinely
incremental events unchanged, so the frontend is built for a stream it does not
currently get (§5.0). Measure before treating a frontend rendering path as the
bottleneck.

**`session_started` yields two display blocks, not one.** The rendered skills
catalogue is carried alongside the opening message as an exact substring of it,
so the browser can lift it into its own panel without parsing prose. That is why
transcript blocks key on sequence number *and* type.

**The host's docker socket is mounted into the harness container.** A session in
`full` mode can therefore do anything to the host daemon, including mounting the
host filesystem into a container it starts. Paths given to `docker run -v` inside
a session name the host, not the container.

**The two folds must agree in shape.** `internal/fold` produces the API
`messages` array and `web/src/api/fold.ts` produces display blocks, from the
same event log. A new event kind needs both.
