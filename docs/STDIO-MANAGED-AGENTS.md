# The stdio protocol, Managed Agents vocabulary

`harness claude-session` is one process that runs one coding session for a
parent application. The parent spawns it, owns the working directory, and
drives it over stdin and stdout. There is no HTTP listener, no work queue and
no worker pool.

**This is a third vocabulary the same kind of session speaks.**
`harness stdio-session` and `harness gemini-session` run the identical loop
and put the OpenAI Responses API's and Google's Interactions API's own
vocabularies on the pipe; [STDIO-PROTOCOL.md](STDIO-PROTOCOL.md) and
[STDIO-INTERACTIONS.md](STDIO-INTERACTIONS.md) are those documents. This one
hosts Claude alone — `claude-opus-5-5`, `claude-sonnet-5`, `claude-fable-5-1` —
because a client speaking Anthropic's own vocabulary has no way to drive
another vendor's model through it, the same reason `gemini-session` hosts
only Google's models. The subcommand is what chooses, because the choice has
to be made before `initialize` can answer.

What crosses the pipe is **Anthropic's own Managed Agents vocabulary**: the
session and event shapes documented at
<https://platform.claude.com/docs/en/managed-agents/sessions> and
<https://platform.claude.com/docs/en/managed-agents/events-and-streaming>.
Every shape this document names is cited against those two pages (and
<https://platform.claude.com/docs/en/managed-agents/tools> for the tool
vocabulary, <https://platform.claude.com/docs/en/managed-agents/overview> and
<https://platform.claude.com/docs/en/managed-agents/session-operations> for
the resource model and statuses), as read live on 2026-09-22.

**None of Anthropic's hosted agent runtime is involved.** Managed Agents
proper runs Claude inside Anthropic's own sandbox, against an `agent`
resource (model, system prompt, tools, MCP servers, skills) and an
`environment` resource (where the sandbox runs) that a caller creates ahead
of a session. This process has neither. It runs the identical loop
`stdio-session` and `gemini-session` run, in the parent's own working
directory, with this repository's own tools — and the client the loop talks
to Anthropic through, `internal/anthropic`, calls the plain Messages API
(`POST /v1/messages`, `docs/ANTHROPIC-INTEGRATION.md`), never
`POST /v1/sessions`. Managed Agents is borrowed here purely as a **parent-facing
wire vocabulary** — the session/event shapes a client already knows if it
speaks Anthropic's real Managed Agents surface — the same way Interactions is
borrowed for `gemini-session` even though the loop's tool execution never
touches Google's own agent runtime. Every place this process cannot honour a
real Managed Agents field (there is no agent, no environment, no sandbox, no
vault, no budget) is refused by name rather than silently accepted.

This document is the contract. A client is built from it and never needs to
read Go.

## Contents

