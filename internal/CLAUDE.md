# internal/

The Go packages, one entry each: what the package is for, what it may depend
on, and the `docs/DESIGN.md` section that argues for it. Composition happens in
`cmd/harness` and nowhere else — no package here constructs another's
dependencies.

[`../ARCHITECTURE.md`](../ARCHITECTURE.md) is the rest of the picture: how the
pieces relate, the dependency direction, the cross-cutting concerns, the
invariants and the gotchas. [`../docs/DESIGN.md`](../docs/DESIGN.md) is why any
of it is shaped that way, and the § references below point into it.

One property shapes the arrangement: the head of every request — system prompt,
then tool array — is byte-identical across sessions and frozen for a session's
life, and the message array only ever grows at the end (§3.2). Packages are
split so that nothing on the request path can perturb that head.

## Codemap

### `cmd/harness`
Subcommand dispatch, flag parsing, and process wiring — one file per subcommand.
Composition happens here and nowhere else; no `internal` package constructs
another's dependencies.

### `internal/deepseek`
The DeepSeek API client: the request body it builds from a `wire.ChatIntent`,
the auxiliary endpoints (`/models`, `/user/balance`), the error body, retry
classification, and the narrow repairs for the quirks recorded in
docs/OBSERVED.md (the misplaced brace in large arguments objects, and
reasoning starvation under a small max_tokens budget). Implements the narrow
`Client` seam `internal/session` declares (docs/KIMI-INTEGRATION.md §4.1): it
turns the loop's `wire.ChatIntent` into DeepSeek's request shape —
`thinking: {type}` and `reasoning_effort` — maps usage onto cache-hit and
cache-miss counts, and owns the two response quirks. The HTTP transport
underneath — base URL, per-request key, retry-with-backoff, the SSE pump and
its idle watchdog — is `internal/providerhttp`, shared with `internal/kimi`;
what stays here is DeepSeek's own dialect. Knows nothing of sessions, tools,
or storage. Depends on: `internal/wire`, `internal/providerhttp`. §4.3, §4.4.

### `internal/providerhttp`
The HTTP transport `internal/deepseek` and `internal/kimi` share: a request
retried with backoff on a provider-supplied set of transient status codes,
and a streaming response pumped as SSE frames into `wire.Event`s behind an
idle watchdog. Carries no provider dialect — no request shape, no usage
mapping, no error-body parsing, no quirk repairs — so each provider keeps its
own `Client` type satisfying the narrow `session.Client` seam independently;
this package only removes the near-verbatim duplication two full client
implementations used to carry (docs/KIMI-INTEGRATION.md §4.1). Depends on:
`internal/wire`.

### `internal/kimi`
The Kimi K3 client, Moonshot AI's OpenAI-compatible endpoint
(`https://api.moonshot.ai/v1`). Shares `internal/wire`'s vocabulary and
`internal/providerhttp`'s transport with `internal/deepseek`; what stays
here is Kimi's own dialect — the client and its options, the auxiliary
endpoint bodies (`/models`, `/users/me/balance`), the error body, retry
classification, and the usage split mapping Kimi's single `cached_tokens`
figure onto the cache-hit/cache-miss counts the cost model uses. Implements
the narrow `Client` seam `internal/session` declares
(docs/KIMI-INTEGRATION.md §4.1): top-level `reasoning_effort`, never a
`thinking` field. Also the client an eval's judge model resolves to when
`model.judge` names `kimi-k3` (`internal/evals`). Knows nothing of sessions,
tools, or storage. Depends on: `internal/wire`, `internal/providerhttp`.

### `internal/gemini`
Google's Gemini API client, hand-rolled rather than the official SDK because
the SDK targets the legacy `generateContent` surface, not
`POST /v1beta/interactions` (docs/GEMINI-INTEGRATION.md §2). Two callers: the
vision tools — `Glance`, `Ground`, `Detect` (docs/TOOLS.md) — through
`Interact`, unchanged since before this package spoke chat completions at
all; and the agent loop, through `StreamChatCompletion` and
`CreateChatCompletion`, the same narrow `Client` seam `internal/deepseek` and
`internal/kimi` implement. Owns the parts of the Interactions surface with no
counterpart in the OpenAI-format dialect the other two share: `thought` steps
carrying an opaque, mandatory signature that must be replayed verbatim
(`wire.Message.ThoughtSignature`, docs/GEMINI-INTEGRATION.md §5.2), a request
shape typed by step kind rather than a flat message array, `arguments` as a
genuine JSON object rather than a string, and errors that can arrive
SSE-framed even under a plain 400. No retry-with-backoff of its own — a
known gap, not an oversight (docs/GEMINI-INTEGRATION.md §8) — because
`internal/providerhttp` hardcodes an `Authorization: Bearer` header this
provider does not send (`X-Goog-Api-Key` instead); it runs its own SSE pump
rather than share that transport. Request and response bodies are Go
structs, never `map[string]any`, for the same byte-stability reason as the
other two clients. Depends on: `internal/wire`.

