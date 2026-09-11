# The stdio protocol, Interactions vocabulary

`harness gemini-session` is one process that runs one coding session for a
parent application. The parent spawns it, owns the working directory, and
drives it over stdin and stdout. There is no HTTP listener, no work queue and
no worker pool.

**This is one of two vocabularies the same session speaks.**
`harness stdio-session` runs the identical session and puts the OpenAI
Responses API's vocabulary on the pipe instead;
[STDIO-PROTOCOL.md](STDIO-PROTOCOL.md) is that document, and its porting
table maps every shape here onto its counterpart there. The subcommand is
what chooses, because the choice has to be made before `initialize` can
answer. Pick the one your client already implements.

The two differ in what the parent reads, not in what the session can do —
with one exception, which is why this document exists: a **thought
signature** has a place here and none there. It is the receipt Google issues
for a thinking step, and a client that stores transcripts meaning to replay
them somewhere needs it. See [Delta types](#delta-types).

What crosses the pipe is **Google's own Interactions vocabulary**. The methods
are the four REST methods on `POST /v1beta/interactions`, and the
notifications are the server-sent events that surface streams — the same
`event_type` names, the same `step` and `delta` objects, the same
`Interaction` resource. A client that already reads Google's event stream
reads this one. <https://ai.google.dev/api/interactions> is the field
reference for every shape named here; this document says what a shape means
when the loop runs on your machine instead of Google's, and records every
place the two differ.

This document is the contract. A client is built from it and never needs to
read Go.

Turret's own cross-repository design (`docs/design/gemini-agent-harness.md` in
`desktop-coding-client`) pins the revision this document and the wire agree on
as of the additions below: `c039c0b4d7bea9f657e09b80d38af833f00c3182`
(`v0.48.0-10-gc039c0b`). `model_details` and stdio `mcp_server` support landed
on top of that revision; a client built against this document handles both.

A client pinned at that revision works against `harness gemini-session`
unchanged. For a period the name was an alias that spoke the Responses
vocabulary instead; it does not any more.

## Contents