- [Starting the process](#starting-the-process)
- [Framing](#framing)
- [The handshake](#the-handshake)
- [Methods](#methods)
- [Notifications](#notifications)
- [Ordering guarantees](#ordering-guarantees)
- [Tools](#tools)
- [Client-declared (custom) tools](#client-declared-custom-tools)
- [Resuming across process restarts](#resuming-across-process-restarts)
- [Permissions](#permissions)
- [Errors](#errors)
- [Lifecycle](#lifecycle)
- [Deviations from Anthropic's HTTP surface](#deviations-from-anthropics-http-surface)
- [Deviations from Codex's app-server](#deviations-from-codexs-app-server)
- [The seam](#the-seam)
- [Not built](#not-built)

## Starting the process

```
harness claude-session [-state-dir DIR] [-keep-state] [-model NAME] [-prices PATH]
                       [-env FILE] [-rg PATH]
```

Same flags, same meanings, as `stdio-session` and `gemini-session`
([STDIO-PROTOCOL.md, "Starting the process"](STDIO-PROTOCOL.md#starting-the-process)).
One thing differs: the credential.

| Variable | Meaning |
| --- | --- |
| `ANTHROPIC_API_KEY` | The Anthropic API key. The only credential this subcommand reads. |

`-model` defaults to `claude-sonnet-5`. `initialize`'s `models` names the
three Claude models this subcommand accepts; a `-model` outside that list is
refused at startup. **When no key is set**, `initialize` still succeeds — a
parent can start the process and query its capabilities without one — and
the first `sessions.create` fails with `-32003` naming `ANTHROPIC_API_KEY`.
**When the key is present but rejected by Anthropic**, the session starts,
the request fails, and the run ends with a `session.error` notification
carrying Anthropic's own message followed by `session.status_idle` with
`stop_reason: {type: "end_turn"}` and `harness.reason: "failed"`.

`-env FILE` reads `ANTHROPIC_API_KEY` alone out of the named file, the same
narrow reading the other two subcommands give their own variables.

**stdout carries protocol frames and nothing else.** Every log line, warning
and diagnostic goes to stderr, exactly as the other two subcommands do.

## Framing

Unchanged from the other two dialects
([STDIO-INTERACTIONS.md, "Framing"](STDIO-INTERACTIONS.md#framing)):
JSON-RPC 2.0, one object per line, newline-delimited, UTF-8, `jsonrpc`
omitted on every frame this process writes. Field naming is Anthropic's own:
`snake_case` throughout, including inside the `harness` extension blocks —
the one place this document's own vocabulary rather than Anthropic's lives,
and it keeps the same casing convention rather than standing out from it.

## The handshake

Identical mechanics to the other two vocabularies
([STDIO-INTERACTIONS.md, "The handshake"](STDIO-INTERACTIONS.md#the-handshake)):
nothing runs until `initialize` has been answered *and* `initialized` has
arrived, `capabilities.function_calls` on the client's `initialize` gates
whether a `custom` tool in a create body is accepted (see
[Client-declared (custom) tools](#client-declared-custom-tools) — the name is
unchanged from the other two dialects even though what it gates is now an
asynchronous flow rather than a blocking call), and the two frames are
handled in arrival order.

### `initialize` result

```jsonc
{
  "server_info": {
    "name": "agent-harness claude-session",
    "version": "v0.48.1",
    "protocol": "anthropic.managed_agents.v1beta"
  },
  "capabilities": {
    "streaming": true,
    "events": true,
    "interrupt": true,
    "resume_session": true,
    "mcp_servers": true,
    "custom_tools": true,
    "permission_modes": ["readonly", "full"]
  },
  "models": ["claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5"],
  "default_model": "claude-sonnet-5",
  "model_details": [
    {
      "id": "claude-fable-5-1",
      "display_name": "Claude Fable 5.1",
      "context_window_tokens": 1000000,
      "effort_levels": ["low", "medium", "high", "xhigh", "max"]
    },
    {
      "id": "claude-opus-5-5",
      "display_name": "Claude Opus 5.5",
      "context_window_tokens": 1000000,
      "effort_levels": ["low", "medium", "high", "xhigh", "max"]
    },
    {
      "id": "claude-sonnet-5",
      "display_name": "Claude Sonnet 5",
      "context_window_tokens": 1000000,
      "effort_levels": ["low", "medium", "high", "xhigh", "max"]
    }
  ]
}
```

`capabilities.events` replaces `append` — one wire method now carries
steering, interrupt and custom-tool answers alike, see
[Methods](#methods). `capabilities.interrupt` says `user.interrupt` is
honoured; there is no separate cancel method to gate. `custom_tools` replaces
`function_tools`, Anthropic's own name for a client-declared tool
(`docs/managed-agents/tools`, "Custom tools ... are analogous to
user-defined client tools in the Messages API"). `model_details.effort_levels`
is this process's own key — Anthropic's own `model_details` concept does not
exist as a discoverable field on the real surface, the way `thinking_levels`/
`reasoning_efforts` already are not on Google's or the Responses API's either
— naming it `effort_levels` rather than reusing either sibling's key because
Managed Agents' own vocabulary for the concept is `effort`
(`docs/managed-agents/agent-setup`, referenced from `docs/managed-agents/sessions`'s
"override agent configuration" section), not "thinking level" or "reasoning
effort".

`mcp_servers` is false when the process has no MCP client wired, exactly as
the other two report it.

## Methods

| Method | Anthropic's counterpart | What it does |
| --- | --- | --- |
| `initialize` | — | Handshake. |
| `initialized` | — | Handshake acknowledgement. |
| `sessions.create` | `POST /v1/sessions` | Start a session, optionally seeded with one `user.message`. |
| `sessions.events` | `POST /v1/sessions/{id}/events` | Post one or more events: a message, an interrupt, or custom-tool results. |
| `sessions.get` | `GET /v1/sessions/{id}` | Read the session, or one of its turns. |
| `sessions.delete` | `DELETE /v1/sessions/{id}` | Forget the session. |
| `shutdown` | — | End the session. Equivalent to closing stdin. |

There is no `sessions.cancel`. Anthropic's own surface has none either —
interrupting a session is a `user.interrupt` *event*, sent through
`sessions.events` (`docs/managed-agents/events-and-streaming`, "Purpose: Stop
agent mid-execution") — and this process follows that shape exactly rather
than inventing a sixth method the way the other two dialects invent
`.append`. See [Unit of work](#the-unit-of-work-a-session-and-its-turns)
below for why `.append` disappears too.

### The unit of work: a session and its turns

The other two dialects address **a run** — one whole agentic turn, minted its
own id at create, chained to the next one by
`previous_interaction_id`/`previous_response_id`. Managed Agents addresses
**a session**: one id for the conversation's whole life, that goes
`running` → `idle` → `running` as a client posts more `user.message` events
into it. There is no create-a-new-turn verb after the first — posting a
message to an idle session *is* starting the next turn
(`docs/managed-agents/sessions`, "Starting the session": "user.message ...
Usage: Starts session or continues after session.status_idle"). This process
follows that shape:

- **A client holds one id for the whole conversation: the session id**,
  spelled `session.id` on every response and `session_id` on every
  notification. It is the same value the other two dialects put in
  `harness.session_id` — this dialect promotes it to the primary address
  because Managed Agents has no secondary one.
- **`sessions.events` posting `user.message` does double duty**, exactly as
  Anthropic's own does: when the session is `idle` it starts a new turn,
  chained onto the session in place, with nothing re-declared (no tools, no
  cwd, no permission mode — none of it left memory, so none of it needs
  re-checking the way a cross-process resume does); when the session is
  `running` it is a steer, landing at the next sub-turn boundary exactly as
  `.append` does today.
- **A per-turn id still exists, at the user's request, inside the `harness`
  extension block rather than as a second top-level id.** Every notification
  carries `harness.turn_id` alongside the `harness.sub_turn` it already
  carries in the other two dialects; `sessions.create`'s and
  `sessions.events`' own results carry it too. A client that wants to
  address one specific turn — get its transcript once the session has moved
  on, or interrupt it specifically — names `harness.turn_id`, on
  `sessions.get` and on a `user.interrupt` event respectively. Naming no
  `harness.turn_id` means "the current or most recent one." Minted the same
  way the other two dialects mint their own run id (`NewRunID`, this
  dialect's own spelling: `turn_<hex>`) — never resolved from the client, so
  a `turn_…` value from a different session, or from a process that no
  longer holds it, is `-32001` (turn not found) the same way an unknown
  `previous_response_id` is on the other two.
- **Resuming across a restart stays exactly `sessions.create` +
  `harness.resume_session_id`**, as proposed and agreed — see
  [Resuming across process restarts](#resuming-across-process-restarts).
  This is itself a deviation from Anthropic's real surface (below): Anthropic
  never "recreates" a session, because a real session outlives any one
  connection on Anthropic's own servers. This process's session dies with
  the process that is running it, the same problem the other two dialects
  solve the same way, so the fix is the same.

### `sessions.create`

```jsonc
{
  "agent": {
    "type": "agent_with_overrides",
    "model": {"id": "claude-sonnet-5", "effort": "high"}
  },
  "initial_events": [
    {"type": "user.message", "content": [{"type": "text", "text": "Add a test for the retry path."}]}
  ],
  "tools": [ /* see Tools */ ],
  "harness": {
    "cwd": "/Users/you/Repos/thing",
    "resume_session_id": "sess-3b71…",
    "permission_mode": "full",
    "deny": ["rm -rf", "git push"],
    "message_id": "msg-1",
    "title": "Retry path test",
    "description": "Cover the 429 branch",
    "result_schema": { /* JSON Schema */ }
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `agent` | no | See below. Only the `agent_with_overrides` form is accepted. |
| `environment_id` | — | **Refused** with `-32004`. See [Deviations](#deviations-from-anthropics-http-surface): `harness.cwd` fills this role, as it already fills Google's `environment`. |
| `initial_events` | no | At most one `user.message` (see below). Anything else — `user.define_outcome`, more than one event, a `system.message` — is refused with `-32004`/`-32602`. |
| `tools` | no | See [Tools](#tools). |
| `vault_ids`, `budget` | — | **Refused** with `-32004`. No vaults, no session budgets (plan, "Not building"). |
| `harness.cwd` | yes, unless continuing | The directory the session works in. Absolute. |
| `harness.resume_session_id` | no | Continue that session, read out of the state directory. |
| `harness.permission_mode` | no | `readonly` (default) or `full`. |
| `harness.deny` | no | Substring patterns, as the other two dialects. |
| `harness.max_sub_turns` | — | **Refused** with `-32004`, as the other two dialects — see [STDIO-PROTOCOL.md, "No sub-turn ceiling"](STDIO-PROTOCOL.md#no-sub-turn-ceiling). |
| `harness.message_id` | no | Echoed on the `user.message` event this input becomes. |
| `harness.title`, `harness.description` | no | Name the run in this harness's own records. |
| `harness.result_schema` | no | Becomes the session's result schema, validated by `Complete`. Spelled under `harness` rather than as a top-level field, because Managed Agents has no `response_format`/`text.format` concept at all — this is the harness's own extension on every dialect, just homeless on this one's own vocabulary. |

#### The `agent` field

Real Managed Agents requires `agent` (an id, a pinned version, or an
`agent_with_overrides` object) and `environment_id`, because a session always
runs against a stored agent and a stored sandbox. This process has neither
resource. `agent` is accepted in one shape only:

```jsonc
{"type": "agent_with_overrides", "model": {"id": "claude-sonnet-5", "effort": "high"}}
```

which is Anthropic's own shape for "run this one session differently from
the agent it references" (`docs/managed-agents/sessions`, "Override agent
configuration for a session") — narrowed to the one thing there is no agent
resource to inherit anything else from. Every other spelling, and every
other field on this one, is refused:

| Sent | Refused because |
| --- | --- |
| A bare agent-id string, or `{"type": "agent"}` | There is no agent resource to look up by id. |
| `agent_with_overrides.id`, `.version` | Same — nothing to override. |
| `agent_with_overrides.system` | The harness's own system prompt is frozen for a session's life, the same reason `instructions`/`system_instruction` are redirected rather than honoured on the other two dialects (see Deviations). |
| `agent_with_overrides.tools`, `.mcp_servers`, `.skills` | This process's tool array comes from the create body's own `tools` field and the harness's built-ins, not from an agent resource. |
| `model.inference_geo` | No data-residency pinning; this process calls one fixed Anthropic endpoint. |
| `model.effort` | **Accepted, as a deviation.** Real Managed Agents ties effort to the agent resource and explicitly does *not* let a `model` override carry it ("a session created with a model override runs at the model's default effort level"). Since this process has no agent resource for effort to live on at all, the model override is the only place left for it, and DeepSeek's and Gemini's dialects both already let a create pick effort explicitly — dropping that ability here would be a regression, not fidelity. |

`initial_events` accepts zero events (the session is created `idle`, no
turn started — see [Session statuses](#session-statuses)) or exactly one
`user.message`, matching real Managed Agents' own accepted set narrowed to
what this process reads as a prompt (Anthropic's four content block types
reduce to text; an `image` or `document` block is refused the same way an
`input_image` is on the other two — see [Not built](#not-built)). Passing
`initial_events` with a `user.message` starts the session running in the same
call, exactly as it does on the real surface.

**Result**, with `stream` true or absent (Anthropic's own surface has no
`stream` field on a session create at all — see Deviations; it is accepted
here only so the other two dialects' `stream: false` behaviour has a place to
live):

```jsonc
{"session": {"id": "sess-3b71…", "status": "running",
             "created_at": "2026-09-22T01:02:03Z", "updated_at": "…",
             "agent": {"model": {"id": "claude-sonnet-5", "effort": "high"}},
             "harness": {"turn_id": "turn_9f0c…"}}}
```

`status` is `idle` when `initial_events` carried nothing. `harness.turn_id`
is absent in that case — no turn has started yet. Streamed notifications
follow the same way they do on the other two dialects; `stream: false` (a
harness extension of this field, not Anthropic's) holds the answer until the
turn ends and returns the fully assembled `session`.

**One at a time.** As on the other two: one process hosts one session, so a
second `sessions.create` naming a *different* new session while one is
already open is `-32600`. A second `sessions.create` naming the same session
via `harness.resume_session_id` while it is already the one this process is
running is likewise refused — there is nothing to resume that is not already
here; post a `user.message`/`user.interrupt` through `sessions.events`
instead.

### `sessions.events`

```jsonc
{
  "session_id": "sess-3b71…",
  "events": [
    {"type": "user.message", "content": [{"type": "text", "text": "actually, do it the other way"}],
     "harness": {"message_id": "msg-2"}}
  ]
}
```

One call may carry more than one event, matching Anthropic's own
`events: [...]` array. This process accepts, per call:

- **At most one `user.message` or one `user.interrupt`** (never both — a
  message and an interrupt in the same call is `-32602`; send the interrupt
  first, wait for it to be acknowledged, then the message).
- **Any number of `user.custom_tool_result` events**, one per pending
  `agent.custom_tool_use` the client is answering. See
  [Client-declared (custom) tools](#client-declared-custom-tools).
- Nothing else. `user.tool_result`, `user.tool_confirmation` and
  `user.define_outcome` are refused with `-32004` — see [Not
  built](#not-built). `system.message` is refused with `-32004`: this
  harness's system prompt is frozen for the session's life the same way
  `instructions` is on every dialect (Deviations).

#### `user.message`

Idle session: starts the next turn, chained onto this one in place — nothing
is re-declared, because nothing left this process's memory. Running session:
a steer, landing at the next sub-turn boundary, exactly as `.append` behaves
on the other two dialects (`docs/STDIO-PROTOCOL.md`, "responses.append" —
same withdrawal-on-end semantics, same `harness.unapplied_message_ids`
accounting, same refusal — `-32002` — outside those two statuses). **A
session that is idle because it is waiting on a custom tool result
(`stop_reason.type: "requires_action"`) refuses a `user.message` with
`-32002`**, the message naming the pending `custom_tool_use_id`s — see
[Client-declared (custom) tools](#client-declared-custom-tools) for why this
is refused rather than queued.

#### `user.interrupt`

```jsonc
{"type": "user.interrupt", "harness": {"turn_id": "turn_9f0c…"}}
```

`harness.turn_id` is optional; omitted, it means "whichever turn is running
now." Named, it must match the running turn or a `-32002` names which turn
actually is running — this process only ever has one turn in flight, so
naming any other id is always a mistake a client should be told about rather
than have silently ignored. **Also valid while the session is idle awaiting
a custom tool result**: it cancels the pending call(s) the same way
cancelling a running turn cancels whatever tool is mid-flight, and the turn
ends with `stop_reason: {type: "user_interrupt"}` — this is the one escape
hatch out of a stuck `requires_action` wait, matching Anthropic's own advice
to use `user.interrupt` to unstick a session before archiving or deleting it
(`docs/managed-agents/session-operations`).

Waits for the turn to actually stop before answering, exactly as
`.cancel` does on the other two dialects.

#### `user.custom_tool_result`

See [Client-declared (custom) tools](#client-declared-custom-tools).

**Result**, one entry per posted event, in the order sent:

```jsonc
{"session_id": "sess-3b71…",
 "results": [
   {"type": "user.message", "seq": 42, "harness": {"turn_id": "turn_9f0c…", "message_id": "msg-2"}}
 ]}
```

An interrupt's result entry carries no `seq` (nothing was appended to the
log — an interrupt is not steer input); a custom-tool-result's entry carries
the `custom_tool_use_id` it resolved. The answer means every event in the
call is **committed**, not that the model has seen a steer yet or that a
pending call has actually been answered inside the loop — the same
committed-not-yet-applied distinction the other two dialects' `.append`
already documents.

### `sessions.get`

```jsonc
{"session_id": "sess-3b71…", "harness": {"turn_id": "turn_9f0c…"}}
```

`harness.turn_id` omitted: the session's current or most recently finished
turn. Named: that specific turn, if this process still holds it in memory
(it holds every turn of the session's chain for the process's life, the same
as `interactions.get`/`responses.get` already can answer for any run in
their own chain — nothing new here). A `turn_id` this process never minted,
or one from a different session, is `-32001`.

Answers with the session including its current turn's assembled events —
the document a client would have built by folding the stream — the same
recovery path `interactions.get`/`responses.get` serve.

### `sessions.delete`

Forgets the session's turns from this process's memory, exactly as
`.delete` on the other two dialects — the on-disk transcript is untouched.
Refused with `-32002` while a turn is `running`, or while one is idle
awaiting a custom tool result (Anthropic's own delete has the identical
refusal — "A running session cannot be deleted").

This is deliberately **not** Anthropic's own delete, which "permanently
removes the record, events, and associated sandbox"
(`docs/managed-agents/session-operations`). A client-issued delete erasing
`harness.resume_session_id`'s own backing store would break the one thing
every dialect's resume story depends on, and no dialect gives a client that
power today.

### `shutdown`

Unchanged from the other two dialects.

## Notifications

Every notification's method name is the Managed Agents event's own `type`,
and its params are that event's own fields, with `session_id` added on every
one — the same reason the other two dialects add their own run id, except
here it addresses the one thing a client actually holds.

| Method | Anthropic's own | Params |
| --- | --- | --- |
| `session.status_running` | yes | `{session_id, harness:{turn_id, sub_turn}}` |
| `session.status_idle` | yes | `{session_id, stop_reason, harness:{turn_id, reason}}` |
| `session.error` | yes | `{session_id, error:{type, message}, harness:{turn_id}}` |
| `session.usage` | yes | `{session_id, harness:{turn_id, sub_turn, usage}}` |
| `agent.message` | yes | `{session_id, id, content, harness:{turn_id, sub_turn}}` |
| `agent.thinking` | yes | `{session_id, id, thinking, harness:{turn_id, sub_turn}}` |
| `agent.tool_use` | yes | `{session_id, id, name, input, evaluated_permission, harness:{turn_id, sub_turn}}` |
| `agent.tool_result` | yes | `{session_id, id, tool_use_id, content, is_error, harness:{turn_id}}` |
| `agent.mcp_tool_use` | yes | Same shape as `agent.tool_use`, for an `mcp_server` tool call. |
| `agent.custom_tool_use` | yes | `{session_id, id, name, input, harness:{turn_id, sub_turn}}` — see below. |
| `span.model_request_start` | yes | `{session_id, id, model, input_tokens, harness:{turn_id, sub_turn}}` |
| `span.model_request_end` | yes | `{session_id, id, harness:{turn_id, sub_turn}}` |
| `event_start` | yes | `{session_id, event:{type, id}}` |
| `event_delta` | yes | `{session_id, event_id, delta}` |
| `harness.tool_output` | no | `{session_id, call_id, text, harness:{turn_id}}` |

`session.thread_status_idle` and everything else naming a `thread` is not
sent — see [Not built](#not-built); this process runs one thread, the
session itself, and never the multi-agent orchestration Managed Agents' own
`threads` concept exists for.

**Every notification carries `harness.turn_id`.** This is the one addition
every frame gets that the other two dialects' `harness.sub_turn` already
established the precedent for: a client that wants to group frames the way
this harness's own transcript does needs both numbers, because this
vocabulary has no id above `sub_turn` on its own frames the way
`interaction_id`/`response_id` already carried one.

### `agent.tool_use` vs `agent.mcp_tool_use` vs `agent.custom_tool_use`

Three call sites, one distinction Anthropic's own vocabulary already makes
for exactly this split:

- **This harness's own built-in tools** — `Bash`, `Read`, `Write`, `Edit`,
  `Glob`, `Grep`, the plan tools, the vision tools, `Complete` — render as
  `agent.tool_use`/`agent.tool_result`, Anthropic's own shape for a
  server-run built-in (`bash`, `file_editor`, and the rest of
  `docs/managed-agents/tools`' own toolset).
- **A client's declared `mcp_server` tools** render as
  `agent.mcp_tool_use`, Anthropic's own distinct type for an MCP-served
  call, `evaluated_permission` carried the same way `agent.tool_use`'s is.
- **A client's declared `custom` (function) tools** render as
  `agent.custom_tool_use` — see next section.

`evaluated_permission` is always `"allow"` or `"deny"` here, never `"ask"`:
this process has no interactive confirmation step
(`docs/STDIO-PROTOCOL.md`, "Permissions" — "There is no approval request in
this protocol, in either direction"), so `user.tool_confirmation` is refused
(see [Not built](#not-built)) and a call this harness's own deny patterns
would refuse is denied outright rather than asked about.

### `session.status_idle` and `stop_reason`

```jsonc
{"session_id": "sess-3b71…",
 "stop_reason": {"type": "end_turn"},
 "harness": {"turn_id": "turn_9f0c…", "reason": "complete"}}
```

`stop_reason.type` is one of Anthropic's own three (`end_turn`,
`requires_action`, `user_interrupt`), plus one this process adds:

| `stop_reason.type` | Anthropic's own | When |
| --- | --- | --- |
| `end_turn` | yes | The turn ended on its own terms — the model answered without a tool call, or called `Complete` successfully. |
| `requires_action` | yes | A custom tool call is pending; `stop_reason.event_ids` names it/them. |
| `user_interrupt` | yes | `user.interrupt` ended the turn, or the process is shutting down. |
| `complete_rejected` | **no — this process's own** | `Complete`'s structured answer failed the result schema three times running. Anthropic's own three-value enum has nothing for a harness-specific validation loop; adding a fourth is more honest than folding it into `end_turn`, which a client would read as success. |

`harness.reason` carries what `harness.reason` already carries on the other
two dialects (`complete`, `no_tool_calls`, `complete_rejected`, `cancelled`)
— `stop_reason.type` is the coarse, Anthropic-shaped signal; `harness.reason`
is the fine one, same division of labour `status`/`harness.reason` already
has on the other two dialects.

### Session statuses

| `status` | Anthropic's own | Meaning here |
| --- | --- | --- |
| `idle` | yes | No turn in flight. Includes a session that finished cleanly, one that failed, and one waiting on a custom tool result — `stop_reason` on the last `session.status_idle` says which. |
| `running` | yes | A turn is in flight. |
| `terminated` | yes, narrowed | This process sets it only while nothing is running and the last turn ended in an unrecoverable error, or after `sessions.delete`. **Unlike Anthropic's own `terminated`, it is not permanent here**: `harness.resume_session_id` can still pick the session back up and start a new turn on it, the same way the other two dialects already let a resume continue a session whose last run failed (`resume.go` refuses a resume only for `StatusCreating`/`StatusCompacted`, never for `StatusFailed`). |
| `rescheduling` | — refused a place | Not sent. A transient retry (`internal/providerhttp`'s own backoff) is invisible on this process's own wire, the same way it is invisible on the other two dialects' — nothing changes about `session.status_running` while a retry is in flight. |

## Ordering guarantees

Identical to the other two dialects
([STDIO-INTERACTIONS.md, "Ordering guarantees"](STDIO-INTERACTIONS.md#ordering-guarantees)),
keyed by `(harness.turn_id, output position)` rather than a bare index —
this vocabulary's events do not carry a numeric index the way a step or an
output item does, so a client folds `agent.message`/`agent.thinking` deltas
by `event_id` and everything else by arrival order within one `turn_id`.
`session.status_running` opens a turn and `session.status_idle` closes it,
the same bracket `interaction.created`/`.completed` and
`response.created`/`.completed` already provide, except a session's stream
does not end there — the next turn's `session.status_running` can follow
directly, with no notification marking "this session is done for good"
short of `session.status_idle` with `stop_reason: {type: "end_turn"}`
following a delete or shutdown.

## Tools

The `mcp_server` and `function`-shaped declarations from the other two
dialects both still apply, spelled in this vocabulary's own tool union:

- **`mcp_server`** (unchanged spelling — Managed Agents' own
  `mcp_toolset`/server-registration concept is agent-scoped and this
  process has no agent, so it keeps the create-body shape the other two
  dialects already use rather than inventing a second one). Same dial,
  same `mcp__<name>__<tool>` naming, same "this is the path to prefer."
- **`custom`** replaces `function` — Anthropic's own name
  (`docs/managed-agents/tools`, "Custom tools"), same fields
  (`name`, `description`, `input_schema` in place of `parameters` —
  Anthropic's own key). Requires `capabilities.function_calls` at
  `initialize`, the same gate the other two dialects already use (the
  capability name is unchanged: it says the client answers custom-tool
  results, whatever this vocabulary calls the tool that provoked one).

`web_search` and `web_fetch` are **not** declared this way. They are
Claude's own server tools, added by `internal/anthropic` after the frozen
array the same way the plan's phase 2/3 describe
(`docs/plans/archive/claude-provider.md`, "Tools"). A `tools` entry naming
`web_search`/`web_fetch` explicitly is refused with `-32602`: a client
cannot turn them off or on per session, matching how neither of the other
two dialects lets a client touch their hosted tool set either.

Everything else Anthropic's own toolset union carries — `bash`, `read`,
`write`, `edit`, `glob`, `grep` as *declarable* tool types — is refused with
`-32004`. Those are this harness's own built-ins; they are not declared,
configurable, or removable from the create body, exactly as on the other two
dialects.

## Client-declared (custom) tools

This is the one flow that differs in kind from the other two dialects, per
sign-off: Anthropic's own asynchronous, event-based shape, not a blocking
JSON-RPC request the way `harness.function_call` is.

### The flow

1. The model calls a `custom` tool. This process emits `agent.custom_tool_use`
   as an ordinary notification:

   ```jsonc
   {"session_id": "sess-3b71…", "id": "sevt_98231", "name": "show_widget",
    "input": {"title": "…"}, "harness": {"turn_id": "turn_9f0c…", "sub_turn": 4}}
   ```

2. The session goes idle:

   ```jsonc
   {"session_id": "sess-3b71…",
    "stop_reason": {"type": "requires_action", "event_ids": ["sevt_98231"]},
    "harness": {"turn_id": "turn_9f0c…"}}
   ```

   `stop_reason.event_ids` may name more than one pending call — the model
   can request several `custom` tool calls in one response, and this
   process's own tool dispatch already runs a sub-turn's calls without
   waiting for each one before starting the next
   (`internal/CLAUDE.md`, `internal/session`, `tooldispatch.go`). Every
   built-in and MCP call in that same batch still runs and resolves
   normally, in parallel with the custom ones sitting pending; the sub-turn
   as a whole does not close, and the model is not asked again, until every
   id in `stop_reason.event_ids` has an answer.

3. The client answers each, in any order, through `sessions.events`:

   ```jsonc
   {"session_id": "sess-3b71…",
    "events": [
      {"type": "user.custom_tool_result", "custom_tool_use_id": "sevt_98231",
       "content": [{"type": "text", "text": "done"}], "is_error": false}
    ]}
   ```

   `content` takes `text` and `image` blocks, `image` base64 — unchanged
   from the other two dialects' own client-answer shape. `is_error` is
   inferred from the Messages API's own `tool_result` content block, which
   `docs/managed-agents/tools` cites custom tools as directly analogous to;
   phase 4's live check should confirm it against a real
   `user.custom_tool_result` payload before relying on it.

4. Once every pending id for the turn has a result, the turn resumes:
   `session.status_running` fires again and the next model request goes
   out with every result folded in.

### What happens to a stray event while a result is pending

- **`user.message`**: refused with `-32002`, naming the pending
  `custom_tool_use_id`(s). Queuing it instead was the other option
  considered and rejected: this vocabulary has no precedent for two kinds
  of pending input at once, and a client that meant to redirect the agent
  can send `user.interrupt` first — the one unambiguous way to abandon a
  pending call — then a fresh `user.message` once idle.
  `harness.unapplied_message_ids` does not apply here, because the message
  was never accepted in the first place.
- **`user.interrupt`**: accepted — see [`user.interrupt`](#userinterrupt)
  above. This is the escape hatch.
- **A second `user.custom_tool_result` for an id already answered**, or for
  one this process never emitted, is refused with `-32002`.

### Timeout

**No protocol-level timeout is added.** A pending custom tool call is bound
by whatever timeout this harness's own tool-execution layer already applies
to a call in flight (`docs/TOOLS.md`, "per-tool timeouts") — see
[The seam](#the-seam) for why that number needs to change for this one call
site. When it elapses, the pending call resolves as an ordinary error result
("timed out waiting for a client result") the same way any other tool
timeout does, the sub-turn continues as if `user.custom_tool_result` had
carried an error, and a `user.custom_tool_result` that arrives after that
point is refused with `-32002` — the id is no longer pending.

## Resuming across process restarts

Identical in spelling and in every check to the other two dialects
([STDIO-INTERACTIONS.md, "Resuming across process restarts"](STDIO-INTERACTIONS.md#resuming-across-process-restarts)):
`sessions.create` with `harness.resume_session_id` set, the frozen prefix
(model, cwd, permission mode, deny patterns, result schema) inherited and
checked rather than taken from the create, and every declared tool checked
against what the session froze. The one difference is what such a create
returns: `session.status` reflecting whatever the session was last left at
(`idle` if nothing was running when the earlier process died, never
`running` — a session an earlier process died holding is reclaimed exactly
as `resume.go` already reclaims one), and no automatic new turn — a resume
that also wants to say something sends `harness.resume_session_id` on the
create *and* posts a `user.message` through `sessions.events` once it has
the session id back, or seeds one via `initial_events`, whichever a client
finds more natural; both work, since `initial_events` on a resuming create is
read exactly as `.events` would be. `-32005`/`-32006` are unchanged.

## Permissions

Unchanged in every particular from the other two dialects
([STDIO-INTERACTIONS.md, "Permissions"](STDIO-INTERACTIONS.md#permissions)):
`readonly`/`full`, set once, `harness.deny` subtracting, the mode gating
execution rather than the tool array, a client's own `custom` tools refused
in `readonly` unless every one of them is marked `harness.read_only`. Note
what `full` strips from `Bash`'s environment here: `ANTHROPIC_API_KEY`
alone, this subcommand's one credential.

## Errors

Same JSON-RPC error numbers as the other two dialects, same meanings, with
one addition:

| Code | Name | When |
| --- | --- | --- |
| `-32700`–`-32006` | (all of them) | Unchanged — see [STDIO-PROTOCOL.md, "Errors"](STDIO-PROTOCOL.md#errors). |
| `-32004` | unsupported | Also: `environment_id`; `vault_ids`; `budget`; any `agent` field this document does not name; `web_search`/`web_fetch` in a `tools` declaration; `user.tool_result`; `user.tool_confirmation`; `user.define_outcome`; `system.message`. |
| `-32002` | not running / not applicable | Also: a `user.message` while a custom tool result is pending; a `user.custom_tool_result` for an id not currently pending; `sessions.delete` while idle-but-pending, the same as while running. |

`-32001` is "run not found" on the other two dialects; here it is "turn not
found" — an unrecognised `harness.turn_id`, on `sessions.get` or on
`user.interrupt`.

## Lifecycle

Unchanged from the other two dialects in every row
([STDIO-INTERACTIONS.md, "Lifecycle"](STDIO-INTERACTIONS.md#lifecycle)) —
stdin closing cancels whatever is running (including a pending custom tool
call, which resolves as cancelled rather than as a timeout) and waits for the
terminal event; stdout breaking is discovered the same way; a tool running at
cancel has its context cancelled the same way.

## Deviations from Anthropic's HTTP surface

Each of these is a place a client written against Anthropic's real Managed
Agents docs would be surprised.

**1. No `agent` or `environment` resource.** Covered above, and load-bearing
enough to repeat here: this process is not driving Anthropic's own agent
runtime, and `agent`/`environment_id` are narrowed to the one thing they can
mean when there is no such resource.

**2. `sessions.create` returns immediately even when streaming**, for the
identical reason the other two dialects' create does
(`docs/STDIO-PROTOCOL.md`, deviation 1): a JSON-RPC result is one frame.
`stream: false` — this process's own extension, since Anthropic's create has
no such field — holds the answer until the turn ends.

**3. `harness.turn_id` exists at all.** Anthropic's own surface has no
per-turn id anywhere; a session's events are read back as one flat,
undifferentiated history (`GET /v1/sessions/{id}/events`, not built here —
see [Not built](#not-built)). This process keeps one internally (one
`Translator`, one assembled document per turn) and a client benefits from
addressing it directly, so it is exposed — inside `harness`, never
displacing `session.id` as the address a client is expected to hold day to
day.

**4. `sessions.create` + `harness.resume_session_id`, not a durable session
that outlives the connection.** Covered under
[Resuming](#resuming-across-process-restarts) and under [Unit of
work](#the-unit-of-work-a-session-and-its-turns).

**5. `agent.custom_tool_use`'s asynchrony is real here, not simulated.**
Unlike a hypothetical shim that answers a custom tool call the instant it is
asked (as `harness.function_call` effectively always has, being a blocking
RPC), this process actually suspends the sub-turn until
`user.custom_tool_result` arrives, which may be an arbitrary wall-clock gap
later. See [The seam](#the-seam) for what that costs `internal/session`.

**6. `sessions.delete` does not delete.** Covered under
[`sessions.delete`](#sessionsdelete).

**7. `stop_reason.type: "complete_rejected"`.** Not one of Anthropic's own
three values — see [`session.status_idle` and
stop_reason](#sessionstatus_idle-and-stop_reason).

**8. `terminated` is not permanent.** See [Session
statuses](#session-statuses).

**9. `system.message` is refused.** Anthropic's own event appends to the
system prompt for the rest of the session; this harness's system prompt is
the frozen shared cache prefix every dialect already protects the same way
(`instructions`/`system_instruction` redirected to the first user message
rather than honoured — the identical deviation the other two dialects
already record).

**10. Usage is reported per model request, not only at turn end.** See
`session.usage`'s row in [Notifications](#notifications) — the same
deviation (8) the other two dialects already record for the same reason.

**11. `event_start`/`event_delta` are not opt-in.** Anthropic's own surface
gates them behind a `?event_deltas[]=...` query parameter on the SSE
connection; a JSON-RPC pipe has no query string, and the other two dialects
already stream every delta unconditionally, so this one does too, for
`agent.message` and `agent.thinking` alike (unlike Anthropic's real surface,
where `agent.thinking`'s preview carries no text at all — see [Not
built](#not-built)).

**12. `initial_events` requires exactly one `user.message`.** Phase 4 does
not build the zero-event, "session created idle, no turn started" shape this
document originally described in [`sessions.create`](#sessionscreate) and
[Session statuses](#session-statuses) — every `sessions.create` this build
answers has already started running by the time it returns. A client wanting
a session with no turn yet posts its first `user.message` through
`sessions.events` immediately after a create that inherits it none the less
requires `harness.cwd` there and then; open an issue if a genuinely turnless
session matters to a client of this build.

**13. `agent.custom_tool_use`'s own id is the tool call's id, not a minted
`sevt_…` one.** The worked example above shows `"id": "sevt_98231"`; this
build reuses the same id `agent.tool_use`/`agent.mcp_tool_use` already carry
for a built-in or MCP call — the provider's own `tool_use` id — because it is
already unique per call and already what `agent.tool_result.tool_use_id`
names back, so minting a second one would only be a second name for the same
thing. `stop_reason.event_ids` and `user.custom_tool_result.custom_tool_use_id`
both use this same id.

**14. `agent.tool_use`/`agent.mcp_tool_use` carry no `evaluated_permission`.**
This harness's own tool_call event commits to the log — and so reaches the
translator — before permission is checked, which happens inside `Execute`
once dispatch actually starts the call; there is no honest answer to give at
notification time for a call about to be denied. The eventual
`agent.tool_result` carries `is_error` and `harness.rule` instead, the same
place a denial already shows up on the other two dialects.

**15. `terminated` is not rendered.** [Session statuses](#session-statuses)
above describes it as reachable for an unrecoverable failure; this build has
no such distinction from an ordinary one and reports every non-running
session `idle`, matching the row's own broader "includes ... one that
failed" reading. Revisit if a client needs to tell the two apart.

**16. `sessions.events`' interrupt-then-message ordering is enforced
per call, not across calls.** "A `user.message` and a `user.interrupt` in the
same call is `-32602`" is read literally: one `sessions.events` call may
carry at most one of the two, in either order relative to any
`user.custom_tool_result` events alongside it, and the code is `-32602`
(`CodeInvalidParams`) rather than a distinct one.

**17. Every run under this dialect sends a fixed `max_tokens`.** This
vocabulary has no `max_output_tokens`/`generation_config.max_output_tokens`
field on `sessions.create` at all for a client to set. The live API refuses
a streaming Messages request with `max_tokens: 0` outright — found during
this phase's own live check, the first request `harness claude-session` ever
sent for real — so this build sends a fixed 8192 on every request rather
than the zero a client's silence would otherwise resolve to.

## Deviations from Codex's app-server

The same three binaries were specified to mirror Codex's `app-server`
method and notification shapes; the other two dialects each record what
survived and what did not
([STDIO-PROTOCOL.md](STDIO-PROTOCOL.md#deviations-from-codexs-app-server),
[STDIO-INTERACTIONS.md](STDIO-INTERACTIONS.md#deviations-from-codexs-app-server)).
This dialect keeps the same **kept** list (JSON-RPC framing, the
`initialize`/`initialized` handshake, an unrecognised notification ignored,
a server request the client cannot answer getting a decline, permissions set
once). Where it differs from *both* of the others, not just from Codex, is
the **replaced** table: there is no `turn/steer` or `turn/interrupt`
counterpart at all, because both collapse into `sessions.events` carrying a
typed event rather than a distinct verb — the mapping is to Anthropic's own
event *type*, not to a JSON-RPC method name, for those two rows. Every other
row (`thread/start`↔`sessions.create`, `item/*`↔`agent.*`/`span.*`,
token-usage↔`session.usage`) carries over the same reasoning the other two
dialects already give.

## The seam

Everything named here is for phase 4 to build; nothing here is built by this
phase.

**`Dialect`.** One new implementation (`ManagedAgents`, alongside
`Responses` and `Interactions`), and one small addition to the interface
itself: a way for `Server` to know that the wire-visible address for a run is
the **session** id, not the run id, for the two places that currently always
use the run id — the append-result echo and the client-tool-call request.
Concretely, something in the shape of

```go
AddressID(runID, sessionID string) string
```

which `Responses` and `Interactions` implement as `return runID` (no change
in behaviour) and `ManagedAgents` as `return sessionID`.
`FunctionCall`/`hostTools.runID` do not need a new field — they are already
an opaque string the dialect decides what to do with; only the value
`server.go` passes into them changes.

**`Server`'s client-facing lookup.** `append`/`cancelRun`/`get`/`delete`
today resolve the id a client sent by looking it up in `s.runs`, keyed by
run id (`s.lookup`). Under `ManagedAgents` a client always sends the session
id. Since one process hosts exactly one session for its whole life, this
does not need a second index — a `lookupBySession(id string) (*run, bool)`
that checks `id` against the one session this process is running (or has
most recently run) and returns the current/latest `*run` is enough. Gated by
the same `AddressID`-style capability, or a second, boolean one
(`AddressesSession() bool`) if that reads more clearly at the call site;
either is a small, single-purpose addition.

**`Server.events` is new, distinct from today's `append`.** The Claude
dialect's one wire method (`sessions.events`) covers what `.append` does
today (steer a running turn) *and* what create's `PreviousRunID` branch does
today (start a new turn on an idle, already-loaded session) *and* interrupt
*and* custom-tool-result resolution — four things `Server.handle`'s
method-name switch currently keeps apart by having four different method
names. This dialect needs one handler that decodes the posted event batch,
looks at each event's own `type`, and dispatches internally: `user.message`
on an idle session reuses (or factors out into a shared helper) the same
same-process-continuation logic `create()`'s `PreviousRunID != ""` branch
already has; `user.message` on a running session reuses today's `append()`
body near-verbatim; `user.interrupt` reuses today's `cancelRun()` body;
`user.custom_tool_result` is genuinely new (next paragraph). None of this
needs a `Dialect` interface change — `DecodeCreate`/`DecodeAppend` already
return neutral structs `Server` interprets; a `DecodeEvents` that returns a
small tagged union (message / interrupt / one-or-more custom tool results)
is an addition in the same spirit, not a different shape of addition.

**`RunView`/`Translator` need nothing new.** `RunView` already carries both
`ID` and `SessionID`; the new translator renders `SessionID` where the other
two render `ID`, and mints its own internal `turn_<hex>` via `NewRunID()`
exactly as the other two mint their own run ids — it is simply never
serialised as a top-level `id`, only nested under `harness.turn_id`. No
interface change.

**A pending-custom-tool-call registry, and the timeout it needs, are the
one piece of this that reaches outside `internal/stdiosession`.** Today,
`hostTools.Call` for a client function blocks synchronously on
`s.conn.Call(ctx, MethodFunctionCall, params, &res)` — a JSON-RPC request
answered by a JSON-RPC response on the same connection, within the same
tool-dispatch goroutine's own wait. The async shape this phase's sign-off
asks for still blocks that same goroutine (nothing about
`internal/session`'s tool dispatch itself needs to change — dispatch is
already opaque to whatever `tools.MCPProvider.Call` does, synchronous or
not, and a sub-turn already does not close until every call in it has a
result) — but what it blocks *on* changes: not a response to a request this
process sent, but an event (`user.custom_tool_result`) arriving on an
entirely different call, `sessions.events`, keyed by `custom_tool_use_id`
rather than by JSON-RPC request id. Concretely, `Server` needs a small
map-plus-channel (or condition variable) keyed by the custom-tool-use id,
written by whatever emits `agent.custom_tool_use`, and signalled by the new
`sessions.events`/`user.custom_tool_result` path; `hostTools.Call` under
this dialect blocks on that channel instead of on `conn.Call`. **The one
change outside this package**: whatever timeout `internal/tools` already
wraps every tool call in (`docs/TOOLS.md`, "per-tool timeouts") is wrong for
this one call site — a human- or system-gated custom tool result has no
natural ceiling the harness can pick on its behalf, and the existing
per-tool timeout was sized for a tool that runs to completion on its own,
not one waiting on an external event that might take minutes. Phase 4 needs
to either exempt a `custom`-tool call from that timeout under this dialect,
or give it a much larger one, and should read the actual mechanism in
`internal/tools` before deciding which — this document does not know its
exact shape and neither figure should be asserted here.

### What phase 4 actually built

Everything above held. Four things this document did not, or could not,
foresee exactly:

- **`Dialect` grew two members, not one `AddressID`, and `AddressID` turned
  out unused.** `AddressID(runID, sessionID string) string` is as proposed,
  but the "append-result echo" it was proposed for does not need it after
  all: `AppendResult(runID string, seq int64) any` already decides per
  dialect where `runID` goes — nested under `harness.turn_id` for
  ManagedAgents, the top-level id for the other two — so the call site
  passes the plain turn id straight through on every dialect, and an earlier
  build of this phase that ran it through `AddressID` first put the
  *session* id under `harness.turn_id` by mistake (caught by this phase's own
  live check, not by a test). `AddressID` is kept on the interface, unused,
  as a documented available seam rather than removed mid-phase. What
  `Server.get`/`.delete` actually needed is `AddressesSession() bool`
  (`false` on Responses and Interactions, `true` here), which they use to
  fall back to a session-keyed lookup when a bare id does not resolve as a
  run id first — the smaller, boolean alternative this document's own seam
  section named as an option.
- **`NewTranslator` gained a `sessionID` parameter.** `RunView` already
  carrying both ids covers every *terminal* frame (`Resource`/`Result`/
  `Completed`/`Failed`), but `Live` and `Event` — which fire throughout a
  turn, long before any `RunView` exists — need `session_id` on every one of
  ManagedAgents' own frames too, and had no way to reach it. Responses and
  Interactions both ignore the new parameter; neither ever needed it.
- **The timeout fix lives partly outside `internal/tools`.**
  `tools.Timeouts` gained one field, `HostTool`, consulted only for the
  reserved client-tool namespace (renamed `tools.ClientToolServerName`,
  formerly a literal `internal/stdiosession` alone knew) and only when set —
  zero keeps every existing caller, `stdio-session`'s and `gemini-session`'s
  own blocking `harness.function_call` included, on the unchanged MCP
  timeout. Setting it for one process alone needed a new
  `session.Runner.ToolTimeouts` field, threaded into the `tools.Executor`
  `Run`/`Resume` already build, because `tools.NewExecutor` takes no
  timeouts today. `harness claude-session` sets it to a large, finite figure
  (24 hours) — not truly unbounded, since a process a stuck call can wedge
  forever is a worse failure mode than a very long one, and `user.interrupt`
  is the documented way to escape it sooner.
- **A registration race, and the pragmatic fix.** The `KindToolCall` event
  behind `agent.custom_tool_use` commits, and reaches the client, strictly
  before the tool-dispatch goroutine that will register the pending wait
  even starts (`internal/session/turn.go` commits a sub-turn's whole
  tool-call batch before `executeToolCalls` runs it) — on a local pipe a
  fast client can answer before this side has anywhere to put the answer.
  `Server`'s registry stashes an early answer for `registerCustomTool` to
  claim, which is indistinguishable from an id this process never declared
  at all; a `user.custom_tool_result` naming an unmet id is therefore
  accepted rather than refused with `-32002` in that narrow case, a
  deliberate simplification over a full three-state registry.

## Not built

Raised rather than fixed, in this repo's own convention:

- **A session created idle, with `initial_events` empty and no turn started.**
  See Deviation 12, above. `sessions.create` in this build always starts the
  session's first turn.
- **`GET /v1/sessions/{id}/events`** (the flat, paginated event-history
  endpoint) is not mirrored. `sessions.get` already answers "what has this
  turn done so far" from this process's own in-memory assembly, the same
  recovery path the other two dialects' `.get` already serve; a client
  wanting the *whole* session's history across every turn asks
  `sessions.get` once per `harness.turn_id` it knows about, or keeps its
  own notification log the way the other two dialects already expect.
- **`GET /v1/sessions`** (listing) is not built. One process hosts one
  session; there is nothing to list.
- **Archiving** (`POST /v1/sessions/{id}/archive`) is not built.
  `sessions.delete`'s narrowed meaning (forget the run(s) from memory,
  leave the transcript alone) already covers what a client would reach for
  archive to do.
- **Updating a session's tools/MCP servers mid-session**
  (`docs/managed-agents/session-operations`, "Updating the agent
  configuration") is not built. The frozen-tool-array invariant every
  dialect already enforces (`resume.go`) applies here too: a session's tool
  array is fixed for its life, and changing it means a new session.
- **`user.tool_result`, `user.tool_confirmation`, `user.define_outcome`,
  `system.message`.** Refused outright (see Errors). The first two exist
  for a self-hosted-sandbox worker and an interactive-confirmation flow
  this process has neither of (no approval step exists in any of the three
  dialects — `docs/STDIO-PROTOCOL.md`, "Permissions"); the third is outcome
  grading, explicitly out of the plan ("Not building"); the fourth is
  covered under Deviation 9.
- **Image and document input** on `user.message`/`initial_events`. Refused
  the same way an `input_image` is refused on the other two dialects — this
  process reads images off its own filesystem with its own tools.
- **`agent.thinking` preview text.** Real Managed Agents documents this as
  always empty on preview (`event_start` only, no `event_delta`); this
  process's own reasoning summary streams the same live text the other two
  dialects already stream, so its `event_delta` for `agent.thinking` *does*
  carry text — a deliberate improvement over the real surface's own
  behaviour, recorded here so a client built against Anthropic's docs first
  is not surprised to receive more than it expected.
- **Multi-agent orchestration, threads, rosters, dreaming, MCP tunnels.**
  Entirely out of scope (plan, "Not building") — this process runs the one
  loop it always has, alone.
