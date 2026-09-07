# MCP servers

How the harness reaches tools that are not its own: an operator registers a
Model Context Protocol server once, globally, and every session started after
that carries the server's tools in its tool array alongside the built-in
twenty.

This is the client side. It is not to be confused with `internal/mcp`, which
is the *server* the harness exposes at `/mcp` so an external agent harness can
launch runs here. The two never meet: `internal/mcp` publishes work,
`internal/mcpclient` consumes tools.

That collision is also why the browser screen lives at `/mcp-servers` and not
at `/mcp`: `harness serve` mounts the outward-facing MCP server on `/mcp` of
the same port, so a screen routed there is simply unreachable — the browser
gets that endpoint's `Bad Request: GET requires an Mcp-Session-Id header`
instead of the app. Do not "tidy" the route back.

## The shape of it

```
browser /mcp-servers screen ─POST/PATCH/DELETE─▶ internal/httpapi ──▶ mcp_servers table
                                                │                     │
                                          refresh (probe)             │ snapshot
                                                ▼                     ▼
                                        internal/mcpclient.Manager ◀───┘
                                          │            │
                                    stdio/HTTP     Definitions() / Call()
                                          ▼            ▼
                                    the server   internal/tools.Executor
                                                       ▲
                                                 internal/session
```

Four decisions carry the design.

**Configuration is global and lives in the database.** One `mcp_servers` row
per server, no per-repository or per-request scoping. A run does not choose
its servers; the operator does, and every run started afterwards gets the same
set. That is what makes enable/disable a single toggle with an obvious
meaning.

