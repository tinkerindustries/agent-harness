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
Flag parsing and process wiring: `main.go` dispatches the three subcommands
and holds the model→client dispatch, `stdiosession.go` composes
`stdio-session`/`gemini-session` and `claudesession.go` composes
`claude-session` — each its own store, hub and `session.Runner` handed to
`internal/stdiosession`, with a client per provider behind that dispatch and
the parent-facing dialect chosen from the subcommand the process was spawned
as. Composition happens here and nowhere else; no `internal` package
constructs another's dependencies.

`stdio-session` hosts every Gemini model the repository routes, the one
DeepSeek model the repository routes, `deepseek-flash`
(`deepSeekSessionModel`), which reads images natively, and all three Claude
models; `claude-session` hosts the three Claude models alone, with
`ANTHROPIC_API_KEY` the one credential it reads. `deepseek-flash` reading
images natively is what keeps a DeepSeek session here to one credential: a
model that could not see would carry the vision tools built to compensate,
four of which reach Google
([`../docs/DEEPSEEK-VISION.md`](../docs/DEEPSEEK-VISION.md)). The Claude
models read images natively too, and their tool array
(`internal/tools.DefinitionsFor`) drops the harness's own `WebFetch` in
favour of Anthropic's own server-side `web_search` and `web_fetch`
([`../docs/ANTHROPIC-INTEGRATION.md`](../docs/ANTHROPIC-INTEGRATION.md)).
`claude-session`'s own `Runner.ToolTimeouts.HostTool` is set to a figure with
no natural ceiling: its client-declared tools resolve on an asynchronous
answer through `sessions.events` rather than a bounded call, the one
`internal/tools` timeout this repository ever widens per subcommand
(`internal/tools.Timeouts.HostTool`, `../docs/STDIO-MANAGED-AGENTS.md`, "The
seam"). Nothing but protocol frames may reach stdout, so the process logs to
stderr and reads no `.env` of its own.

### `internal/deepseek`
The DeepSeek API client, speaking **two** surfaces at one base URL: Chat
Completions (`intent.go`, `stream.go`, `client.go`) and the Responses API
(`responses.go`, `responsestypes.go`, `responsesstream.go`,
`responsesclient.go` — docs/DEEPSEEK-RESPONSES.md). `ResponsesClient` embeds
`*Client` rather than reimplementing it, because the retry classification,
the usage split, the cache slack and both quirk repairs are the provider's
and not the surface's; what it replaces is the two request methods, the body
they send and the frames they read. A session speaks one surface for its
whole life — the two dialects do not serialise alike and the request head is
the frozen cache prefix (docs/DESIGN.md §3.2). The rest of this entry
describes the Chat Completions half: the request body it builds from a
`wire.ChatIntent`,
the auxiliary endpoints (`/models`, `/user/balance`), the error body, retry
classification, and the narrow repairs for the quirks recorded in
docs/OBSERVED.md (the misplaced brace in large arguments objects, and
reasoning starvation under a small max_tokens budget). `modelinfo.go` is the
per-model descriptive tables — advertised reasoning efforts, context window,
display name — this provider's answer to `internal/gemini/thinkinglevels.go`,
read by `harness stdio-session`'s handshake (docs/STDIO-PROTOCOL.md,
`model_details`) and by nothing on the request path. Implements the narrow
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
idle watchdog. `PumpStreamWith` takes the frame decoder as an argument —
chat completion chunks ending at `[DONE]`, or DeepSeek's semantic Responses
events ending at `response.completed` — so the watchdog, the
context-guarded sends and the line reader that cannot be stranded are
written once for both. Carries no provider dialect — no request shape, no usage
mapping, no error-body parsing, no quirk repairs — so each provider keeps its
own `Client` type satisfying the narrow `session.Client` seam independently;
this package only removes the near-verbatim duplication two full client
implementations used to carry (docs/KIMI-INTEGRATION.md §4.1). `Transport.Do`
has a third caller, `internal/gemini`, which supplies `SetAuth` to send its
`x-goog-api-key` header instead of the `Authorization: Bearer` `Do` sends by
default (docs/GEMINI-INTEGRATION.md §8) — `Transport.PumpStream` stays
DeepSeek's and Kimi's alone, since it decodes `wire.ChatCompletionChunk`, a
shape Gemini's SSE frames don't carry. Depends on: `internal/wire`.

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
`thinking` field. Knows nothing of sessions, tools, or storage. Depends on: `internal/wire`, `internal/providerhttp`.