### `internal/attachment`
Validates one image attachment a producer submitted — POST /api/runs
(`internal/httpapi`) or the MCP `deepseek_agent` tool (`internal/mcp`) —
before its bytes reach the store: the name must be a plain file name (the
workspace writes the file under it, so a path-shaped name would be a way out
of `scratch/attachments/`), the extension must be one of the image types
`ReviewScreenshot` accepts, an asserted MIME type must match the extension,
and the decoded bytes must fit the per-file cap. Also carries the
image-extension-to-MIME-type table those two producers, the
screenshot-serving endpoint (`internal/httpapi/screenshots.go`), and the
vision tools (`internal/tools/vision.go`) all agree on — distinct from
`internal/tools/screenshot.go`'s narrower `screenshotOutputExtensions`
(no WebP, because that one names what Chromium's capture can produce, not
what the harness can read back in). A leaf, deliberately: `internal/httpapi`
imports neither `internal/session` nor `internal/worker`, and a validator
that stays a leaf keeps that boundary legible for a second importer.
Depends on: nothing internal.

### `internal/wire`
The provider-neutral wire vocabulary every request path speaks: the message
and tool types, the request and response bodies, the streaming chunk types,
the role, finish-reason, effort, and thinking constants, the SSE scanner,
the tool-call assembler, and the stream event vocabulary — plus
`ChatIntent`, the provider-neutral request intent the session seam speaks
(model, messages, effort, whether to think, the token ceiling, the tools),
which each provider implementation turns into its own request shape. Both
providers produce and consume these unchanged — DeepSeek today, Kimi next
(docs/KIMI-INTEGRATION.md §4.1). Field order is the byte-stability contract
the prompt cache depends on (docs/DESIGN.md §3.2), pinned by the golden
request-body test. Depends on: nothing internal. §3.2, §4.3, §4.4.