**The tool array is built from a stored snapshot, never from a live
connection.** Each row carries `tools_json`, the tool list as it was read from
that server the last time a probe succeeded. A session assembling its array
reads rows, not sockets. A server that is enabled but unreachable at the
moment a session starts therefore contributes the tools it contributed last
time, rather than silently shrinking the array — which matters because the
array is the frozen request head (ARCHITECTURE.md, "The request head is
frozen"): two sessions whose arrays differ share no prompt cache, and a
resumed session whose array shrank under it would invalidate its own prefix.
A server that has never probed successfully contributes nothing, and the
screen shows the error.

**Resolution happens once per run.** `Runner.Run` resolves the array — the
provider's frozen definitions plus the MCP snapshot — and stores it on the
session row as `tool_schema`, as it always has. Every request in that run
sends that array, and `Runner.Resume` reads it back from the row rather than
re-resolving. A server toggled mid-run cannot change what the run is sending.

**A call goes out through the same permission gate as everything else.** MCP
tools reach outside the workspace by definition, so `readonly` sessions do not
get to call them unless the server itself is marked `allow_readonly`. The
decision is made in Go at the moment of the call, from a map frozen on the
policy at run start — never from a live read of the table.

## Naming

A tool from server `blender` called `get_objects_summary` is offered to the
model as `mcp__blender__get_objects_summary`: the `mcp__` prefix, the server
name, `__`, the tool's own name. It is the convention Claude Code uses, which
means models have seen it.

- Server names match `^[a-z0-9][a-z0-9_-]{0,31}$`, validated on write, so the
  prefix can never introduce a character the model API rejects in a function
  name.
- A server name may not contain `__`, validated on write for a second reason:
  `__` is what separates the server from the tool, so a server called
  `foo__bar` would make `mcp__foo__bar__tool` mean two different things at
  once. Everything that reads a server back out of a qualified name — the
  per-server read-only allowance above all — needs there to be one answer. A
  *tool* name may contain `__` and stays unambiguous, because the server is
  whatever precedes the first one.
- A tool name from the server is sanitised to `[A-Za-z0-9_-]`, every other
  character becoming `_`.
- The whole name is capped at 64 characters, the function-name limit both
  providers enforce. A name that would exceed it keeps the prefix, truncates
  the tool part, and appends `_` plus the first six hex characters of the
  SHA-256 of the original name, so truncation stays deterministic and two
  long names that share a prefix stay distinct.
- Within one server, a collision after sanitising is resolved the same way.
- Ordering is deterministic: servers by name, then tools by their name as the
  server reported it. MCP definitions are appended *after* the built-in array,
  so the built-in portion's bytes are unchanged by any MCP configuration.

The qualified name is computed exactly once, by a probe, with the whole
server's tool list in hand — never re-derived when the array is built. That
is what makes collision handling run once per probe rather than possibly
differently on every read, and it is what makes the tool array a pure
function of the stored rows (`internal/store`'s `MCPToolSnapshot.QualifiedName`):
building a session's array is a matter of reading `tools_json` back, with no
dial and no chance of two reads of the same unchanged snapshot disagreeing.

## The table

```sql
CREATE TABLE IF NOT EXISTS mcp_servers (
	name           TEXT PRIMARY KEY,
	transport      TEXT NOT NULL,               -- 'stdio' or 'http'
	command        TEXT NOT NULL DEFAULT '',    -- stdio: the executable
	args           TEXT NOT NULL DEFAULT '[]',  -- stdio: JSON array of strings
	env            TEXT NOT NULL DEFAULT '{}',  -- stdio: JSON object of strings
	url            TEXT NOT NULL DEFAULT '',    -- http: the endpoint
	headers        TEXT NOT NULL DEFAULT '{}',  -- http: JSON object of strings
	enabled        INTEGER NOT NULL DEFAULT 1,
	allow_readonly INTEGER NOT NULL DEFAULT 0,
	allow_sampling INTEGER NOT NULL DEFAULT 0,  -- may it spend model tokens?
	tools_json     TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	resources_json TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	prompts_json   TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	instructions   TEXT NOT NULL DEFAULT '',    -- last successful probe
	stale          INTEGER NOT NULL DEFAULT 0,  -- the server says they moved on
	probed_at      TEXT NOT NULL DEFAULT '',
	probe_error    TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1
);
```

The four snapshot columns — `tools_json`, `resources_json`, `prompts_json`,
`instructions` — are only ever written by a successful probe, and a failed
probe writes `probe_error` and leaves the snapshot alone. That
asymmetry is the whole point of the columns: what a session builds must not
depend on whether a subprocess happened to start this minute. The two are
written in one statement, because they come from one handshake — a session
must never read this probe's tools under an earlier probe's instructions.

`instructions` is what the server sent in its `InitializeResult`, capped at
32 KB. A row written before the column existed backfills to `''`, which is
indistinguishable from a server that sends none; the operator's next Refresh
fills it in.

Secrets: `env` values and `headers` values can hold API keys. `GET
/api/mcp/servers` masks them the way the settings surface masks a secret key
(`internal/redact.Secret`) — keys in the clear, values masked. A write that
sends an empty string as a value keeps the stored value for that key; removing
the key removes the value. The full values never leave the process.

## Probing

A probe connects, initialises, and reads everything the server advertises:
`tools/list` to the end of pagination, then `resources/list`,
`resources/templates/list` and `prompts/list` — each only when the server's
own capabilities say it has one. That guard is not an optimisation. A
tools-only server, which is most of them and includes the official Blender
server, answers `resources/list` with method-not-found, and a probe that
asked anyway would turn every such server into a failed probe with a
baffling reason.

It writes the result in one statement: the three lists, `instructions` from
the handshake it just completed, `probed_at`, `stale` cleared, and
`probe_error` cleared. It runs when a server is
created, when its connection details change, and when the operator presses
Refresh. Nothing probes on a schedule — an operator who changes nothing gets a
tool array that never changes underneath them.

`transport: "stdio"` launches `command` with `args` and `env` and speaks over
the child's stdin/stdout (`mcp.CommandTransport`). `transport: "http"` speaks
streamable HTTP to `url` with `headers` (`mcp.StreamableClientTransport`).

A dial is given minutes rather than the seconds a bare process start or TCP
connect would need, because two slow things compound on a first run: a
server launched as `uvx <package>` or `npx <package>` resolves and installs
before it ever speaks MCP, and a server that drives an external application
may talk to that application during startup, before it will answer
`tools/list` at all. The two bounds are deliberately unequal — the dial's
own is shorter than the probe budget around it — so the inner one fires
first and the row records what the dial was waiting for instead of a bare
`context deadline exceeded`.

A failed probe's reason is written on a fresh context, not the one that just
expired. This is the same rule `Runner.FailSetup` follows for a run's
terminal bookkeeping, and for the same reason: a deadline is the most common
way a probe fails, so the caller's context is already dead by the time there
is something to record, and a write through it leaves the row saying nothing
about a probe that plainly did not work.

`Refresh` always dials fresh — bypassing whatever is cached, since the
point of an explicit probe is to prove the *current* configuration actually
connects, not that some earlier connection is still alive.

A probe failure is never an error to the caller of the HTTP surface: create
still answers 201 and refresh still answers 200, both carrying the row with
`probe_error` on it. The screen renders that reason, and replacing it with
a 5xx would take away the one thing an operator can act on.

## Connections

`internal/mcpclient.Manager` holds one live session per server, shared by
every session in the process, so two runs calling the same server's tools in
close succession reuse one live process or HTTP session instead of paying a
fresh dial each time. A cached session is handed back only after two checks:
its fingerprint — a deterministic encoding of the row's transport, command,
args, env, url, and headers — still matches a fresh read of the row (an
edited server drops its cached connection and redials on its very next call,
without needing a restart), and a 5-second ping against the cached session
still succeeds (a server that has quietly died gets redialled rather than
handed to a caller as a session that will fail on first use). Neither check
holds the manager's lock across the network call, so one slow or wedged
server cannot stall every other server sharing the same manager.

## Calling

`internal/tools.Executor` routes a call whose name starts with `mcp__` to the
`MCPProvider` seam rather than to `toolFuncs`. What comes back is flattened
into the harness's own `Result`:

- Text content blocks join with a blank line between them.
- An image content block is written into the workspace at
  `scratch/mcp/<server>-<tool>-<n>.<ext>` and reported by path, so the
  existing vision tools can look at it and the transcript can show it. `n` is
  a running per-session counter, not a per-call index, so two separate calls
  to the same tool never collide on the same path. On a provider that reads
  images natively the first image also rides back as `Result.ImageURL`.
- `StructuredContent` with no text content is marshalled as JSON.
- `isError` from the server becomes `Result.IsError` — a tool that failed is
  a result the model can route around, never a failed run.
- The output cap and truncation label are the harness's own
  (`tools.output_cap`).

A call to a server that will not connect returns an error result naming the
server and the reason. A run never fails because an MCP server is down.

`tools.mcp_timeout` (default 120s) bounds one call. It is longer than the
30-second default because the calls that motivated this — rendering a
viewport, driving an external application — routinely are.

## Resources

A resource is data a server exposes for reading — a file, a record, a
document. A *resource template* is the same thing with `{placeholders}` in
its URI, a shape to be filled in rather than something readable as it
stands. Both are snapshotted by the probe into `resources_json`.

They are **not** tools on the model's array, and that is the whole design.
The array is frozen for the life of a run, so a server holding a
documentation set would either flood every request with hundreds of entries
or change the array whenever its contents changed — invalidating the
prompt-cache prefix of a session already running. Instead, four fixed tools
appear whenever a session has any MCP tools at all:

| Tool | What it does |
| --- | --- |
| `MCPListResources` | lists resources and templates from the stored snapshot |
| `MCPReadResource` | reads one, live, by server and URI |
| `MCPListPrompts` | lists prompts and the arguments each takes |
| `MCPGetPrompt` | renders one, live |

Four definitions, no matter how much is behind them. Listing reads the
snapshot — so an unreachable server still lists what it had; reading dials,
because the contents are the point and a stale copy would be worse than an
error.

`MCPReadResource` refuses a URI containing `{}`. Reading a template
literally may well *succeed*, returning the server's answer for a record
called `{id}` — a plausible wrong answer the model cannot tell from a right
one.

## Prompts

A prompt is a message template its server composed for a task it supports,
with named arguments. Snapshotted into `prompts_json` and reached through
`MCPListPrompts` / `MCPGetPrompt`, for the same reason resources are. A
prompt is listed with its arguments always: a prompt named alone is a prompt
the model can only call wrong.

`GetPrompt` flattens the messages a server returns into text with each
message's role in front of it. The roles stay because a prompt is a
conversation the server composed, and running two speakers together loses
where the turn changed.

## Roots

Roots are the client's answer to "which directories are you working in",
offered to every server. This harness answers with the workspaces of its
live sessions, read from the `workspace_leases` table — one row per running
session, acquired at start and released at the end.

They follow the *process*, not any one session, and they have to: a server
is dialled once and shared by every session in this process, so it cannot be
told a different set per caller. Claiming otherwise would be a lie told per
tool call.

A cached connection's roots are updated in place when that set changes,
which sends the server a `roots/list_changed` notification. It is deliberately
not a redial: tearing down a `uvx`-launched subprocess every time a session
starts would spend seconds of process startup to deliver one line of
bookkeeping.

## Sampling

Sampling is a server asking the *harness* to run a model turn on its behalf.
It runs on the flash model, non-thinking, capped at 4000 output tokens
regardless of what the server asked for — a server naming its own token
budget is a server spending somebody else's money, so its request is a
ceiling to lower, never an instruction to follow.

Two separate gates stand in front of it:

1. **Is a model wired at all?** `Manager.Sampler` is nil on the CLI paths and
   in every test, and a server that asks is told so.
2. **Is *this* server allowed?** `allow_sampling` is off by default, per
   server, and an operator turns it on. Connecting a server and letting it
   spend your tokens on prompts it wrote are different decisions, and only
   one of them is implied by pressing Add.

A refusal is an *error*, not an empty completion. The server asked for
something it did not get, and telling it so lets it fall back; answering with
silence dressed as a model turn would be a lie it cannot detect.

## Elicitation

Elicitation is a server asking the *user* a question mid-call. This harness
always declines, and that is the design rather than a stub: work here arrives
on a durable queue and runs unattended, so the operator who submitted it is
not sitting in front of a form and may not be awake. The protocol has a word
for exactly this situation. A server that is declined can take its other
path; a server left waiting would hang a queued run behind a question nobody
will ever see. The ask is logged in full, because a server asking for input
is telling the operator something about how it expects to be driven.

## Completions

`POST /api/mcp/servers/{name}/complete` asks a server what values an argument
could take — `{"kind":"prompt"|"resource","ref":…,"argument":…,"value":…}`.
It is an operator-surface endpoint only: nothing in a run calls it, because a
model does not autocomplete. A server without the capability answers with an
error, which surfaces as a 502 naming the server rather than a 500 that reads
like the harness broke.

## What a server sends back unasked

Four things arrive on a connection that are not replies to a request, and
they route differently because only one of them can be tied back to a caller.

**Progress.** A notification names the call it belongs to, via a token this
client attaches when — and only when — something is listening. It is
streamed into the transcript as `tool_stdout`, the same channel a running
`Bash` command's output uses, so a three-minute render shows progress
instead of looking like a hang. A token is not issued for a call nobody is
watching: asking a server to narrate itself into a log nothing reads is
traffic for its own sake.

A straggler that arrives after the call has answered goes to the harness log
instead. That is ordinary rather than exceptional — the reply can reach the
caller while the last notification is still queued behind it — and holding
the sink open to catch it would put "still working" into a transcript that
has already shown the work finish.

**Log messages.** `logging/setLevel` is sent at `info` on connect, to servers
that advertise the capability. The messages go to the harness log under the
server's name, beside the stderr a stdio server already writes there. They
carry no request correlation, so there is nowhere else honest to put them.

**List-changed.** A server saying its tools, prompts, or resources have moved
on sets `stale` on its row and nothing else. It deliberately does not
re-probe: a run's tool array is frozen, and replacing it under a session in
flight would invalidate the prompt-cache prefix every request of that run
shares. The flag is a note for the operator; the next Refresh is what acts on
it, and clears it.

**Resource updated.** Logged under the server's name.

## Since protocol 2026-07-28, servers do not call clients

Worth knowing before reading the handler code. Sampling, elicitation, and
`roots/list` used to be server-initiated JSON-RPC requests. From protocol
version 2026-07-28 they are forbidden as such (SEP-2322): a server embeds the
ask in the *result* of the call it is already serving, and the client's
multi-round-trip middleware fulfils it and re-invokes the server's handler
with the answer.

Two consequences. The middleware is on by default, so there is no retry loop
in this package — only handlers and the policy each applies. And a server
cannot ask for roots from inside a tool handler by calling `roots/list`; it
returns an `InputRequests` map instead. The tests are written that way
because it is the only way that works.

## Permissions

| Session mode | `allow_readonly` | Outcome |
| --- | --- | --- |
| `full` | either | allowed |
| `readonly` | false | denied, rule names the server |
| `readonly` | true | allowed |

Deny patterns match against the descriptor exactly as they do for built-in
tools; the descriptor for an MCP call is its full prefixed name, so
`-deny mcp__blender__` keeps a run off one server entirely.

The allowance a run resolves is frozen onto its policy for the run's length,
and the copy it started with is kept on the session row (`mcp_read_only`)
beside the permission mode and the deny patterns. A resume resolves it fresh,
so an operator who revokes a server's allowance has the next resume honour it.
That is right where an operator owns the registry and wrong where the caller
does — `harness gemini-session`'s client supplies both the tools and their
allowance in the same request — so that surface holds a resume to the stored
copy instead, and refuses a create that would move it
(docs/STDIO-PROTOCOL.md, "Resuming across process restarts").

## What the model is told

Nothing is added to the system prompt. The head stays byte-identical to what
it renders today for a session with no MCP servers configured, and the tool
array is the only part of the request that changes.

The opening user message gains a short MCP section — beside the `CLAUDE.md`
excerpts and the skills catalogue, rendered only when the session's array
carries MCP tools — naming the connected servers and saying that their tools
are named `mcp__<server>__<tool>` and reach systems outside the workspace.

Under that listing, each server's own `instructions` are quoted verbatim,
attributed to the server that sent them, in the same sorted order — and only
for a server that actually contributed tools to this session's array, since
prose about tools the session cannot call is prose it has no use for.

**Verbatim is deliberate.** This text is written by a server's authors for a
model to read, and it is where the operational knowledge lives that a tool
schema has no room for: the official Blender server's explains the datablock
model, warns that operators clobber the selection as a side effect, and says
that an unflushed bmesh silently loses every edit. None of that is derivable
from twenty-six function signatures. Summarising it here would be this
harness second-guessing the only party that knows, and dropping it — which
is what happened until the `instructions` column existed — leaves a model
holding a large tool array and no idea how the thing behind it works.

A server that sends none renders nothing, so the block is byte-identical to
what it was before any of this existed. A failure reading the instructions
costs a log line, not the run: the tools are still on the array, and the
session still starts.

## Adding a server

The `/mcp-servers` screen's add form is behind a paste box rather than a set of
fields to fill in by hand: paste a line copied straight out of a server's own
README or shell history and it turns into the fields itself
(`web/src/api/mcpCommand.ts`'s `parseMCPCommand`) — a `claude mcp add ...`
invocation, a bare command, or a bare URL. `--scope` is accepted and
silently dropped, because this harness's configuration is global rather than
per-project or per-user (above) — there is nothing here for it to mean.

Walking through the example the screen itself shows:

```
claude mcp add --scope user blender -- uvx blender-mcp
```

parses to a stdio server named `blender`, command `uvx`, args
`["blender-mcp"]`. Saving it does what any create does: `POST
/api/mcp/servers`, then an immediate probe — `uvx blender-mcp` launched as a
subprocess *inside the harness container*, given up to a minute to resolve
and install the package cold and speak MCP over stdio ("Probing" above). A
successful probe fills the card with `blender`'s tool list; a failed one
leaves `probe_error` on the row, truncated to 500 bytes, and the card shows
it with nothing to list yet under `tools_json` — there is no snapshot from
an earlier success to fall back to for a server that has never probed
successfully. The usual causes are mundane: a command that doesn't exist on
the image (a runtime the container doesn't carry), a malformed URL or header
for an `http` server, an env var the operator meant to set and didn't — and,
for a server that bridges to an application, the one the next section is
about.

## Blender, the worked example

Both stacks here run the official Blender MCP server, which is not on PyPI —
it is a subdirectory of Blender's own repository, so the row pins a commit
rather than a version:

```
uvx --from git+https://projects.blender.org/lab/blender_mcp@<sha>#subdirectory=mcp \
    --with 'mcp[cli]<2' blender-mcp
```

with `BLENDER_MCP_HOST=host.docker.internal` in `env`. Do not confuse it with
the community `uvx blender-mcp` package, which is a different server with a
different tool list (Poly Haven, Sketchfab and Hyper3D asset tools) and reads
a differently named `BLENDER_HOST`.

It is also the server the `instructions` column was built for. It sends
around five kilobytes at initialize — the datablock model, the active-object
versus selection distinction, the depsgraph update, the bmesh flush — and
until that column existed the SDK read them and this harness dropped them,
leaving a session holding twenty-six Blender tools and nothing about how
Blender behaves.

**Its twenty-six tools reach two different Blenders, and that is the thing to
understand before debugging one.** Fourteen of them — `execute_blender_code`,
the `jump_to_*` navigation, the screenshots — talk over TCP to the add-on
running inside the operator's *interactive* Blender, the one with the scene
open on screen. The other twelve, every name ending `_for_cli`, do not: they
shell out to `blender --background` from the MCP server subprocess, which
runs inside the harness container. So one half drives the operator's live
session and the other half opens a `.blend` in a throwaway process, and a
tool that fails tells you which half you were in.

The add-on half is the one likely to catch a first-time user out, because the
subprocess dials from *inside* the container. A Blender running on the
operator's own machine and nowhere else is a `localhost:9876` the container
cannot reach, so the probe fails with a connection error naming the MCP
server, not Blender — that is as far as the subprocess itself got. On Docker
Desktop `host.docker.internal` reaches the host's loopback and the add-on's
default bind is enough; anywhere else Blender has to be reachable from the
container's own network, the same "which side of the docker socket" question
that already governs what a `full`-mode session can reach (ARCHITECTURE.md,
"Gotchas").

The CLI half needs no network and a binary instead: the image carries Blender
itself, pinned in the Dockerfile to the same version the operator runs, for
exactly these twelve tools. The comment on that layer is the reference for
why it comes from Alpine's `edge` repository and why `spirv-tools` is named
alongside it. Without it the twelve fail on every call with an error naming
Python rather than the missing Blender.

**The two image screenshot tools need `size_limit_in_bytes` set.** Left at
its default of `0` — no limit — `get_screenshot_of_window_as_image` and
`get_screenshot_of_area_as_image` fail against a normal-sized Blender window
with `Invalid response from Blender at …:9876: Unterminated string`: a
full-resolution PNG, base64'd, overruns the framing of the add-on's TCP
bridge, and the MCP server is left parsing a truncated JSON string. Any
non-zero limit works — measured down from 300000 to 8000 bytes, all fine —
and the same window answers `get_screenshot_of_window_as_json` without
complaint, because that payload is small. The other twenty-four tools work
with their defaults. Nothing here can fix it from this side; it is the
vendored server's own wire format, and the workaround is to pass the
argument.

Paths cross the same divide. The add-on resolves a path on the *host*, the
CLI tools resolve one inside the *container*, and the two agree only where
the workspace mount makes them agree — source and target are the same
absolute path there by design (docs/WORKTREES.md, "Path parity"), so a
`render_viewport_to_path` written under the workspace root is a file the
session can then read back. A path anywhere else means whichever filesystem
that half happens to be standing on.