### `internal/gemini`
Google's Gemini API client, hand-rolled rather than the official SDK because
the SDK targets the legacy `generateContent` surface, not
`POST /v1beta/interactions` (docs/GEMINI-INTEGRATION.md §2). Two callers: the
vision tools — `Glance`, `Ground`, `Detect` (docs/TOOLS.md) — through
`Interact`, whose request and response shapes are unchanged since before this
package spoke chat completions at all; and the agent loop, through
`StreamChatCompletion` and `CreateChatCompletion`, the same narrow `Client`
seam `internal/deepseek` and `internal/kimi` implement. Owns the parts of the
Interactions surface with no counterpart in the OpenAI-format dialect the
other two share: `thought` steps carrying an opaque, mandatory signature that
must be replayed verbatim (`wire.Message.ThoughtSignature`,
docs/GEMINI-INTEGRATION.md §5.2), a request shape typed by step kind rather
than a flat message array, `arguments` as a genuine JSON object rather than a
string, and errors that can arrive SSE-framed even under a plain 400. All
three HTTP-issuing methods, `Interact` included, now retry a transient
429/500/503 with backoff through `internal/providerhttp.Transport`
(`chatTransport`, docs/GEMINI-INTEGRATION.md §8) via `Transport.SetAuth`, the
seam that lets this provider's `x-goog-api-key` header ride the same
`Transport.Do` DeepSeek's and Kimi's `Authorization: Bearer` do. `PumpStream`
is not shared — it decodes `wire.ChatCompletionChunk`, the OpenAI-format
chunk shape, and Gemini's SSE frames carry a different vocabulary entirely —
so `stream.go`'s own `pumpChatEvents` still reads the body `Transport.Do`
retried into existence. Request and response bodies are Go
structs, never `map[string]any`, for the same byte-stability reason as the
other two clients. Depends on: `internal/wire`, `internal/providerhttp` (just
`Transport`, for retry-with-backoff — never `PumpStream`).

