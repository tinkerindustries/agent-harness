# Architecture

Where things live and what they may depend on. Why they are that way is
[`docs/DESIGN.md`](docs/DESIGN.md), and § references below point into it.

## Overview

A work request names a prompt, one or more repositories, and a permission mode.
The harness clones those repositories into a directory of its own, runs an agent
loop that reads and writes files and runs commands in there, and publishes a
result. Requests arrive over NATS JetStream; results go back over a second
stream. A browser can watch, change settings, and start, steer, and stop a run.
Start and stop act on the run through two declared seams
(docs/RUN-CONTROL.md): `RunPublisher`, a narrow interface declared in
`internal/httpapi` and implemented by `cmd/harness` over the queue's own
JetStream handle, publishes a validated work request to the WORK stream; and
`RunController`, declared in `internal/httpapi` and implemented by
`*worker.Pool`, ends a run in this process. Steer needs no seam at all — it is
a store write by the handler and a store read by the loop, with the database
as the boundary.

One binary, `harness`, is every entry point. One subcommand is a long-running
service and the rest are one-shot CLI:

- **`harness serve`** — the worker pool, the SQLite store, the HTTP
  surface, and the MCP launch server, in a single process. It serves the web
  UI, `/api/...`, and `/mcp` on one HTTP port; the embedded MCP service lets
  an external agent harness launch and collect runs on the same streams and
  the same HTTP API. It holds no database handle of its own — `serve` is the
  single writer. Concurrent sessions are goroutines, not child processes
  (§4.5).

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
    hub --> http[internal/httpapi<br/>SSE; GET/HEAD, writes, run control]
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

One entry per package — what it is for, what it may depend on, and the
`docs/DESIGN.md` section behind it — now lives beside the code it describes,
in [`internal/CLAUDE.md`](internal/CLAUDE.md), so an agent editing a package
loads the map for it without being handed the whole document.

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

- **`internal/httpapi` imports neither `session` nor `worker`.** It reaches the
  store and the hub, for both reads and writes, and neither of those reaches
  session or worker. That import boundary — not the absence of write endpoints
  — is what keeps run control a declared seam rather than a reach into a
  running loop, and it is why the boundary survived the read-only rule being
  retired. Run control goes through seams declared here deliberately rather
  than by an import appearing: the stop endpoint holds a narrow `RunController`
  interface implemented by `*worker.Pool`, and the start endpoint holds a
  narrow `RunPublisher` interface implemented by `cmd/harness` over the
  queue's own JetStream handle (docs/RUN-CONTROL.md), exactly the shape
  `QueuePool` already uses for `/api/queue`. Steering is the exception that
  proves the rule — it needs no seam because it is a store write by the
  handler and a store read by the loop, and the store is already here.
- **`internal/session` is the only package that speaks to both the API client
  and the tools.** A change that needs both belongs there.
- **`internal/worker` is the only package that acks a JetStream message.**
- Nothing imports `cmd/`.

## Cross-cutting concerns

**Configuration.** Two sources, and the split is deliberate. Bootstrap —
where the database lives, where the service binds, the NATS address, the
price table path, the workspace root — comes through `internal/config` from
the environment, with `.env` loaded best-effort at startup and real
environment variables winning over it. Everything operator-tunable — API
keys, models, run budgets, tool limits, worker sizes, retention — lives in
the `settings` table and resolves through `internal/settings`' registry, so
an operator changes a limit with `harness config set` (or the settings
screen) without a rebuild. Settings marked "requires a restart" are read once
at startup and the UI says so. Names and defaults are documented in
`.env.example` and in the registry itself.

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
- **The HTTP API serves `GET` and `HEAD` on every path, and the writing
  methods only where a write route exists.** The three actions that touch a
  run are `POST /api/runs`, which publishes a validated work request to the
  WORK stream through the declared `RunPublisher` seam; `POST
  /api/sessions/{id}/stop`, which goes through the declared `RunController`
  seam; and `POST /api/sessions/{id}/steer`, which is a store write the loop
  reads at its next sub-turn boundary — all authenticated by a bearer token
  (docs/RUN-CONTROL.md). A POST anywhere else stays a 405 with a correct
  `Allow` header.
- **`internal/mcp` opens no SQLite handle.** `harness serve` is the single
  writer.
- **`parent_is_user` is producer-stamped.** It is set by the three producers —
  the browser's `POST /api/runs`, the MCP `deepseek_agent` tool, and the CLI —
  never by a request body or a tool input, and never inherited from anything a
  calling agent asserts. `parent_agent_type` remains caller-asserted and is
  therefore not trustworthy the way `parent_is_user` is.
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
same event log. A new event kind needs both. The steering pair is the current
example: both folds know `steer_message` and `steer_applied`, and each does
with them what its own consumer needs — the Go fold appends the applied
steer's text as a user message, and the browser fold emits a pending block
and flips it to delivered, which is the one place the browser fold completes a
block it has already emitted (docs/RUN-CONTROL.md "The frontend").