- [Starting the process](#starting-the-process)
- [Framing](#framing)
- [The handshake](#the-handshake)
- [Methods](#methods)
- [Notifications](#notifications)
- [Ordering guarantees](#ordering-guarantees)
- [Tools](#tools)
- [Resuming across process restarts](#resuming-across-process-restarts)
- [Permissions](#permissions)
- [Errors](#errors)
- [Lifecycle](#lifecycle)
- [Deviations from Google's HTTP surface](#deviations-from-googles-http-surface)
- [Deviations from Codex's app-server](#deviations-from-codexs-app-server)
- [Not built](#not-built)

## Starting the process

```
harness gemini-session [-state-dir DIR] [-keep-state] [-model NAME] [-prices PATH]
                       [-env FILE] [-rg PATH]
```

`harness stdio-session` takes the same flags and speaks the other vocabulary.
Nothing else distinguishes them.

The parent supplies the API key in the environment it spawns the process
with:

| Variable | Meaning |
| --- | --- |
| `GEMINI_API_KEY` | The Google API key. Read first. |
| `GOOGLE_API_KEY` | The same thing under the name Google's own SDKs read. Used when `GEMINI_API_KEY` is unset. |

There is no settings store here and no screen to type a key into, so a hosted
session's credentials are the host's to supply. The key reaches this
process's own Google API client directly and is never written to the state
directory's own settings table, and it is stripped from the environment
`Bash` and a stdio `mcp_server` child inherit — both would otherwise get the
whole of this process's own environment, key included, as "the parent's
environment" the next paragraph describes for `Bash`. **When neither is
set**,
`initialize` still succeeds — a parent can start the process and query its
capabilities without a key — and the first `interactions.create` fails with
`-32003` and a message naming both variables. Nothing is attempted against
Google, so a misconfigured host gets one clear error before any work happens
rather than a stream that dies on its first request. **When the key is
present but rejected by Google**, the run starts, the request fails, and the
interaction ends with an `error` notification carrying Google's own message
followed by `interaction.completed` with `status: "failed"`.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-state-dir` | a per-process directory under the user cache dir | Where this session's SQLite state and transcript mirror live. A directory the parent names is kept; the default one is removed when the process exits. It is also what `harness.resume_session_id` reads: a parent that wants a session to survive this process names one. |
| `-keep-state` | off | Keep the default state directory after exit, for reading a finished session's transcript. |
| `-model` | `gemini-3.7-flash` | What a create body with no `model` runs on. `initialize`'s `models` names every model this process accepts; see below. |
| `-prices` | `configs/prices.json` | The price table behind the cost figure on `harness.usage`. A missing table costs the cost figure and nothing else. |
| `-env` | unset | A `KEY=VALUE` file to take the API key from when the environment carries none. **Only `GEMINI_API_KEY` and `GOOGLE_API_KEY` are read out of it** — see below. A file that cannot be read is fatal. |
| `-rg` | `$AGENT_HARNESS_RG`, then `rg` on the `PATH` | The ripgrep binary the session's `Grep` calls run. A parent that ships one names it here. A path that is not there is fatal at startup, and nothing on the `PATH` is used when the flag is set. With no binary named and none on the `PATH`, `Grep` falls back to its own Go walk (docs/TOOLS.md, "Grep and Glob"). |

**stdout carries protocol frames and nothing else.** Every log line, warning
and diagnostic goes to stderr. No `.env` is read implicitly: the parent owns
the working directory, which for a hosted session is a repository the session
is about to work in, and a `.env` sitting in it must not contribute
environment to this process — every command the session's Bash tool runs
inherits that environment.

`-env FILE` is the one way a file reaches this process, and it does not
weaken that rule. The path is explicit, so no directory contributes anything
by merely being the working directory; and **only the two key variables are
taken from the file**, never the rest of it, so nothing in it becomes ambient
for the session's own subprocesses. The environment still wins where both
carry a key. The flag is for a person driving the process by hand without
exporting a key first — a parent application should keep supplying the
environment.

## Framing

JSON-RPC 2.0, one object per line, newline-delimited, UTF-8.

The `jsonrpc` member is **omitted** on every frame this process writes, and
ignored on every frame it reads. A client that sends it is fine.

```jsonc
// request  (client → server, and server → client for harness.function_call)
{"id": "c1", "method": "interactions.create", "params": { }}
// response
{"id": "c1", "result": { }}
{"id": "c1", "error": {"code": -32602, "message": "…"}}
// notification (no id, never answered)
{"method": "step.delta", "params": { }}
```

Request ids may be any JSON value; they are echoed verbatim. Ids this process
mints for its own requests are strings beginning `h`.

A line that is not valid JSON is answered with a `-32700` frame carrying no
id and then skipped. One malformed line does not end a session.

**Field naming is Google's:** `snake_case` throughout, including in the
extension blocks.

## The handshake

Nothing runs until `initialize` has been answered *and* the `initialized`
notification has arrived. Anything else before that is refused with `-32000`.

These two frames are handled in arrival order even when a client pipelines
them without waiting, so sending `initialize`, `initialized` and a first
`interactions.create` back to back is correct.

### `initialize` (request)

```jsonc
{
  "client_info": {"name": "Turret", "version": "1.4.0"},
  "capabilities": {
    "function_calls": true   // this client answers harness.function_call
  }
}
```

`capabilities.function_calls` is load-bearing. A client that does not claim it
cannot declare `function` tools: the create is refused rather than accepted
with tools nothing would ever call.

Result:

```jsonc
{
  "server_info": {
    "name": "agent-harness gemini-session",
    "version": "v0.48.1",
    "protocol": "google.interactions.v1beta"
  },
  "capabilities": {
    "streaming": true,
    "append": true,
    "cancel": true,
    "previous_interaction": true,
    "resume_session": true,
    "mcp_servers": true,
    "function_tools": true,
    "permission_modes": ["readonly", "full"]
  },
  "models": ["gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite"],
  "default_model": "gemini-3.7-flash",
  "model_details": [
    {
      "id": "gemini-3.8-flash",
      "display_name": "Gemini 3.8 Flash",
      "context_window_tokens": 1048576,
      "thinking_levels": ["low", "medium", "high"]
    },
    {
      "id": "gemini-3.7-flash",
      "display_name": "Gemini 3.7 Flash",
      "context_window_tokens": 1048576,
      "thinking_levels": ["low", "medium", "high"]
    },
    {
      "id": "gemini-3.6-flash",
      "display_name": "Gemini 3.6 Flash",
      "context_window_tokens": 1048576,
      "thinking_levels": ["minimal", "low", "medium", "high"]
    },
    {
      "id": "gemini-3.5-flash",
      "display_name": "Gemini 3.5 Flash",
      "context_window_tokens": 1048576,
      "thinking_levels": ["minimal", "low", "medium", "high"]
    },
    {
      "id": "gemini-3.5-flash-lite",
      "display_name": "Gemini 3.5 Flash Lite",
      "context_window_tokens": 1048576,
      "thinking_levels": ["minimal", "low", "medium", "high"]
    }
  ]
}
```

`mcp_servers` is false when the process was started without an MCP client; an
`mcp_server` tool is then refused rather than ignored.

`model_details` is one array, ordered however `models` is, one entry per
model this process accepts — never a parallel map a client has to reconcile
against `models` by name. `server_info.protocol` and `server_info.version`
remain the only two compatibility fields; `model_details` carries capability
data, not a version.

- `id` matches an entry in `models`.
- `display_name` is a human-readable name, when this process has one to
  offer; a client falls back to `id` when it is absent.
- `context_window_tokens` is the model's total input token budget. A client
  computing a context percentage divides the latest sub-turn's input tokens
  by this figure, never the cumulative interaction total.
- `thinking_levels` says what `generation_config.thinking_level` may be **for
  this model**, because the answer differs between models:
  `gemini-3.7-flash` rejects `minimal` and its siblings accept it. Empty when
  this process has no table for the model, in which case any level reaches
  the API for it to judge. A create naming a level its model refuses is
  answered `-32602` before the run starts, rather than reaching Google and
  failing the interaction mid-stream.

### `initialized` (notification)

Empty params. Sent once, after `initialize` returns.

## Methods

| Method | Google's counterpart | What it does |
| --- | --- | --- |
| `initialize` | — | Handshake. A pipe has no HTTP to negotiate over. |
| `initialized` | — | Handshake acknowledgement. |
| `interactions.create` | `POST /v1beta/interactions` | Start a run. |
| `interactions.append` | — | Add input to a run already in flight. Steering. |
| `interactions.cancel` | `POST /v1beta/interactions/{id}/cancel` | Interrupt a run. |
| `interactions.get` | `GET /v1beta/interactions/{id}` | Read an interaction and its assembled steps. |
| `interactions.delete` | `DELETE /v1beta/interactions/{id}` | Forget an interaction. |
| `shutdown` | — | End the session. Equivalent to closing stdin. |

### `interactions.create`

Google's create-interaction body, narrowed, plus a `harness` block.

```jsonc
{
  "model": "gemini-3.7-flash",
  "input": "Add a test for the retry path.",
  "system_instruction": "You are working inside Turret.",
  "previous_interaction_id": "int_9f0c…",
  "tools": [ /* see Tools */ ],
  "response_format": {"type": "text", "mime_type": "application/json",
                      "schema": { /* JSON Schema */ }},
  "generation_config": {"thinking_level": "high", "max_output_tokens": 0},
  "stream": true,
  "store": true,
  "harness": {
    "cwd": "/Users/you/Repos/thing",
    "resume_session_id": "sess-3b71…",
    "permission_mode": "full",
    "deny": ["rm -rf", "git push"],
    "max_sub_turns": 200,
    "message_id": "msg-1",
    "title": "Retry path test",
    "description": "Cover the 429 branch"
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `model` | no | One of `initialize`'s `models`. Defaults to `default_model`. |
| `agent` | — | **Refused** with `-32004`. Google's managed agents run on Google's machines. |
| `input` | yes | A string, one `Content`, an array of `Content`, or an array of `user_input` `Step`s. Text only — see [Not built](#not-built). |
| `system_instruction` | no | **Prepended to the first user input**, not sent as a system instruction. See [Deviations](#deviations-from-googles-http-surface). |
| `tools` | no | See [Tools](#tools). |
| `previous_interaction_id` | no | Continue that interaction's session, in this process. See below. |
| `response_format` | no | `schema` becomes the run's result schema, which the agent's `Complete` tool validates its answer against. |
| `generation_config.thinking_level` | no | One of the levels `initialize` gave for this model — `low`, `medium`, `high` for `gemini-3.7-flash`, which refuses `minimal`. Defaults to `high`. A level the model does not take is `-32602`. |
| `generation_config.max_output_tokens` | no | Per-request output cap. Zero leaves the API's own default. |
| `stream` | no | Default true. See below. |
| `store` | no | Accepted and ignored: this process always stores, because the loop's state machine *is* its event log. |
| `harness.cwd` | yes, unless continuing | The directory the session works in. Absolute. |
| `harness.resume_session_id` | no | Continue that session, read out of the state directory. See [Resuming across process restarts](#resuming-across-process-restarts). |
| `harness.permission_mode` | no | `readonly` (default) or `full`. See [Permissions](#permissions). |
| `harness.deny` | no | Substring patterns matched against a call's descriptor. Only ever subtracts from what the mode allows. |
| `harness.max_sub_turns` | no | Ceiling on how many model round-trips the run may take. Zero is the harness default. |
| `harness.message_id` | no | Echoed on the `user_input` step this input becomes. |
| `harness.title`, `harness.description` | no | Name the run in this harness's own records. |

**Result.** With `stream` true or absent, the answer is the interaction as at
`interaction.created` and the steps follow as notifications:

```jsonc
{"interaction": {"id": "int_…", "object": "interaction",
                 "model": "gemini-3.7-flash", "status": "in_progress",
                 "created": "2026-09-07T01:02:03Z", "updated": "…",
                 "harness": {"session_id": "sess-…"}}}
```

With `stream: false` the answer is held until the run ends and carries the
whole finished interaction including `steps`. The notifications are sent
either way, so a client that sets `stream: false` and ignores them gets
exactly Google's non-streaming behaviour.

**Threads are `previous_interaction_id` chains.** There is no `thread/start`.
A create with no `previous_interaction_id` starts a new session in
`harness.cwd`; a create naming one continues *that interaction's session* with
the new input, and the run replays the whole conversation so far. Every
interaction in a chain reports the same `harness.session_id`, and each has its
own `id`. A continued create must not change `model` — the session's prompt
prefix is frozen for its life — and must not send `harness.cwd`, which it
inherits.

**`previous_interaction_id` is bounded by this process.** The id is minted in
memory and resolved from memory, so it means nothing to a process that did not
mint it: a client that sends one across a restart gets `-32001`. Crossing a
restart is `harness.resume_session_id`, below. Sending both on one create is
`-32602`.

**One at a time.** One process hosts one session, so a create while another
interaction is in progress is refused with `-32600`. Cancel it or wait.

### `interactions.append`

Adds input to an interaction already in flight. This is steering.

```jsonc
{"interaction_id": "int_…",
 "input": "actually, do it the other way",
 "harness": {"message_id": "msg-2"}}
```

Result:

```jsonc
{"interaction_id": "int_…", "seq": 42}
```

The answer means the input is **committed**, not that the model has seen it. It
reaches the model at the next sub-turn boundary — after the current tool round
finishes, never in the middle of one — and appears then as a `user_input` step
with `harness.source: "append"` and the `message_id` echoed. That boundary is
what keeps a steer from arriving halfway through a tool call.

An append to an interaction that is not `in_progress` is refused with
`-32002`; start a new interaction with `previous_interaction_id` set to it
instead.

### `interactions.cancel`

```jsonc
{"interaction_id": "int_…"}
```

Cancels the run and **waits for it to stop** before answering with the
interaction, whose `status` is then `cancelled` and whose `harness.reason` is
`"cancelled"`. The stream ends with an `interaction.completed` carrying the
same.

Cancelling an interaction that has already finished is **not an error**: the
caller asked for a state it is already in, and a client racing a completion
should not have to handle both outcomes. It answers with the interaction as it
stands.

A tool still running when the cancel arrives has its context cancelled, which
kills the whole process group of a `Bash` call. A child that has detached from
its process group and holds the output pipe open is bounded separately, by the
tool layer's own wait delay — the cancel returns in seconds either way.

### `interactions.get`

```jsonc
{"interaction_id": "int_…"}
```

Answers with the interaction including `steps`: the assembled document, which
is exactly what a client would have built by folding the stream. Use it to
recover after a client-side drop, or to read a finished run without having
kept the notifications.

### `interactions.delete`

Forgets the interaction. Answers `{}`, as Google's does. It does not erase the
run: the transcript under the state directory is untouched. An interaction
still running is refused with `-32002`.

### `shutdown`

Answers `{}` immediately, then cancels whatever is running and lets it record
its terminal event. Closing stdin does the same thing.

## Notifications

Every notification's method name **is** the `event:` name of the corresponding
Google SSE frame, and its params **are** that frame's `data` payload, with one
addition: an `interaction_id` on every frame, because a pipe carries frames
for more than one interaction over its life where an HTTP response carries one.

| Method | Google | Params |
| --- | --- | --- |
| `interaction.created` | yes | `{interaction, event_type}` |
| `interaction.status_update` | yes | `{interaction_id, status, event_type, harness:{sub_turn}}` |
| `step.start` | yes | `{interaction_id, index, step, event_type}` |
| `step.delta` | yes | `{interaction_id, index, delta, event_type}` |
| `step.stop` | yes | `{interaction_id, index, event_type}` |
| `interaction.completed` | yes | `{interaction, event_type}` |
| `error` | yes | `{interaction_id, error:{code,message}, event_type}` |
| `harness.tool_output` | no | `{interaction_id, call_id, text, event_type}` |
| `harness.usage` | no | `{interaction_id, sub_turn, usage, event_type}` |

Google's `done` frame has no counterpart: `interaction.completed` already ends
the interaction and the pipe stays open for the next one.

**An unrecognised notification must be ignored, in both directions.** This
process ignores any notification it does not know, and a client must do the
same: notifications will be added.

### Step types

`step.start`'s `step` object carries a `type` that says which of its fields
are meaningful. These are the types this surface produces:

| `type` | Fields on `step.start` | Deltas it emits |
| --- | --- | --- |
| `user_input` | `content` (complete), `harness.source`, `harness.message_id` | none |
| `thought` | — | `thought_summary`, `thought_signature` |
| `model_output` | — | `text` |
| `function_call` | `id`, `name`, `arguments: {}` | `arguments_delta` |
| `function_result` | `call_id`, `name`, `result`, `is_error`, `harness.*` | none |

`harness.sub_turn` is on every step: which model round-trip of the run
produced it. An interaction here is a whole agentic run, so a client that
wants to group steps the way this harness's own transcript does needs the
number; on Google's surface one interaction is one model call and there is no
such thing.

A `user_input` step carries its whole `content` on `step.start` and emits no
deltas, because the text was never streamed — it was complete when the loop
recorded it. `harness.source` is:

- `input` — the interaction's own `input`. Its `content` is the whole opening
  message the model was given, which includes the task the client sent plus
  the working directory listing, the skills catalogue and any `CLAUDE.md` the
  harness found. A parent rendering its user's own words should render what it
  sent, not this.
- `append` — added by `interactions.append`.
- `reminder` — generated by the loop itself, restating a rule further down a
  long conversation. Nothing the client sent.

A `function_result` step's `harness` block carries `rule` when the call was
refused by permission policy, `truncated` when the output was cut to the
output cap, and `child_interaction_id` when the call was a `Task` sub-agent
that ran as its own session.

### Delta types

| `delta.type` | Shape | On |
| --- | --- | --- |
| `text` | `{"type":"text","text":"…"}` | `model_output` |
| `thought_summary` | `{"type":"thought_summary","content":{"type":"text","text":"…"}}` | `thought` |
| `thought_signature` | `{"type":"thought_signature","signature":"…"}` | `thought` |
| `arguments_delta` | `{"type":"arguments_delta","arguments":"{\"…\""}` | `function_call` |

`text` and `thought_summary` deltas are fragments and must be concatenated.
`arguments_delta` fragments must be concatenated into one JSON string and
parsed at `step.stop`.

`thought_signature` is **not** a fragment. It is one complete opaque value, the
receipt Google issues for a thinking step, and it arrives once, as the last
delta on its `thought` step. It has no counterpart in any other engine's
protocol and it exists here because it exists in Gemini: a client that stores
transcripts and expects to replay them somewhere should store it with the
step.

### `harness.usage`

One request's token accounting, as it happens:

```jsonc
{"interaction_id": "int_…", "sub_turn": 7,
 "usage": {"total_tokens": 12043, "total_input_tokens": 11800,
           "total_cached_tokens": 9216, "total_output_tokens": 190,
           "total_thought_tokens": 53,
           "harness": {"cost_usd": 0.0031}},
 "event_type": "harness.usage"}
```

Google reports usage once, on `interaction.completed`, which for a run
spanning a hundred sub-turns is an hour late for anything showing spend as it
accrues. The `interaction.completed` total still arrives and is still
authoritative; these are its parts. `usage.harness.cost_usd` is this harness's
own price table applied to those tokens, and is absent when no price table was
loaded.

### `harness.tool_output`

Incremental stdout from a tool that is still running, addressed by the
`call_id` of the `function_call` step that started it. Google has nothing for
it, because on Google's surface the client runs the tools and already has the
output.

## Ordering guarantees

1. Notifications for one interaction arrive in the order this process
   produced them. Every frame goes through one writer.
2. Every `step.delta` and `step.stop` names an index some `step.start` opened.
   No step is started or stopped twice.
3. `interaction.created` is the first notification of an interaction and
   `interaction.completed` is the last. Nothing for that interaction follows
   it.
4. `interaction.completed` arrives after every step of the run, including
   every `step.stop`.
5. **Steps can overlap.** A `thought` step and a `model_output` step are open
   at the same time during a sub-turn. Key steps by `index`; do not assume the
   highest index is the only open one, and do not assume a `step.start` closes
   the step before it. The reason is in
   [Deviations](#deviations-from-googles-http-surface).
6. An `interactions.append` that has been answered is committed. Its
   `user_input` step appears at the next sub-turn boundary, not immediately.

## Tools

The agent always has this harness's own tools — reading and writing files,
`Bash`, search, the plan tools, `Complete`. They are the session's whole
reason to be running on the parent's filesystem and they are not declared,
configurable or removable from the create body.

On top of those, the create body's `tools` array declares what the parent
wants the session to reach. Two of Google's tool types are honoured, and they
answer the same question two different ways:

### `mcp_server` — the parent stands a server up

```jsonc
{"type": "mcp_server",
 "name": "orchestrator",
 "url": "http://127.0.0.1:53411/s/sess-abc/orchestrator",
 "headers": {"Authorization": "Bearer …"},
 "harness": {"read_only": true}}
```

This process dials the server over HTTP itself, exactly as an
operator-configured MCP server is dialled, and its tools join the session's
array as `mcp__<name>__<tool>`. **This is the path to prefer.** The tools
arrive with their own schemas, their results come back as MCP content
including images, and nothing new crosses the pipe while the session runs — a
tool call is a request from this process to the parent's loopback server, not
a round trip through the protocol that also carries the token stream.

`name` must match `^[a-z0-9][a-z0-9_-]{0,31}$` and must not contain `__`,
which separates the server from the tool in the `mcp__<server>__<tool>` name
each of its tools is offered under; a name carrying one could not be read back
out, and the read-only gate is decided from it. `host` is reserved. A server
that fails to probe contributes no tools and does not fail the create.

An `mcp_server` declaration may dial over stdio instead, for a server the
parent has no loopback endpoint to stand up:

```jsonc
{"type": "mcp_server",
 "name": "filesystem",
 "command": "npx",
 "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Repos/thing"],
 "env": {"NODE_ENV": "production"},
 "harness": {"read_only": true}}
```

This process spawns `command` itself, exactly as an operator-configured
stdio MCP server is spawned, and its tools join the session's array the same
way an HTTP server's do. `url`/`headers` and `command`/`args`/`env` are
mutually exclusive on one declaration: naming fields from both pairs, or
neither, is `-32602`. `env` is a connection secret exactly as an HTTP
server's `headers` are — kept in memory for the length of one dial and never
written to this process's own database, so a `-state-dir` a parent keeps for
resuming holds neither. A resuming create re-supplies it, the same way it
re-supplies a bearer header, and the frozen-toolset check (see
[Resuming across process restarts](#resuming-across-process-restarts))
applies to a stdio server exactly as it does to an HTTP one: name, qualified
tool names and schemas, and `read_only` must reproduce; `command`, `args` and
`env` are connection metadata a resume re-supplies "good now."

### `function` — the parent answers over the pipe

```jsonc
{"type": "function",
 "name": "show_widget",
 "description": "Draw a diagram in the conversation",
 "parameters": {"type": "object", "properties": { }},
 "harness": {"read_only": true}}
```

For a parent whose tools are in-process and which does not want to stand an
HTTP server up. The tool joins the session's array as `mcp__host__show_widget`
— the loop reaches everything that is not a built-in through one namespaced
seam, so a client function needs a namespace even though there is no server.
The **client sees its own unqualified name** on the callback.

Requires `capabilities.function_calls` at `initialize`.

When the model calls one, this process sends a request:

```jsonc
// server → client
{"id": "h3", "method": "harness.function_call",
 "params": {"interaction_id": "int_…", "type": "function_call",
            "id": "call_98231", "name": "show_widget",
            "arguments": {"title": "…"}}}
```

`params` is a Google `FunctionCallStep`. `id` is the same `id` the client
already saw on that call's `function_call` step, so the answer renders under
the right call.

The client answers with a Google `FunctionResultStep`:

```jsonc
// client → server
{"id": "h3", "result": {"result": [{"type": "text", "text": "done"}],
                        "is_error": false}}
```

`result` accepts `text` and `image` content blocks; an image block's `data` is
base64.

**A client that cannot run the call must answer with a JSON-RPC error.**
Never leave the request pending. A decline becomes an ordinary error tool
result the model can react to and the run carries on; silence holds the
sub-turn open until the tool's own timeout expires.

### Everything else

`google_search`, `code_execution`, `computer_use`, `file_search`,
`google_maps`, `retrieval` and `url_context` are refused with `-32004`. They
are tools Google runs on Google's machines as part of serving the interaction,
and this process is not serving the interaction — it is running the loop
itself and calling `/v1beta/interactions` one model turn at a time.

## Resuming across process restarts

A session outlives the process that ran it. The row, its event log and its
frozen tool array are in the SQLite file under `-state-dir`, so a parent that
respawns `harness gemini-session` on the same directory can pick the
conversation up:

```jsonc
{"input": "carry on where you left off",
 "tools": [ /* the same tools, with connection metadata that is good now */ ],
 "harness": {"resume_session_id": "sess-3b71…"}}
```

The answer is a new interaction with a new `id` and the same
`harness.session_id`. The model is sent the whole conversation the earlier
process recorded, and the run continues with its sub-turn count, its plan and
its history intact — the same continuation `previous_interaction_id` performs,
reached by a different key.

Two things make this work.

**Name the state directory.** The default one is per-process and is removed at
exit, so a resume needs `-state-dir DIR`, the same `DIR` both times. A session
id the directory holds nothing for is `-32005`.

**Do not move the prefix.** The system prompt and the tool array are what
every request of a session shares, and the prompt cache is built on them, so a
resumed session sends the array it froze rather than one resolved fresh. The
create therefore inherits `model`, `harness.cwd`, `harness.permission_mode`,
`harness.deny` and `response_format` from the session, and naming any of them
differently is `-32602` rather than a silent override. Sending them unchanged
is fine.

### The tools have to come back

The tool array is the part a restart genuinely breaks. A `function` tool was
declared over a pipe that has closed; an `mcp_server` tool was dialled at a
URL the old parent listened on and no longer does. So the resuming create
re-declares them, and this process checks what it resolves against what the
session froze:

- Every `mcp_server` the session was started with must appear in `tools`,
  with the URL and headers that are live **now** — including one whose probe
  failed, which a create tolerates and which therefore left no tools in the
  frozen array to be recognised by. A server the create adds that the session
  did not have is refused too. Both checks run before anything is written or
  dialled, so a refused create leaves nothing behind for the next one to trip
  over.
- Every tool in the frozen array must resolve again under the same qualified
  name and with the same parameter schema. Schemas are compared as documents,
  so whitespace and key order are free.
- Nothing new may appear. The run sends the frozen array, so a tool added on
  the way back in would never be offered to the model.
- `harness.read_only` must say what it said the first time, on every tool.
  Marking a tool read-only on the way back in would be granting the session a
  call it never had; unmarking one would take away a call it has been making.

Any of those failing is `-32006`, with the offending tool or server named.
Changing a session's tools or its permissions means starting a new session.

**Credentials in `headers` and stdio `env` are never written to disk.** They
stay in this process's memory and are put back on the connection for the
length of each dial, so a `-state-dir` a parent keeps in order to resume
holds no bearer token and no stdio env value. They do have to be re-supplied
on every resume, which is the same thing the URL or command requires and for
the same reason.

**A session an earlier process died holding is reclaimable.** The state
directory belongs to one process at a time, so a row still marked running is a
leftover rather than somebody else's claim: it is closed as `cancelled` and
then resumed. A crashed parent costs nothing but the sub-turn in flight.

## Permissions

Set once, on the create that starts a chain, and **never asked about again**.
There is no approval request in this protocol, in either direction. A loop
that blocks on a human stalls when nobody is watching, which is why this
harness does not have one.

| `harness.permission_mode` | What runs |
| --- | --- |
| `readonly` | Reading, searching, the plan tools, and the tools that only write to the session's own scratch directory. `Bash`, file writes and edits are refused. |
| `full` | Everything. |

`harness.deny` subtracts from whichever mode is set and can never widen it.
Each entry is matched as a substring of the call's descriptor — the command
for `Bash`, `"<Tool> <arg>"` otherwise.

**The mode gates execution, not the tool array.** The model is offered every
tool in every mode and a refused call comes back as a `function_result` with
`is_error: true` and `harness.rule` naming the rule. The model sees why and
can do something else.

**A tool the client declared is refused in `readonly` unless the client marks
it `harness.read_only`.** This process has no idea what a client tool does —
it reaches outside the working directory by definition — so the same rule an
MCP server gets applies to it.

The gate is per namespace rather than per tool, and every `function` tool
shares one namespace, so `readonly` allows them only when **every** one of
them is marked `harness.read_only`. One unmarked tool among them makes the
whole set unreachable in `readonly` rather than carrying itself in on the
others' marking. A parent that wants its read-only tools usable in a
`readonly` session declares only those; the rest would be refused there
anyway.

Note what `full` means here: the session runs `Bash` as this process's own
user, in the parent's own working directory, with the parent's environment —
**minus `GEMINI_API_KEY` and `GOOGLE_API_KEY`**, the one exception, stripped
in both modes for the reason [Starting the process](#starting-the-process)
gives. There is no sandbox otherwise. Deciding whether a given session gets
`full` is the parent's, and the mode is the whole of what this protocol
gives it to decide with.

## Errors

Every method-level failure is a JSON-RPC error object. `data`, when present,
carries Google's own error shape (`{"code": "…", "message": "…"}`), so a
client has the vendor's string code as well as this protocol's numeric one.

| Code | Name | When |
| --- | --- | --- |
| `-32700` | parse error | A line that is not valid JSON. Answered with no id; the line is skipped. |
| `-32600` | invalid request | `initialize` twice; a create while another interaction is running; a create during shutdown. |
| `-32601` | method not found | An unknown method sent as a request. An unknown *notification* is ignored instead. |
| `-32602` | invalid params | Malformed params, an unknown model, a missing `harness.cwd`, an unknown permission mode, a function tool from a client with no `function_calls` capability, a continued create that changes model. |
| `-32603` | internal error | A failure inside this process. |
| `-32000` | not initialized | Any method before the handshake completes. |
| `-32001` | interaction not found | An interaction id this process never minted, or has deleted. |
| `-32002` | interaction not running | An append or delete against an interaction that is not `in_progress`. |
| `-32003` | credentials missing | `interactions.create` with no API key in the environment. |
| `-32004` | unsupported | `agent`; a server-side tool type; an `mcp_server` tool with no MCP client; image input. |
| `-32005` | session not found | A `harness.resume_session_id` this process's state directory holds no session for. |
| `-32006` | toolset mismatch | A resuming create whose tools do not reproduce the array the session froze. |

Failures *during* a run are not method errors — the create has already been
answered. They arrive as an `error` notification followed by
`interaction.completed` with `status: "failed"` and the message repeated in
`interaction.errors`.

### Interaction statuses

| `status` | Meaning |
| --- | --- |
| `in_progress` | Running. |
| `completed` | The run ended on its own terms. `harness.reason` says which way. |
| `incomplete` | The run hit `harness.max_sub_turns`. Continuing it with `previous_interaction_id` is the sensible next move. |
| `cancelled` | `interactions.cancel`, `shutdown`, or stdin closing. |
| `failed` | The loop could not finish: a model error, a transport failure, a store failure. |

`harness.reason` on a completed interaction is `complete` (the agent called
`Complete`), `no_tool_calls` (it answered and asked for nothing else),
`max_sub_turns`, `complete_rejected` (its structured result failed the schema),
or `cancelled`. Google's status enum does not separate an agent that finished
from one that ran out of room, and the difference decides whether a parent
offers to continue.

## Lifecycle

| Event | What happens |
| --- | --- |
| **stdin closes** | The read loop ends, a run in flight is cancelled, and the process waits for it to record its terminal event before exiting. The interaction's final status is `cancelled`. This is the normal way a parent ends a session. |
| **stdout breaks** | Writes fail silently — there is nowhere to report a failure to write. The read side discovers the same break and ends the session. |
| **the parent exits** | Both pipes break; as above. The process does not outlive its parent. |
| **a tool is running at cancel** | Its context is cancelled, which signals the whole process group of a `Bash` child. A grandchild holding the output pipe is bounded by the tool layer's own wait delay rather than waiting forever. |
| **the model errors mid-turn** | The stream ends, the loop records the failure, and the client gets `error` then `interaction.completed` with `status: "failed"`. A reasoning-starved response — the budget spent before any answer text — is retried once at double the budget before that, and both attempts are billed and both appear on `harness.usage`. |
| **the parent stops reading stdout** | Frames queue in this process rather than being dropped. There is no ceiling: a parent that has stopped reading has stopped hosting the session, and stdin closing is what ends it. |

A run's transcript survives under the state directory. With the default state
directory it is removed on exit unless `-keep-state` is set; a directory the
parent named with `-state-dir` is always kept.

## Deviations from Google's HTTP surface

Each of these is a place a client written against Google's docs would be
surprised.

**1. `interactions.create` returns immediately even when streaming.** Google
streams the events inside the HTTP response body. A JSON-RPC result is one
frame, so it cannot carry a stream: the create is answered with the interaction
as at `interaction.created` and the events follow as notifications. `stream:
false` restores Google's shape by holding the answer until the run ends.

**2. An interaction is a whole agentic run, not one model turn.** On Google's
surface a `function_call` step ends the interaction with status
`requires_action`, the client runs the tool, and a second `interactions.create`
with `previous_interaction_id` and a `function_result` continues it. Here the
loop does all of that internally: one interaction covers as many model turns
as the task takes, and the `function_call` and `function_result` steps of every
one of them stream out as they happen. That is the whole point of the binary —
a client that wanted to drive the tool loop itself would call Google directly.
`harness.sub_turn` on each step is how a client recovers the round-trip
boundaries.

**3. Steps overlap.** Google's stream never has two steps open at once. Here a
`thought` step stays open while the `model_output` step that follows it
streams. The reason is where the thought signature comes from: the harness
learns it at the *end* of a sub-turn, after the answer text has already
streamed, so closing the thought step before the answer starts would leave the
signature nowhere to go. Key steps by index.

**4. `system_instruction` is prepended to the first user input.** This harness
renders its own system prompt, and that prompt plus the tool array is the
frozen shared prefix every request of a session sends — it is what the prompt
cache is built on, and moving a byte of it costs a full-price re-read of the
whole conversation. A client's instruction therefore lands on the first user
message instead, which is where a host's mode fragment or session preamble
belongs anyway. It is not silently dropped and it is not sent as a system
instruction.

**5. `previous_interaction_id` continues a *session*, not a stored
interaction.** Google stores interactions server-side and chains them. Here
the chain is one local session resumed in place: the model sees the whole
conversation replayed, and the run continues from where it left off with its
sub-turn count, its plan and its history intact. Every interaction in a chain
reports the same `harness.session_id`. Google's ids outlive any one process
because Google holds them; these do not, which is why there is a second field
for reattaching to a session — see
[Resuming across process restarts](#resuming-across-process-restarts).

**6. `store` is ignored.** Google lets a caller opt out of storage. This
process cannot: the loop's state machine is its event log, and a run that kept
nothing could not be resumed, folded into a request, or told what it had
already done. Whether that log survives the process is `-state-dir` and
`-keep-state`, not this field.

**7. `harness.cwd` rather than `environment`.** Google's `EnvironmentConfig`
describes mounting sources into a sandbox Google runs. This process works in a
directory that already exists on the parent's machine. Overloading
`environment` for it would leave nowhere for a real `environment` to go if one
ever became meaningful.

**8. Usage is reported per request as well as at the end.** See
[`harness.usage`](#harnessusage).

**9. `interactions.cancel` is not restricted to background interactions.**
Google's cancel only applies to interactions started with `background: true`.
Here it is the interrupt, and it applies to whatever is running.

**10. There is an `interactions.append`.** Google has no verb for adding input
to an interaction in flight, because on its surface there is no window in
which to add anything.

## Deviations from Codex's app-server

This binary was specified to mirror the method and notification shapes of
OpenAI's `codex app-server`, so that a client with a translator for Codex
reuses it. That was reversed deliberately: the payload vocabulary is Google's
throughout. What survives from Codex, and what does not:

**Kept**, because a pipe needs them and Codex's answers are good ones:

- JSON-RPC 2.0 over stdio, newline-delimited, `jsonrpc` omitted on the wire.
- `initialize` then an `initialized` notification, gating everything.
- An unrecognised notification is ignored rather than answered.
- A server-initiated request the client does not implement gets a decline,
  never silence.
- A permission posture set once at start and never asked about again.

**Replaced**, method for method:

| Codex | Here | Why |
| --- | --- | --- |
| `thread/start`, `thread/resume` | `interactions.create`, with and without `previous_interaction_id` | Google already has a multi-turn model. A thread concept beside it would be a second name for the same chain. |
| `turn/start` | `interactions.create` | A turn and an interaction are the same object under two names. |
| `turn/steer` | `interactions.append` | Same semantics — input into a turn in flight, applied at a sub-turn boundary. |
| `turn/interrupt` | `interactions.cancel` | Google has a cancel. |
| `item/started`, `item/updated`, `item/completed` | `step.start`, `step.delta`, `step.stop` | Identical lifecycle, Google's names. An item and a step are the same idea. |
| `turn/started`, `turn/completed` | `interaction.status_update`, `interaction.completed` | Google's frames, with `harness.sub_turn` carrying what Codex's per-turn notifications carried. |
| token-usage notification | `harness.usage` | Google's usage object rather than Codex's. |
| dynamic tools registered at `thread/start` | Google's `tools` array with `function` and `mcp_server` members | Google's own tool union already has both shapes. |

**Not adopted:** the sandbox-policy vocabulary. Codex sets a sandbox posture
at thread start; this process has no sandbox to configure, and saying it did
would be a lie a client might rely on. `harness.permission_mode` says exactly
what it does — which tools execute — and nothing about isolation.

## Not built

Raised rather than fixed, in this repo's own convention:

- **Image input.** `input` accepts text only; an `image` content block is
  refused with `-32004`. Making it work means materialising the bytes into the
  working directory so the loop can name the file in its opening message, and
  writing into a directory the client owns is a decision this protocol has not
  taken. Write the file yourself and name its path in the text.
- **More than one interaction at a time.** One process hosts one session. A
  parent that wants two sessions spawns two processes, which is also how it
  gets two working directories.
- **`interaction.status_update` for anything but sub-turn boundaries.** Google
  emits it on interaction-level transitions; here it only ever reports
  `in_progress` at the start of a sub-turn.
- **`harness.tool_output` for tools other than `Bash`.** Nothing else in the
  harness produces incremental output today.