### `internal/anthropic`
The Anthropic client, hand-rolled the same way `internal/gemini` and
`internal/deepseek` are: `POST https://api.anthropic.com/v1/messages`, Go
structs rather than `map[string]any` for byte-stable requests. Hosts three
models — `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-fable-5-1`
(`modelinfo.go`). Implements the narrow `Client` seam `internal/session`
declares: `intent.go`'s `requestFromIntent` renders the system prompt into
top-level `system`, items into `user`/`assistant` messages, and up to three
cache breakpoints (`applyCacheBreakpoints`); mid-conversation system items
render as a system message on the models `systemMessageModels` names and as a
user text block on any other. Owns the one shape with no counterpart
in either OpenAI-format dialect: a whole response's assistant `content`
array captured verbatim as `wire.EventProviderBlocks` and replayed
byte-for-byte on the next request, because Anthropic's preserved-thinking
check needs every `thinking` and `redacted_thinking` block back in original
order alongside blocks (`server_tool_use`, `*_tool_result`) `wire.Item` has
no field for — DeepSeek, Kimi and Gemini never set this event and their
goldens pin that unchanged. A turn that left a server tool open beside a
client call must be followed by tool results alone, so `intent.go`'s
`holdBehindOpenServerTools` moves any later user or system message past the
turn that closes it, and the client implements `session.ServerToolHolder` so
the loop defers steers and reminders at such a boundary
(`docs/ANTHROPIC-INTEGRATION.md`, "Server tools left open across a tool
round"). `stream.go`'s `readSSE` is this client's own SSE
decoder for the surface's named-event vocabulary
(`message_start`/`content_block_start`/`content_block_delta`/`message_delta`/
`message_stop`), wrapped by `client.go`'s `streamOneRound` in a
`providerhttp.StreamOpener`/`StreamPump` pair so a connection that dies
after a 200 reopens on `providerhttp.Transport.RetryStream`'s backoff, per
HTTP round rather than around the whole `pause_turn` resume loop —
`pause_turn` (Claude wanting another round of server-tool use) resumes
internally, bounded, so the loop always sees one logical response.
`errors.go` retries 429, the 5xx faults, and 529 (`overloaded_error`); a
`stop_reason: "refusal"` ends the turn with `*RefusalError`. Depends on:
`internal/wire`, `internal/providerhttp` (`Transport`, `SetAuth` for
`x-api-key`, and `RetryStream`). `docs/ANTHROPIC-INTEGRATION.md` is the
provider reference.

### `internal/stdiosession`
The protocol `harness stdio-session`, `harness gemini-session` and `harness
claude-session` speak: JSON-RPC 2.0 over stdin and stdout, carrying a
vendor's own vocabulary rather than one of this repo's invention. Three of
them, behind one `Dialect` seam. `responses.go` is the OpenAI Responses
API's — its REST methods on `POST /responses` as JSON-RPC methods and its
semantic server-sent events as notifications (docs/STDIO-PROTOCOL.md), the
same vocabulary `internal/deepseek` sends the provider and the same one the
loop itself folds into (`wire.Item`). `interactions.go` is Google's
Interactions API's — its methods on `POST /v1beta/interactions` and its step
events (docs/STDIO-INTERACTIONS.md), the same vocabulary `internal/gemini`
sends Google. `managedagents.go`, `managedagentstranslate.go`,
`managedagentswire.go` and `managedagentsevents.go` are Anthropic's Managed
Agents API's — `sessions.create`/`.get`/`.delete` and one `sessions.events`
covering steering, interrupting and answering a custom tool call
(docs/STDIO-MANAGED-AGENTS.md) — a parent-facing vocabulary only: nothing
here calls Anthropic's real `/v1/sessions`, and the loop underneath still
calls the plain Messages API through `internal/anthropic`. `cmd/harness`
picks one from the subcommand, which is where it has to be picked:
`initialize` already answers with a protocol string, a capability named for
its own continuation id, and each model's effort set under its own key, so
there is nothing left to negotiate afterwards.

None is a proxy. The loop runs the tools, so what a parent reads is rendered
from the event log rather than forwarded, and a create's `input` is read for
its text alone. `server.go` names no wire type of any vocabulary: it decodes
into `CreateRequest` and keeps a run's neutral facts, and a `Translator` per
run turns the committed events plus the hub's live text deltas into that
vocabulary's frames and holds the document they assemble into. It streams
text from the live frames and takes structure — tool calls, results, thought
signatures, usage, the run's end — from the log, which is why a reasoning
item and a message item can be open at once here and never are on any HTTP
surface's own stream.

The three vocabularies are not interchangeable in every respect. Only the
Interactions one carries a **thought signature**, the receipt Google issues
for a thinking step; the loop replays it to Google either way, but a client
that stores transcripts meaning to replay them elsewhere can only get it from
`gemini-session`. Only ManagedAgents addresses a **session** rather than a
run — `Dialect.AddressID` and `Dialect.AddressesSession` are the seam that
lets `Server.get`/`.delete`/append's own result echo resolve either way — and
only it delivers a client-declared tool asynchronously:
`agent.custom_tool_use` fires as an ordinary notification, the session goes
idle with `stop_reason: {type: "requires_action"}`, and
`user.custom_tool_result` through `sessions.events` resumes the turn.
`hostTools.Call` blocks on a small map-plus-channel registry
(`managedagentsevents.go`, `Server.customPending`/`.customReady`) instead of
on `conn.Call` under this dialect alone; `Server.beginRun` factors create's
own run-starting tail out so `sessions.events`' `user.message` on an idle
session can start the next turn the identical way, reusing the same
per-session `hostTools` a session's first turn built rather than rebuilding
one, since nothing on this dialect's own wire ever re-declares tools.

Also implements `tools.MCPProvider` for the two tool shapes a client may
declare, `function` (called back over the pipe, or — under ManagedAgents —
answered asynchronously) and `mcp_server` (dialled by `internal/mcpclient` as
any configured server is); both are spelled the same in every vocabulary, so
`Tool` is one type and the tool path sees no dialect beyond the one branch in
`Server.buildTools` that picks `hostTools.call` or `hostTools.async`.
`modelinfo.go` is the one file here that knows a model has a provider at all:
the handshake's per-model details and the create's effort check are answered
out of `internal/gemini`'s, `internal/deepseek`'s or `internal/anthropic`'s
tables, so the providers' differing effort sets reach a client as data rather
than as a special case anywhere else. `resume.go` is the seam between the two
ways a create names a conversation: a run id is minted in memory and dies
with the process, so continuing across a restart goes by session id out of
the `-state-dir` store instead, and everything the session's prompt prefix is
built from — model, workspace, permission mode, deny patterns, and the frozen
tool array the create has to re-declare with live connection metadata — is
checked against the row rather than taken from the create. Depends on:
`internal/session`, `internal/store`, `internal/hub`, `internal/tools`,
`internal/mcpclient`, `internal/provider`, and — in `modelinfo.go` alone, for
the descriptive tables the handshake publishes — `internal/gemini`,
`internal/deepseek` and `internal/anthropic`.

### `internal/attachment`
Validates one image attachment a client submitted before its bytes reach the
store: the name must be a plain file name (the file is written under it, so a
path-shaped name would be a way out of `scratch/attachments/`), the extension
must be one of the image types `ReviewScreenshot` accepts, an asserted MIME
type must match the extension, and the decoded bytes must fit the per-file
cap. Also carries the image-extension-to-MIME-type table the vision tools
(`internal/tools/vision.go`) agree on — distinct from
`internal/tools/screenshot.go`'s narrower `screenshotOutputExtensions` (no
WebP, because that one names what Chromium's capture can produce, not what
the harness can read back in). It owns `WorkspaceDir`, `WorkspacePaths` and
`Write`: the one spelling of `scratch/attachments/`, and the confinement
check that writing into it goes through, whether the images arrived with the
opening message or were pasted into a session that is already running. A leaf
by design, depending on nothing internal, which is what lets every package
that has to agree on that one string read it here.
Depends on: nothing internal.

