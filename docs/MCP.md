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
	tools_json     TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	probed_at      TEXT NOT NULL DEFAULT '',
	probe_error    TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1
);
```

`tools_json` is only ever written by a successful probe, and a failed probe
writes `probe_error` and leaves the snapshot alone. That asymmetry is the
whole point of the column: the array a session builds must not depend on
whether a subprocess happened to start this minute.

Secrets: `env` values and `headers` values can hold API keys. `GET
/api/mcp/servers` masks them the way the settings surface masks a secret key
(`internal/redact.Secret`) — keys in the clear, values masked. A write that
sends an empty string as a value keeps the stored value for that key; removing
the key removes the value. The full values never leave the process.

## Probing

A probe connects, initialises, calls `tools/list`, and writes the result:
`tools_json`, `probed_at`, and `probe_error` cleared. It runs when a server is
created, when its connection details change, and when the operator presses
Refresh. Nothing probes on a schedule — an operator who changes nothing gets a
tool array that never changes underneath them.

`transport: "stdio"` launches `command` with `args` and `env` and speaks over
the child's stdin/stdout (`mcp.CommandTransport`). `transport: "http"` speaks
streamable HTTP to `url` with `headers` (`mcp.StreamableClientTransport`).

A dial that starts cold is given up to 60 seconds rather than the shorter
bound a bare process start or TCP connect would need: the servers that
motivated this are launched as `uvx <package>` or `npx <package>`, and both
routinely resolve and install a package before ever speaking MCP on a first
run. `Refresh` always dials fresh — bypassing whatever is cached, since the
point of an explicit probe is to prove the *current* configuration actually
connects, not that some earlier connection is still alive.

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

## Permissions

| Session mode | `allow_readonly` | Outcome |
| --- | --- | --- |
| `full` | either | allowed |
| `readonly` | false | denied, rule names the server |
| `readonly` | true | allowed |

Deny patterns match against the descriptor exactly as they do for built-in
tools; the descriptor for an MCP call is its full prefixed name, so
`-deny mcp__blender__` keeps a run off one server entirely.

## What the model is told

Nothing is added to the system prompt. The head stays byte-identical to what
it renders today for a session with no MCP servers configured, and the tool
array is the only part of the request that changes.

The opening user message gains a short MCP section — beside the `CLAUDE.md`
excerpts and the skills catalogue, rendered only when the session's array
carries MCP tools — naming the connected servers and saying that their tools
are named `mcp__<server>__<tool>` and reach systems outside the workspace.

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
for `blender-mcp` specifically, the one below.

**The one thing likely to catch a first-time user out.** `blender-mcp` is a
bridge, not the whole story: it talks over TCP to a Blender instance running
its own MCP add-on on port 9876, and the probe dials from *inside* the
harness container. A Blender running on the operator's own machine and
nowhere else is a `localhost:9876` the container cannot reach, so the probe
fails with a connection error naming `blender-mcp`, not Blender — that is as
far as the subprocess itself got. Blender has to be reachable from the
container's own network for the probe to succeed, the same "which side of
the docker socket" question that already governs what a `full`-mode session
can reach (ARCHITECTURE.md, "Gotchas").