### `internal/session`
The agent loop: sub-turn iteration, the system prompt, tool dispatch, ordering
of tool results, compaction, and resume. The widest dependency set in the repo,
deliberately — this is where everything meets. Its reach to the model API is
through a declared seam rather than an import: `Client`, a narrow interface
declared here and implemented by `internal/deepseek`, `internal/kimi`, and
`internal/gemini`, each of which turns the loop's `wire.ChatIntent` into its
own provider's request shape and owns that provider's usage mapping and
response quirks — the same shape `RunPublisher` and `RunController` take,
with cmd/harness choosing the implementation when it builds the Runner
(docs/KIMI-INTEGRATION.md §4.1, docs/GEMINI-INTEGRATION.md §5.1). Owns the
session row's lifecycle around the worker's preparation window: `Create`
inserts it as `creating` before the workspace is built, `FailSetup` moves it
to `failed` with an error event when
preparation fails, and `Run` promotes a pre-created row to `running` (or
inserts when there is none). Consumed by `internal/worker` and by the
CLI's `run` and `resume`. §4.5, §4.6. `Runner`'s five jobs split by file, all
on the same type (`runner.go`'s own package doc names which file holds
which): `RunOptions` and the `Runner` type stay in `runner.go` beside the
settings accessors; `lifecycle.go` is `Create`/`FailSetup`/`Run` and the loop
that drives a run to a terminal result; `sinks.go` is where a sub-turn's
output goes (the disk mirror, the hub); `tooldispatch.go` executes a
sub-turn's tool calls; `livestate.go` persists the plan and recent-calls
roll. MCP support (`mcp.go`, [`../docs/MCP.md`](../docs/MCP.md)) is resolved
once, not read live: `resolveMCPDefinitions` asks the `Runner`'s own `MCP
tools.MCPProvider` seam — nil for a caller with none wired, the CLI and every
existing test among them — for the enabled servers' tool array and
per-server readonly map, and `Run` and `Resume` each call it exactly once and
freeze the result onto the row's `tool_schema` and the policy's
`MCPReadOnlyServers`, so a server an operator toggles mid-run cannot change
what a running session sends.

### `internal/tools`
Every tool the model can call: schemas matching the trained-in shape, argument
validation in Go, workspace confinement, per-tool timeouts and output caps, and
the permission policy that gates execution. Also owns the frozen tool
definitions the request head carries — the per-provider arrays, and
`DefinitionsForVariant`, which resolves a session's array through the
provider→model table and subtracts the named variant's dropped tools
(`internal/promptvariant`), so a variant session's row, head, and requests all
carry the same smaller array. The catalogue and its wording are
[`../docs/TOOLS.md`](../docs/TOOLS.md); a change here is a cache-prefix change.
It also declares `MCPProvider`, the narrow seam `internal/mcpclient`
implements, and owns the `mcp__` naming vocabulary
(`MCPToolPrefix`, `MCPServerOf`) both packages share; `execMCP`
(`mcpexec.go`) is what flattens one MCP server's raw reply into a `Result` —
text under the ordinary output cap, an image written into the session's
`scratch/mcp/`, a server-reported `isError` carried through as an ordinary
failed result rather than a Go error — and the permission policy's
`MCPReadOnlyServers` map is the per-server readonly allowance frozen onto it
at run start ([`../docs/MCP.md`](../docs/MCP.md)).
Depends on: `internal/wire`, `internal/provider`, `internal/promptvariant`,
`internal/attachment` (the image-extension-to-MIME-type table the vision
tools read images by).

### `internal/fold`
Folds the event log into the wire `messages` array (`internal/wire`'s
`Message`, the shape both providers send). Pure, append-only, a
switch on event kind. Its counterpart is the frontend's own fold in
`web/src/api`, which walks the same log to produce display blocks. §4.1.

### `internal/store`
SQLite (`modernc.org/sqlite`, pure Go, WAL) plus the derived disk mirror under
`<data dir>/sessions/` and diff computation. The database is authoritative; the
mirror is rebuildable with `harness export`. The `settings` table holds the
harness's stored configuration — the DeepSeek API key among it — written and
read through `internal/settings`. Owns the session status vocabulary:
`StatusRunning`, `StatusCreating` (the window while a queue-driven run's
workspace is being prepared), the terminal statuses, and `IsLive` — every
branch and SQL predicate that means "this session is live" builds off it,
never a string literal. Split by concern, `store.go`'s own package doc names
which file holds which: the `Store` type and the single-writer loop stay in
`store.go`; `errors.go` is the error vocabulary; `schema.go` the SQL schema
and column migration; `sessions.go` the status vocabulary and session CRUD;
`events.go` the event payload types and the append-only log's queries;
`leases.go` workspace leases; `mcp.go` is the `mcp_servers` table
([`../docs/MCP.md`](../docs/MCP.md)) — CRUD plus `SaveMCPProbe`, whose one
asymmetry is the whole point of the table: a successful probe overwrites
`tools_json`, a failed one only ever writes `probe_error`, so a session
resolving its tool array from a stored row never sees it shrink because a
server happened to be unreachable the moment that row was read. Depends on:
nothing internal. §4.8.

### `internal/hub`
In-process SSE fan-out: per-session transcript subscribers and a quieter
session-list subscriber set. Fed the same events a session appends. Touches
no queue — the browser reads the store and the hub, never the queue. Owns
the two
shapes a session row goes out in, and the projection between them:
`SessionState`, the whole row, which the REST endpoints return and which one
session's own stream carries as `state` frames, and `ListRow`, what the list
feed sends — the fields that screen renders, because a row is republished on
every sub-turn of every running session to every open list. `ListRowOf` is
the only place the two meet, so a field reaches the list feed by being added
there on purpose. §4.2, §5.8.

### `internal/httpapi`
The HTTP surface and the static file server for the embedded frontend. `GET`
and `HEAD` on every path, plus the write endpoints over the data the harness
manages (docs/DATA-API.md) and the run-control endpoints (docs/RUN-CONTROL.md).
Serves the store and the hub and writes through the store; the reach into the
run loop is through two declared seams rather than imports: `RunController`,
implemented by `*worker.Pool`, for the stop endpoint, and `RunPublisher`,
implemented by `cmd/harness` over the store-backed queue, for the start
endpoint; and `EvalController`, implemented over `*evals.Orchestrator`, for
starting and cancelling an eval — so this package still imports neither
`session` nor `worker`
and holds no queue handle, only the narrow ability to enqueue one validated
request (it imports `queue` for the request type and its `Validate`,
deliberately, so a body validated here can never drift from the queue's).
Steering (`POST /api/sessions/{id}/steer`) needs no seam: it is a store write
the session loop reads at its next sub-turn boundary. It holds the loaded
price table for `GET /api/pricing`, which serves the rate schedule and no
rates — `internal/pricing` depends on nothing internal, so this adds no edge
worth worrying about, and the browser prices nothing (docs/DATA-API.md
"pricing"). The MCP server registry (`mcp.go`, [`../docs/MCP.md`](../docs/MCP.md))
is a fifth write surface with the same shape: `GET`, `POST`, `PATCH`, and
`DELETE` on `/api/mcp/servers` read and write `store`'s `mcp_servers` rows
directly, and probing goes through `MCPProber`, a seam this package declares
for itself narrower than `MCPProvider` and implemented by the same
`*mcpclient.Manager` — so triggering a probe from the browser still never
gives this package a reach into `session` or `worker`. §4.2. Split by
resource, one file per
group; `server.go`'s own package doc names which file holds which (the
`Server` type and `routes()` stay there so the whole surface is still
readable in one list).

### `internal/webassets`
`go:embed` of the built frontend, so the binary ships with no runtime assets.
`dist/` is Vite output and is not in git.

### `internal/queue`
The request and result shapes, their validation, and the store-backed work
queue: `Queue` (enqueue, claim, dispose, the wake channel) over the store's
`work_queue` table, plus the `Msg` seam the worker pool consumes. Request
validation checks a named model against the model→provider table
(`internal/provider`), so an unknown model fails at validation rather than
reaching a provider. §4.10.

### `internal/worker`
The pool. Pulls a request, creates its session row as `creating` (Runner.Create,
so the run is visible and stoppable while the workspace is being prepared),
builds the workspace, installs the shipped skills into it
(`internal/skills.Install`, best-effort — a skill that fails to land is one
catalogue entry missing, not a failed run), runs it as a session (which
promotes the row to `running`), records the result on the `work_requests`
row, then acks the queue row — in that order, so a crash redelivers rather
than loses. A preparation failure marks the row `failed` (Runner.FailSetup),
and a stop during preparation marks it `cancelled`, before the setup result
is published, so no row is left stuck in `creating`. Owns acknowledgement
discipline and idempotency against the `work_requests` table. §4.10.

### `internal/workspace`
Prepares the per-session directory — a `scratch/` subdirectory for files that
are not part of the deliverable, an empty `skills/` for skills given to the
session rather than committed to a repository (scanned by `internal/skills`;
the directory name is spelled in both packages and pinned equal by a test, so
neither depends on the other at build time), and clones of the repositories a
request names — including the remote-URL restrictions that keep `ext::` and local
paths out. Each clone then gets its Node dependencies installed, with the
lockfile choosing the package manager; the install is best-effort and never
fails a run. §4.10.

### `internal/promptvariant`
The named alternatives to the shipped system prompt, and the reminder cadences
that go with them. Imports nothing internal but `internal/wire`, which is
what lets `internal/queue` validate a variant name on a work request without
pulling the agent loop in behind it — a boundary
`internal/httpapi/boundary_test.go` pins. `internal/session` owns the prompt
text; this package owns the edits to it. [../docs/EVALS.md](../docs/EVALS.md).

### `internal/provider`
The one model→provider table (docs/KIMI-INTEGRATION.md §4.3): `ModelFor`
maps a model name to the provider serving it, with no default — an unknown
model is an error, so request validation rejects it loudly instead of
silently routing to a provider. Three providers today: DeepSeek
(`deepseek-v4-pro`, `deepseek-v4-flash`), Kimi (`kimi-k3`), and Gemini
(`gemini-3.7-flash`, docs/GEMINI-INTEGRATION.md §7 Phase 5). It is a package
of its own so that cmd/harness (client construction) and `internal/queue`
(request validation) can both reach it without importing the agent loop.
Depends on: nothing internal.

### `internal/evals`
Measures a prompt change. Publishes a suite of tasks under two or more prompt
variants through the WORK stream, records the run and its members in
`eval_runs` and `eval_members` as it goes, scores each run from its stored
events, and
compares the arms. The optional judge speaks to a model provider through a
narrow `Client` seam declared here — one non-streaming completion — and
implemented by `internal/deepseek` and `internal/kimi`, with cmd/harness
resolving which one a judge model gets through `internal/provider`
(docs/KIMI-INTEGRATION.md §4.3); a boundary test pins that `internal/deepseek`
is never a direct dependency. Depends on: `internal/queue` to publish,
`internal/store` to read, `internal/wire` for the judge's request vocabulary.
[../docs/EVALS.md](../docs/EVALS.md).

### `internal/skills`
Scans each cloned repository for `.claude/skills/` and `.deepcode/skills/`, and
the workspace's own `skills/` (`WorkspaceSkillsDir`, created by
`internal/workspace`), and renders what it finds into a catalogue. The
workspace directory is how a skill reaches a session without being committed
to any repository the session is working in — and it sits under the workspace
root because the catalogue carries only descriptions, so the model opens each
`SKILL.md` with `Read`, which is workspace-confined. `Install` is what fills
that directory: it writes the shipped tree (`assets`, embedded in the binary)
into one prepared workspace, taking only directories that hold a `SKILL.md`,
and `internal/worker` calls it once the workspace exists. Neither discovery
nor installation ever fails a run. Depends on: nothing internal. §4.11.

### `assets`
The files the repository ships rather than runs, and the only Go package
outside `cmd/` and `internal/`: it sits at the root because `go:embed` cannot
reach outside its own package directory. `assets/skills` is for humans —
Claude Code skills for driving the harness from outside, copied by hand and
never loaded by a run. `assets/agent-skills` is embedded and is what
`internal/skills.Install` writes into every prepared workspace. Depends on:
nothing.

### `internal/claudemd`
Scans the workspace and each cloned repository for a root `CLAUDE.md` and
renders their contents into the opening user message, ahead of the skill
catalogue and the task, capped per file and in total. Discovery never fails a
run. Depends on: nothing internal.

### `internal/mcpclient`
The client side of MCP support ([`../docs/MCP.md`](../docs/MCP.md)) — not to
be confused with `internal/mcp` just below, the *server* this harness
exposes at `/mcp`; the two never meet, and the three letters they share are
the only thing they share. `Manager` implements `MCPProvider`, the narrow
seam `internal/tools` declares: `Definitions` is a pure function of the
`mcp_servers` table's stored snapshot — no server is dialled to build a
session's tool array — and `Call`, plus the `Refresh` probe, are the only
paths that open a connection, over a cache of one live session per server,
redialled when the row's connection configuration has moved on since the
cached session was dialled or a liveness ping says it has quietly died.
`cmd/harness` wires the one `Manager` per process into `internal/session`
through `MCPProvider` and into `internal/httpapi` through the narrower
`MCPProber` seam that package declares for itself, so probing from the
`/mcp-servers` screen never gives `internal/httpapi` a reach into `session` or
`worker`. Depends on: `internal/store` (the `mcp_servers` rows),
`internal/wire` (the tool array shape the request head carries),
`internal/tools` (the `MCPProvider` seam and its
`MCPContent`/`MCPImage`/`MCPServerOf` vocabulary), and the MCP Go SDK
(`github.com/modelcontextprotocol/go-sdk/mcp`).

### `internal/mcp`
The MCP launch server: tools and resources over streamable HTTP, mounted at
`/mcp` by `harness serve` on its own HTTP server and handed serve's own
queue, control token, settings resolver, and store. Backed by the work queue
and the harness's HTTP API (`GET /api/requests/{id}` for results). It opens
no database — the store handle it writes attachments through is serve's own,
so `serve` stays the single writer (docs/DATA-API.md, "attachments"). It
never touches the system prompt or the tool array.

### `internal/cache`
The prompt-cache churn diagnostic from [`../docs/CACHE.md`](../docs/CACHE.md):
predicts a sub-turn's cache miss from what the harness knows it appended and
names the first message that differs when prediction and reality diverge.
Diagnostic, not a request-path dependency.

### `internal/config`
Environment loading and `.env` parsing. Carries the bootstrap values only —
the data directory (which locates the database the settings themselves live
in), the network addresses, the price table path, and the workspace root —
plus the thinking toggle. Everything else (models, run budgets, worker sizes)
lives in the settings registry. Read configuration through here rather than
calling `os.Getenv` elsewhere.

### `internal/settings`
The registry and resolver for the `settings` table. The registry is one
ordered slice of descriptors — key, type (string/integer/duration), default,
validation bounds, description, and the secret/restart flags — covering every
setting: the API keys, the run budget, the tool limits, the model names, and
the restart-required operational limits. `ValidKeys` and `IsSecretKey` derive
from it; `Set` validates against it, so a value rejected by `harness config`
reads identically from the HTTP API and the screen. The resolver reads through
to the store on every call, so a key changed by another process takes effect
on the next request without restarting anything (restart-flagged keys are the
exception: they are read once at startup and marked as such). Depends on: the
settings surface of `internal/store` only.

### `internal/pricing`
The price table, loaded from JSON at runtime and carrying its own capture date.
Depends on: nothing internal. §4.9.

### `web/`
The React frontend — three screens: the session list, one session's
transcript, and the settings screen. It reads and writes the harness's data
and can start, steer, and stop a running session, all through the run-control
endpoints (docs/RUN-CONTROL.md). Its own build and test cycle; see
[`../web/CLAUDE.md`](../web/CLAUDE.md) for the constraints on changing it. §5.