### `internal/wire`
The provider-neutral wire vocabulary every request path speaks. `item.go` is
the loop's own conversation form — `Item`, the Responses API's input-item
shape, which `internal/fold` produces and `internal/deepseek`'s Responses
client serialises unchanged; `messages.go` renders it back into a messages
array for the two Chat Completions dialects, byte for byte what they were
always sent. The rest
is the older dialect's own: the message and tool types, the request and response bodies, the streaming chunk types,
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
of tool results, compaction, and resume — the last of which continues a
session that has already ended
([`../docs/RUN-CONTROL.md`](../docs/RUN-CONTROL.md), "Continuing"). The
widest dependency set in the repo,
deliberately — this is where everything meets. Its reach to the model API is
through a declared seam rather than an import: `Client`, a narrow interface
declared here and implemented by `internal/deepseek`, `internal/kimi`, and
`internal/gemini`, each of which turns the loop's `wire.ChatIntent` into its
own provider's request shape and owns that provider's usage mapping and
response quirks — the same shape `RunPublisher` and `RunController` take,
with cmd/harness choosing the implementation when it builds the Runner
(docs/KIMI-INTEGRATION.md §4.1, docs/GEMINI-INTEGRATION.md §5.1). Owns the
session row's lifecycle around a caller's preparation window: `Create`
inserts it as `creating` before the workspace is ready, `FailSetup` moves it
to `failed` with an error event when preparation fails, and `Run` promotes a
pre-created row to `running` (or inserts when there is none). §4.5, §4.6.
`Runner`'s five jobs split by file, all
on the same type (`runner.go`'s own package doc names which file holds
which): `RunOptions` and the `Runner` type stay in `runner.go` beside the
settings accessors; `lifecycle.go` is `Create`/`FailSetup`/`Run` and the loop
that drives a run to a terminal result; `sinks.go` is where a sub-turn's
output goes (the disk mirror, the hub); `tooldispatch.go` executes a
sub-turn's tool calls; `livestate.go` persists the plan and recent-calls
roll. MCP support (`mcp.go`, [`../docs/MCP.md`](../docs/MCP.md)) is resolved
once, not read live: `resolveMCPDefinitions` asks the `Runner`'s own `MCP
tools.MCPProvider` seam — nil for a caller with none wired, every existing
test among them — for the enabled servers' tool array and
per-server readonly map, and `Run` and `Resume` each call it exactly once and
freeze the result onto the row's `tool_schema` and the policy's
`MCPReadOnlyServers`, so a server an operator toggles mid-run cannot change
what a running session sends. `attachments.go` is the one thing this package
writes to a workspace it did not create: the images a person pasted into the
composer, loaded from the store by id and materialised into the live
workspace by `pickUpSteers` (at the boundary the steer is applied) and by
`Resume` (before the continuation is appended), so the file always exists
before the message naming it reaches the model
([`../docs/RUN-CONTROL.md`](../docs/RUN-CONTROL.md), "Images in the
composer").

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
`Executor.ExtraEnv` is the one hook that lets something outside decide part
of a `Bash` call's environment, resolved at the moment of the call, for a
value that cannot be process-wide because it differs per workspace. Depends
on:
`internal/wire`, `internal/provider`, `internal/promptvariant`,
`internal/attachment` (the image-extension-to-MIME-type table the vision
tools read images by).

### `internal/fold`
Folds the event log into the wire `messages` array (`internal/wire`'s
`Message`, the shape every provider's Chat-Completions-style request is
rendered from). Pure, append-only, a switch on event kind. §4.1.

### `internal/store`
SQLite (`modernc.org/sqlite`, pure Go, WAL) plus the derived disk mirror under
`<data dir>/sessions/` and diff computation. The database is authoritative;
the mirror is written alongside it as a session goes and never rebuilt from
the database after the fact (docs/DESIGN.md §4.8). The `settings` table holds
the harness's stored configuration — the DeepSeek API key among it — written
and read through `internal/settings`. Owns the session status vocabulary:
`StatusRunning`, `StatusCreating` (the window while a run's workspace is
being prepared), the terminal statuses, and `IsLive` — every
branch and SQL predicate that means "this session is live" builds off it,
never a string literal. Split by concern, `store.go`'s own package doc names
which file holds which: the `Store` type and the single-writer loop stay in
`store.go`; `errors.go` is the error vocabulary; `schema.go` the SQL schema
and column migration; `sessions.go` the status vocabulary and session CRUD — including
`mcp_read_only`, the copy of the per-server read-only allowance a run froze,
kept beside `permission_mode` and `deny_patterns` so a caller that must hold a
resume to what the session started with has it;
`events.go` the event payload types and the append-only log's queries;
`mcp.go` is the `mcp_servers` table
([`../docs/MCP.md`](../docs/MCP.md)) — CRUD plus `SaveMCPProbe`, whose one
asymmetry is the whole point of the table: a successful probe overwrites
`tools_json` and `instructions`, a failed one only ever writes
`probe_error`, so a session resolving its tool array from a stored row never
sees it shrink because a server happened to be unreachable the moment that
row was read. Depends on:
nothing internal. §4.8.

### `internal/hub`
In-process fan-out: per-session transcript subscribers and a quieter
session-list subscriber set. Fed the same events a session appends. Owns the
two shapes a session row goes out in, and the projection between them:
`SessionState`, the whole row, which one session's own stream carries as
`state` frames, and `ListRow`, what the list feed sends. `ListRowOf` is
the only place the two meet, so a field reaches the list feed by being added
there on purpose. §4.2, §5.8.

### `internal/promptvariant`
The named alternatives to the shipped system prompt, and the reminder cadences
that go with them. Imports nothing internal but `internal/wire`, so a variant name can be
validated without pulling the agent loop in behind it. `internal/session` owns
the prompt text; this package owns the edits to it.

### `internal/provider`
The one model→provider table (docs/KIMI-INTEGRATION.md §4.3): `ModelFor`
maps a model name to the provider serving it, with no default — an unknown
model is an error, so request validation rejects it loudly instead of
silently routing to a provider. Four providers today: DeepSeek
(`deepseek-flash`), Kimi (`kimi-k3`), Gemini (`gemini-3.7-flash`,
docs/GEMINI-INTEGRATION.md §7 Phase 5), and Anthropic (`claude-opus-5-5`,
`claude-sonnet-5-5`, `claude-fable-5-1`, docs/ANTHROPIC-INTEGRATION.md). It
also carries `SeesImages`, the
one model→capability table for native vision — keyed by model rather than
by provider, since a future DeepSeek model could disagree with
`deepseek-flash`'s own vision capability the way `deepseek-v4-pro` used to
before this harness dropped it (docs/DEEPSEEK-VISION.md). It is a package of
its own so that client construction and request validation can both reach
it without importing the
agent loop.
Depends on: nothing internal.

### `internal/skills`
Scans each cloned repository for `.claude/skills/` and `.deepcode/skills/`,
and the workspace's own `skills/` (`WorkspaceSkillsDir`), and renders what it
finds into a catalogue. The workspace directory is how a skill reaches a
session without being committed to any repository the session is working in —
and it sits under the workspace root because the catalogue carries only
descriptions, so the model opens each `SKILL.md` with `Read`, which is
workspace-confined. Discovery never fails a run. Depends on: nothing
internal. §4.11.

### `internal/claudemd`
Scans the workspace and each cloned repository for a root `CLAUDE.md` and
renders their contents into the opening user message, ahead of the skill
catalogue and the task, capped per file and in total. Discovery never fails a
run. Depends on: nothing internal.

### `internal/mcpclient`
The client side of MCP support ([`../docs/MCP.md`](../docs/MCP.md)): an
operator registers an external MCP server and its tools join the session's
array. `Manager` implements `MCPProvider`, the narrow
seam `internal/tools` declares: `Definitions`, `Instructions`, `Resources`
and `Prompts` are pure functions of the `mcp_servers` table's stored
snapshot — no server is dialled to build a session's tool array, to quote
what a server said about itself at initialize into the opening message, or
to list what it holds — while `Call`, `ReadResource`, `GetPrompt`,
`Complete`, and the `Refresh` probe are the paths that open a connection, over a cache of one live session per server,
redialled when the row's connection configuration has moved on since the
cached session was dialled or a liveness ping says it has quietly died.
`cmd/harness` wires the one `Manager` per process into `internal/session`
through `MCPProvider`. Depends on: `internal/store` (the `mcp_servers` rows),
`internal/wire` (the tool array shape the request head carries, and the
chat intent a server's sampling request runs as),
`internal/tools` (the `MCPProvider` seam and its
`MCPContent`/`MCPImage`/`MCPServerOf` vocabulary), and the MCP Go SDK
(`github.com/modelcontextprotocol/go-sdk/mcp`).

### `internal/cache`
The prompt-cache churn diagnostic from [`../docs/CACHE.md`](../docs/CACHE.md):
predicts a sub-turn's cache miss from what the harness knows it appended and
names the first message that differs when prediction and reality diverge.
Diagnostic, not a request-path dependency.

### `internal/config`
`.env` parsing, and nothing else. The process is configured by the parent that
spawns it — credentials in the environment, everything else on the command
line — so a `.env` is a convenience for a person running the binary by hand.

### `internal/settings`
The registry and resolver for the `settings` table. The registry is one
ordered slice of descriptors — key, type (string/integer/duration), default,
validation bounds, description, and the secret/restart flags — covering every
setting: the API keys, the run budget, the tool limits, the model names, and
the restart-required operational limits. `ValidKeys` and `IsSecretKey` derive
from it; `Set` validates against it. The resolver reads through to the store
on every call, so a key changed by another process takes effect on the next
request without restarting anything (restart-flagged keys are the exception:
they are read once at startup and marked as such). Depends on: the
settings surface of `internal/store` only.

### `internal/androiddns`
Redirects Go's resolver to real name servers when a `CGO_ENABLED=0` build runs
on Android, which has no `/etc/resolv.conf`. `cmd/harness` installs it at
startup, and each dial re-reads Termux's resolv.conf when a stat shows it
changed, so a network switch reaches a running process; it changes nothing on
any other GOOS
([`../docs/ANDROID.md`](../docs/ANDROID.md)). Depends on: nothing internal.

### `internal/pricing`
The price table, loaded from JSON at runtime and carrying its own capture date.
Depends on: nothing internal. §4.9.

