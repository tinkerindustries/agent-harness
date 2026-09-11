# The stdio protocol: hosting a coding session in another application

`harness stdio-session` is one process that runs one coding session for a
parent application. The parent spawns it, owns the working directory, and
drives it over stdin and stdout. There is no HTTP listener, no work queue and
no worker pool.

What crosses the pipe is **the OpenAI Responses API's own vocabulary**. The
methods are its REST methods on `POST /responses`, and the notifications are
that surface's semantic server-sent events — the same `type` names, the same
output items and deltas, the same `response` resource. A client that already
reads a Responses event stream reads this one.
<https://developers.openai.com/api/reference/resources/responses> and
`third_party/deepseek-docs/api/create-response.md` are the field reference for
every shape named here; this document says what a shape means when the loop
runs on your machine instead of a provider's, and records every place the two
differ.

**The same vocabulary runs the length of the process.** `internal/deepseek`
posts to DeepSeek's own `POST /responses`
([`DEEPSEEK-RESPONSES.md`](DEEPSEEK-RESPONSES.md)), and the loop's own
conversation vocabulary is that surface's too (`wire.Item`), so the request
body's `input` is what the fold produced, serialised as it stands — no
per-item rebuild between them:

```
create.input items ──► flattened to a plain string ──► the agent loop
                                                            │
                                    the loop's own event log (SQLite)
                                     │                          │
                     internal/fold → []wire.Item                │
                                     │  (serialised as they are)│
                              POST /responses            response.* frames
```

The two ends are still rendered separately from that log rather than one
being forwarded to the other — the loop runs the tools, so the frames a
parent reads describe work the provider never saw. What is gone is the second
representation in the middle: the Chat Completions providers now render *from*
items (`wire.MessagesFromItems`) rather than items being rendered from them.

Three things follow, and a client should know all three:

- **A create's `input` is read for its text and nothing else.** Only
  `message` items and bare content parts are accepted; a `function_call` or
  `function_call_output` in `input` is refused. A client cannot replay a
  conversation into this process, because the conversation it would be
  replaying is already here — see
  [Resuming](#resuming-across-process-restarts).
- **What the model is actually sent is more than what the parent sent.** The
  first user message the provider sees is the harness's opening message: the
  task, the workspace listing, the skills catalogue, the repositories' own
  instructions. `harness.source` on the user message item says which is
  which.
- **A response's `output` is not the provider's `output`.** Both are lists of
  the same item types, but the parent's carries the whole agentic run — every
  sub-turn's calls, their results, and the user messages the loop folded in —
  where each provider request carries one turn's worth. `harness.sub_turn`
  is how a client recovers the boundaries.

The Gemini models this process also hosts are rendered from the same log by
the same translator, with `internal/gemini` speaking Interactions to Google
underneath.

That is a statement about the wire rather than about the model: a client
drives every hosted model with the same frames. `initialize`'s `models` and
`model_details` are how it learns which is which, and it does not otherwise
have to care. See ["Which models this process
hosts"](#which-models-this-process-hosts).

This document is the contract. A client is built from it and never needs to
read Go.

**This is one of two vocabularies the same session speaks.**
`harness gemini-session` runs the identical session and puts Google's
Interactions vocabulary on the pipe instead, in the spelling
`internal/gemini` already uses against Google;
[STDIO-INTERACTIONS.md](STDIO-INTERACTIONS.md) is that document. The
subcommand is what chooses, because the choice has to be made before
`initialize` can answer: its result carries a protocol string, a capability
named for its own continuation id, and each model's effort set under its own
key.

Pick the one your client already implements. This one hosts a DeepSeek model
as well as Google's; the other hosts Google's alone, since a client speaking
Google's vocabulary has no way to drive a model of another vendor's through
it. One thing this vocabulary cannot carry is a **thought signature**, which
Google issues for a thinking step and a client storing transcripts for replay
needs; see [STDIO-INTERACTIONS.md](STDIO-INTERACTIONS.md).

Turret's own cross-repository design (`docs/design/gemini-agent-harness.md` in
`desktop-coding-client`) pins the Interactions revision at
`c039c0b4d7bea9f657e09b80d38af833f00c3182` (`v0.48.0-10-gc039c0b`), and a
client at that revision drives `harness gemini-session` unchanged.

The two vocabularies, shape for shape:

| Interactions (`gemini-session`) | Responses (`stdio-session`) |
| --- | --- |
| `interactions.create` / `.append` / `.cancel` / `.get` / `.delete` | `responses.create` / `.append` / `.cancel` / `.get` / `.delete` |
| `interaction_id` | `response_id` |
| `interaction.created` / `.completed` | `response.created` / `response.completed` |
| `interaction.status_update` | `response.in_progress` |
| `step.start` / `step.stop` | `response.output_item.added` / `.done` |
| `step.delta` | `response.output_text.delta`, `response.reasoning_text.delta`, `response.function_call_arguments.delta` |
| `error` | `response.failed` |
| step `user_input` / `thought` / `model_output` / `function_call` / `function_result` | items `message` (role `user`) / `reasoning` / `message` (role `assistant`) / `function_call` / `function_call_output` |
| `system_instruction` | `instructions` |
| `generation_config.thinking_level` | `reasoning.effort` |
| `generation_config.max_output_tokens` | `max_output_tokens` |
| `response_format.schema` | `text.format.schema` |
| `previous_interaction_id` | `previous_response_id` |
| `thinking_levels` on `model_details` | `reasoning_efforts` |
| `index` on a step frame | `output_index` on an item frame |
| `event_type` on every frame | `type`, and a `sequence_number` |
| `thought_signature` delta | *(no counterpart)* |

The `harness.*` extensions are unchanged in meaning throughout, and
`harness.resume_session_id` still carries a conversation across a restart.

## Contents

- [Starting the process](#starting-the-process)
- [Which models this process hosts](#which-models-this-process-hosts)
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
- [Deviations from the HTTP surface](#deviations-from-the-http-surface)
- [Deviations from Codex's app-server](#deviations-from-codexs-app-server)
- [Not built](#not-built)

## Starting the process

```
harness stdio-session [-state-dir DIR] [-keep-state] [-model NAME] [-prices PATH]
                      [-env FILE]
```

**`harness gemini-session` is not an alias for this.** It takes the same
flags and runs the same session, and it speaks Google's Interactions
vocabulary rather than this one
([STDIO-INTERACTIONS.md](STDIO-INTERACTIONS.md)). The handshake reports back
the name it was spawned as, so `server_info.name` says which you got and
`server_info.protocol` says which vocabulary that name speaks.

The parent supplies the API keys in the environment it spawns the process
with:

| Variable | Meaning |
| --- | --- |
| `GEMINI_API_KEY` | The Google API key, for the Gemini models. Read first. |
| `GOOGLE_API_KEY` | The same thing under the name the surface's own SDKs read. Used when `GEMINI_API_KEY` is unset. |
| `DEEPSEEK_API_KEY` | The DeepSeek API key, for `deepseek-v4-flash-vision-exp`. |

There is no settings store here and no screen to type a key into, so a hosted
session's credentials are the host's to supply. A key reaches this process's
own API client for that provider directly and is never written to the state
directory's own settings table, and all three variables are stripped from the
environment `Bash` and a stdio `mcp_server` child inherit — both would
otherwise get the whole of this process's own environment, keys included, as
"the parent's environment" the next paragraph describes for `Bash`.

**Neither key is required, and neither implies the other.** A host that
supplies one runs that provider's models; the models it did not supply a key
for are still advertised on `initialize`, and a create naming one fails with
`-32003` and a message naming **that provider's** variable. **When no key is
set at all**, `initialize` still succeeds — a parent can start the process
and query its capabilities without one — and the first `responses.create`
fails the same way. Nothing is attempted against either provider, so a
misconfigured host gets one clear error before any work happens rather than a
stream that dies on its first request. **When a key is present but rejected
by the provider**, the run starts, the request fails, and the response
ends with an `error` notification carrying the provider's own message
followed by `response.completed` with `status: "failed"`.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-state-dir` | a per-process directory under the user cache dir | Where this session's SQLite state and transcript mirror live. A directory the parent names is kept; the default one is removed when the process exits. It is also what `harness.resume_session_id` reads: a parent that wants a session to survive this process names one. |
| `-keep-state` | off | Keep the default state directory after exit, for reading a finished session's transcript. |
| `-model` | see below | What a create body with no `model` runs on. `initialize`'s `models` names every model this process accepts, and a `-model` outside that list is refused at startup rather than at the first create. Unset, it is `gemini-3.7-flash` — or `deepseek-v4-flash-vision-exp` when `DEEPSEEK_API_KEY` was supplied and neither Google variable was, because a host that gave one key meant the model that key runs. |
| `-prices` | `configs/prices.json` | The price table behind the cost figure on `harness.usage`. A missing table costs the cost figure and nothing else. |
| `-env` | unset | A `KEY=VALUE` file to take the API keys from when the environment carries none. **Only `GEMINI_API_KEY`, `GOOGLE_API_KEY` and `DEEPSEEK_API_KEY` are read out of it** — see below. A file that cannot be read is fatal. |

**stdout carries protocol frames and nothing else.** Every log line, warning
and diagnostic goes to stderr. No `.env` is read implicitly: the parent owns
the working directory, which for a hosted session is a repository the session
is about to work in, and a `.env` sitting in it must not contribute
environment to this process — every command the session's Bash tool runs
inherits that environment.

`-env FILE` is the one way a file reaches this process, and it does not
weaken that rule. The path is explicit, so no directory contributes anything
by merely being the working directory; and **only the three key variables are
taken from the file**, never the rest of it, so nothing in it becomes ambient
for the session's own subprocesses. The environment still wins where both
carry a key. The flag is for a person driving the process by hand without
exporting a key first — a parent application should keep supplying the
environment.

## Which models this process hosts

`initialize`'s `models` is the whole list, and `responses.create` refuses
anything outside it. Under `stdio-session` that is every Gemini model the
harness routes, plus exactly one DeepSeek model:

| Model | Provider | Key |
| --- | --- | --- |
| `gemini-3.8-flash`, `gemini-3.7-flash`, `gemini-3.6-flash`, `gemini-3.5-flash`, `gemini-3.5-flash-lite` | Google | `GEMINI_API_KEY` / `GOOGLE_API_KEY` |
| `deepseek-v4-flash-vision-exp` | DeepSeek | `DEEPSEEK_API_KEY` |

`harness gemini-session` advertises the Google rows and not the DeepSeek one.
A client speaking Google's vocabulary would be naming a DeepSeek model in a
`generation_config` and reading its answers as Google steps, so that command
does not offer it; a `-model` naming it there is refused at startup.

**One DeepSeek model, and it is the vision one.** The harness routes two
others, `deepseek-v4-flash` and `deepseek-v4-pro`, and this process refuses
both. Neither reads images, and a session on a model that cannot see is given
the six tools that exist to compensate — `Screenshot`, `Glance`, `Ground`,
`Detect`, `Transcribe`, `Crop` — four of which send their images to Google.
Hosting one would therefore mean a DeepSeek session that quietly needs a
Google API key as well, or that carries six tools failing whenever the model
reaches for them. `deepseek-v4-flash-vision-exp` reads images itself, so it
is offered none of them and needs one credential
(`docs/DEEPSEEK-VISION.md`). A create naming one of the other two is answered
`-32602` with the hosted list in the message.

Everything else on the wire is the same for either provider. The frames are
the surface's whatever the model is, the tool vocabulary is identical, resuming
works the same way — and a resumed session cannot change model, so a DeepSeek
session stays one for its whole life. Two things do differ, and both are
answered per model on the handshake rather than by the client knowing whose
model it is: `context_window_tokens`, and `reasoning_efforts` (below, and
["Deviations from the HTTP
surface"](#deviations-from-the-http-surface)).

## Framing

JSON-RPC 2.0, one object per line, newline-delimited, UTF-8.

The `jsonrpc` member is **omitted** on every frame this process writes, and
ignored on every frame it reads. A client that sends it is fine.

```jsonc
// request  (client → server, and server → client for harness.function_call)
{"id": "c1", "method": "responses.create", "params": { }}
// response
{"id": "c1", "result": { }}
{"id": "c1", "error": {"code": -32602, "message": "…"}}
// notification (no id, never answered)
{"method": "a delta frame", "params": { }}
```

Request ids may be any JSON value; they are echoed verbatim. Ids this process
mints for its own requests are strings beginning `h`.

A line that is not valid JSON is answered with a `-32700` frame carrying no
id and then skipped. One malformed line does not end a session.

**Field naming is the surface's:** `snake_case` throughout, including in the
extension blocks.

## The handshake

Nothing runs until `initialize` has been answered *and* the `initialized`
notification has arrived. Anything else before that is refused with `-32000`.

These two frames are handled in arrival order even when a client pipelines
them without waiting, so sending `initialize`, `initialized` and a first
`responses.create` back to back is correct.

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
    "name": "agent-harness stdio-session",
    "version": "v0.48.1",
    "protocol": "openai.responses.v1"
  },
  "capabilities": {
    "streaming": true,
    "append": true,
    "cancel": true,
    "previous_response": true,
    "resume_session": true,
    "mcp_servers": true,
    "function_tools": true,
    "permission_modes": ["readonly", "full"]
  },
  "models": ["gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite", "deepseek-v4-flash-vision-exp"],
  "default_model": "gemini-3.7-flash",
  "model_details": [
    {
      "id": "gemini-3.8-flash",
      "display_name": "Gemini 3.8 Flash",
      "context_window_tokens": 1048576,
      "reasoning_efforts": ["low", "medium", "high"]
    },
    {
      "id": "gemini-3.7-flash",
      "display_name": "Gemini 3.7 Flash",
      "context_window_tokens": 1048576,
      "reasoning_efforts": ["low", "medium", "high"]
    },
    {
      "id": "gemini-3.6-flash",
      "display_name": "Gemini 3.6 Flash",
      "context_window_tokens": 1048576,
      "reasoning_efforts": ["minimal", "low", "medium", "high"]
    },
    {
      "id": "gemini-3.5-flash",
      "display_name": "Gemini 3.5 Flash",
      "context_window_tokens": 1048576,
      "reasoning_efforts": ["minimal", "low", "medium", "high"]
    },
    {
      "id": "gemini-3.5-flash-lite",
      "display_name": "Gemini 3.5 Flash Lite",
      "context_window_tokens": 1048576,
      "reasoning_efforts": ["minimal", "low", "medium", "high"]
    },
    {
      "id": "deepseek-v4-flash-vision-exp",
      "display_name": "DeepSeek V4 Flash Vision (experimental)",
      "context_window_tokens": 1000000,
      "reasoning_efforts": ["low", "high", "max"]
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
  by this figure, never the cumulative response total.
- `reasoning_efforts` says what `reasoning.effort` may be **for
  this model**, because the answer differs between models:
  `gemini-3.7-flash` rejects `minimal` and its siblings accept it. Empty when
  this process has no table for the model, in which case any level reaches
  the API for it to judge. A create naming a level its model refuses is
  answered `-32602` before the run starts, rather than reaching the provider
  and failing the response mid-stream.

  **The two providers' sets differ, and neither is a superset.** DeepSeek's is `low`,
  `high`, `max` — DeepSeek's `reasoning_effort` values, which the level maps
  onto — while the Gemini models take `low`, `medium`, `high` and sometimes
  `minimal`, neither of which DeepSeek advertises. A client that renders this array gets it right; a
  client that hardcoded one provider's set offers `minimal`, which DeepSeek
  refuses, and
  hides `max`, which works. Read the array.

  `medium` is the one value accepted without being advertised: DeepSeek maps
  it onto `high` rather than rejecting it, so a client offering Gemini's set
  is never refused for sending it, but nothing promises it is a distinct
  gradation, so it is not published as one.

### `initialized` (notification)

Empty params. Sent once, after `initialize` returns.

## Methods

| Method | HTTP counterpart | What it does |
| --- | --- | --- |
| `initialize` | — | Handshake. A pipe has no HTTP to negotiate over. |
| `initialized` | — | Handshake acknowledgement. |
| `responses.create` | `POST /v1beta/responses` | Start a run. |
| `responses.append` | — | Add input to a run already in flight. Steering. |
| `responses.cancel` | `POST /v1beta/responses/{id}/cancel` | Interrupt a run. |
| `responses.get` | `GET /v1/responses/{id}` | Read a response and its assembled output items. |
| `responses.delete` | `DELETE /v1beta/responses/{id}` | Forget a response. |
| `shutdown` | — | End the session. Equivalent to closing stdin. |

### `responses.create`

The Responses API's create-response body, narrowed, plus a `harness` block.

```jsonc
{
  "model": "gemini-3.7-flash",
  "input": "Add a test for the retry path.",
  "instructions": "You are working inside Turret.",
  "previous_response_id": "resp_9f0c…",
  "tools": [ /* see Tools */ ],
  "text": {"format": {"type": "json_schema", "name": "result",
                      "schema": { /* JSON Schema */ }}},
  "reasoning": {"effort": "high"},
  "max_output_tokens": 0,
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
| `background` | yes | **Refused** with `-32004` when true. Every response here is already answered at once and streamed as it goes, so there is no foreground to move off. |
| `input` | yes | A string, one `Content`, an array of `Content`, or an array of `message` (role `user`) `Step`s. Text only — see [Not built](#not-built). |
| `instructions` | no | **Prepended to the first user input**, not sent as a system instruction. See [Deviations](#deviations-from-the-http-surface). |
| `tools` | no | See [Tools](#tools). |
| `previous_response_id` | no | Continue that response's session, in this process. See below. |
| `text.format` | no | `schema` becomes the run's result schema, which the agent's `Complete` tool validates its answer against. |
| `reasoning.effort` | no | One of the levels `initialize` gave for this model — `low`, `medium`, `high` for `gemini-3.7-flash`, which refuses `minimal`. Defaults to `high`. A level the model does not take is `-32602`. |
| `reasoning.max_output_tokens` | no | Per-request output cap. Zero leaves the API's own default. |
| `stream` | no | Default true. See below. |
| `store` | no | Accepted and ignored: this process always stores, because the loop's state machine *is* its event log. |
| `harness.cwd` | yes, unless continuing | The directory the session works in. Absolute. |
| `harness.resume_session_id` | no | Continue that session, read out of the state directory. See [Resuming across process restarts](#resuming-across-process-restarts). |
| `harness.permission_mode` | no | `readonly` (default) or `full`. See [Permissions](#permissions). |
| `harness.deny` | no | Substring patterns matched against a call's descriptor. Only ever subtracts from what the mode allows. |
| `harness.max_sub_turns` | no | Ceiling on how many model round-trips the run may take. Zero is the harness default. |
| `harness.message_id` | no | Echoed on the user message item this input becomes. |
| `harness.title`, `harness.description` | no | Name the run in this harness's own records. |

**Result.** With `stream` true or absent, the answer is the response as at
`response.created` and the output items follow as notifications:

```jsonc
{"response": {"id": "resp_…", "object": "response",
                 "model": "gemini-3.7-flash", "status": "in_progress",
                 "created": "2026-09-07T01:02:03Z", "updated": "…",
                 "harness": {"session_id": "sess-…"}}}
```

With `stream: false` the answer is held until the run ends and carries the
whole finished response including `output`. The notifications are sent
either way, so a client that sets `stream: false` and ignores them gets
exactly the surface's non-streaming behaviour.

**Threads are `previous_response_id` chains.** There is no `thread/start`.
A create with no `previous_response_id` starts a new session in
`harness.cwd`; a create naming one continues *that response's session* with
the new input, and the run replays the whole conversation so far. Every
response in a chain reports the same `harness.session_id`, and each has its
own `id`. A continued create must not change `model` — the session's prompt
prefix is frozen for its life — and must not send `harness.cwd`, which it
inherits.

**`previous_response_id` is bounded by this process.** The id is minted in
memory and resolved from memory, so it means nothing to a process that did not
mint it: a client that sends one across a restart gets `-32001`. Crossing a
restart is `harness.resume_session_id`, below. Sending both on one create is
`-32602`.

**One at a time.** One process hosts one session, so a create while another
response is in progress is refused with `-32600`. Cancel it or wait.

### `responses.append`

Adds input to a response already in flight. This is steering.

```jsonc
{"response_id": "resp_…",
 "input": "actually, do it the other way",
 "harness": {"message_id": "msg-2"}}
```

Result:

```jsonc
{"response_id": "resp_…", "seq": 42}
```

The answer means the input is **committed**, not that the model has seen it. It
reaches the model at the next sub-turn boundary — after the current tool round
finishes, never in the middle of one — and appears then as a user message item
with `harness.source: "append"` and the `message_id` echoed. That boundary is
what keeps a steer from arriving halfway through a tool call.

An append to a response that is not `in_progress` is refused with
`-32002`; start a new response with `previous_response_id` set to it
instead.

### `responses.cancel`

```jsonc
{"response_id": "resp_…"}
```

Cancels the run and **waits for it to stop** before answering with the
response, whose `status` is then `cancelled` and whose `harness.reason` is
`"cancelled"`. The stream ends with an `response.completed` carrying the
same.

Cancelling a response that has already finished is **not an error**: the
caller asked for a state it is already in, and a client racing a completion
should not have to handle both outcomes. It answers with the response as it
stands.

A tool still running when the cancel arrives has its context cancelled, which
kills the whole process group of a `Bash` call. A child that has detached from
its process group and holds the output pipe open is bounded separately, by the
tool layer's own wait delay — the cancel returns in seconds either way.

### `responses.get`

```jsonc
{"response_id": "resp_…"}
```

Answers with the response including `output`: the assembled document, which
is exactly what a client would have built by folding the stream. Use it to
recover after a client-side drop, or to read a finished run without having
kept the notifications.

### `responses.delete`

Forgets the response. Answers `{}`, as the surface's own delete does. It does not erase the
run: the transcript under the state directory is untouched. A response
still running is refused with `-32002`.

### `shutdown`

Answers `{}` immediately, then cancels whatever is running and lets it record
its terminal event. Closing stdin does the same thing.

## Notifications

Every notification's method name **is** the `event:` name of the corresponding
Responses SSE frame, and its params **are** that frame's `data` payload, with
one addition: a `response_id` on every frame, because a pipe carries frames
for more than one response over its life where an HTTP response carries one.
Each also carries its own `type` and a monotonic `sequence_number`, as the
HTTP frames do.

| Method | On the HTTP surface | Params |
| --- | --- | --- |
| `response.created` | yes | `{type, sequence_number, response}` |
| `response.in_progress` | yes | `{type, sequence_number, response_id, harness:{sub_turn}}` |
| `response.output_item.added` | yes | `{type, sequence_number, response_id, output_index, item}` |
| `response.output_item.done` | yes | `{type, sequence_number, response_id, output_index, item}` |
| `response.output_text.delta` | yes | `{type, sequence_number, response_id, item_id, output_index, content_index, delta}` |
| `response.reasoning_text.delta` | yes | `{type, sequence_number, response_id, item_id, output_index, content_index, delta}` |
| `response.function_call_arguments.delta` | yes | `{type, sequence_number, response_id, item_id, output_index, content_index, delta}` |
| `response.completed` | yes | `{type, sequence_number, response}` |
| `response.failed` | yes | `{type, sequence_number, response}` |
| `harness.tool_output` | no | `{type, sequence_number, response_id, call_id, text}` |
| `harness.usage` | no | `{type, sequence_number, response_id, sub_turn, usage}` |

There is no `[DONE]`: the HTTP stream ends on `response.completed` and so does
this one, after which the pipe stays open for the next response.

The frames this process does **not** send, which the HTTP surface does:
`response.content_part.added` / `.done`, `response.output_text.done`,
`response.reasoning_text.done`, `response.function_call_arguments.done`, and
the `response.web_search_call.*` family. The `.done` frames carry the
completed value of something whose deltas the client already has, and
`response.output_item.done` carries the assembled item anyway; web search is a
server-side tool this harness does not use.

**An unrecognised notification must be ignored, in both directions.** This
process ignores any notification it does not know, and a client must do the
same: notifications will be added.

### Output item types

`response.output_item.added`'s `item` object carries a `type` that says which
of its fields are meaningful. These are the types this surface produces:

| `type` | Fields on `.added` | Deltas it emits |
| --- | --- | --- |
| `message`, `role: "user"` | `content` (complete), `harness.source`, `harness.message_id` | none |
| `reasoning` | — | `response.reasoning_text.delta` |
| `message`, `role: "assistant"` | — | `response.output_text.delta` |
| `function_call` | `call_id`, `name`, `arguments` (`{}`) | `response.function_call_arguments.delta` |
| `function_call_output` | `call_id`, `name`, `output` (complete), `harness.is_error`, `harness.rule`, `harness.truncated`, `harness.child_response_id` | none |

Two of those five are this protocol's own, and both follow from a response
being a whole agentic run rather than one model turn. On the HTTP surface a
`function_call_output` is something the *client* sends back in the next
request's input, and a user message only ever appears in input — here the loop
runs the tools itself and folds its own inputs, so both appear in the output,
as the same items in the same shapes in the order they happened.

`role` is what tells the two `message` items apart. A client rendering a
transcript keys on `type` **and** `role`, never on `type` alone: an assistant
message is the model's answer and streams its text as deltas; a user message
is what the loop put in front of the model and arrives complete.

Every item carries `id` and `status`. The id is `item_<output_index>` and is
what the deltas name in `item_id`; the status is `in_progress` on `.added` and
`completed` on `.done`.

### Content parts

An item's `content` (and a `function_call_output`'s `output`) is an array of
parts, typed by direction the way the Responses surface types them:

| `type` | Fields | Where |
| --- | --- | --- |
| `input_text` | `text` | a user message, a tool's output |
| `output_text` | `text` | an assistant message |
| `reasoning_text` | `text` | a reasoning item |
| `input_image` | `image_url` | a tool's output, when the tool produced an image |

`image_url` is a base64 data URL (`data:image/png;base64,…`), which is the
same string the provider is sent, so a parent renders a screenshot straight
from the frame.

### `harness.usage`

One request's token accounting, as it happens:

```jsonc
{"type": "harness.usage", "sequence_number": 41, "response_id": "resp_…",
 "sub_turn": 7,
 "usage": {"input_tokens": 11800, "output_tokens": 190,
           "total_tokens": 12043,
           "input_tokens_details": {"cached_tokens": 9216},
           "output_tokens_details": {"reasoning_tokens": 53},
           "harness": {"cost_usd": 0.0031}}}
```

The HTTP surface reports usage once, on the terminal frame, which for a run
spanning a hundred sub-turns is an hour late for anything showing spend as it
accrues. The `response.completed` total still arrives and is still
authoritative; these are its parts. `usage.harness.cost_usd` is this
harness's own price table applied to those tokens, and is absent when no
price table was loaded.

`output_tokens` includes the reasoning tokens, and
`output_tokens_details.reasoning_tokens` is the breakdown of it rather than a
sibling to add on. The Interactions vocabulary splits the same figures the
other way, so a client porting between them must not carry the arithmetic
across.

### `harness.tool_output`

Incremental stdout from a tool that is still running, addressed by the
`call_id` of the `function_call` item that started it. The HTTP surface has
nothing for it, because there the client runs the tools and already has the
output.

## Ordering guarantees

1. Notifications for one response arrive in the order this process
   produced them. Every frame goes through one writer.
2. Every a delta frame and `response.output_item.done` names an index some `response.output_item.added` opened.
   No item is added or done twice.
3. `response.created` is the first notification of a response and
   `response.completed` is the last. Nothing for that response follows
   it.
4. `response.completed` arrives after every output item of the run, including
   every `response.output_item.done`.
5. **Output items can overlap.** A `reasoning` item and an assistant
   `message` item are open at the same time during a sub-turn. Key items by
   `output_index`; do not assume the
   highest index is the only open one, and do not assume a `response.output_item.added` closes
   the item before it. The reason is in
   [Deviations](#deviations-from-the-http-surface).
6. An `responses.append` that has been answered is committed. Its
   user message item appears at the next sub-turn boundary, not immediately.

## Tools

The agent always has this harness's own tools — reading and writing files,
`Bash`, search, the plan tools, `Complete`. They are the session's whole
reason to be running on the parent's filesystem and they are not declared,
configurable or removable from the create body.

On top of those, the create body's `tools` array declares what the parent
wants the session to reach. Two of the surface's tool types are honoured, and they
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
 "params": {"response_id": "resp_…", "type": "function_call",
            "id": "call_98231", "name": "show_widget",
            "arguments": {"title": "…"}}}
```

`params` is a `function_call` item. `call_id` is the same `call_id` the client
already saw on that call's `function_call` item, so the answer renders under
the right call.

The client answers with a `function_call_output`'s own fields:

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

`web_search`, `file_search`, `code_interpreter`, `computer_use` and `mcp`
are refused with `-32004`. They are tools a provider runs on its own machines
as part of serving the response,
and this process is not serving the response — it is running the loop
itself and calling `/v1beta/responses` one model turn at a time.

## Resuming across process restarts

A session outlives the process that ran it. The row, its event log and its
frozen tool array are in the SQLite file under `-state-dir`, so a parent that
respawns `harness stdio-session` on the same directory can pick the
conversation up:

```jsonc
{"input": "carry on where you left off",
 "tools": [ /* the same tools, with connection metadata that is good now */ ],
 "harness": {"resume_session_id": "sess-3b71…"}}
```

The answer is a new response with a new `id` and the same
`harness.session_id`. The model is sent the whole conversation the earlier
process recorded, and the run continues with its sub-turn count, its plan and
its history intact — the same continuation `previous_response_id` performs,
reached by a different key.

Two things make this work.

**Name the state directory.** The default one is per-process and is removed at
exit, so a resume needs `-state-dir DIR`, the same `DIR` both times. A session
id the directory holds nothing for is `-32005`.

**Do not move the prefix.** The system prompt and the tool array are what
every request of a session shares, and the prompt cache is built on them, so a
resumed session sends the array it froze rather than one resolved fresh. The
create therefore inherits `model`, `harness.cwd`, `harness.permission_mode`,
`harness.deny` and `text.format` from the session, and naming any of them
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
tool in every mode and a refused call comes back as a `function_call_output` with
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
carries the surface's own error shape (`{"code": "…", "message": "…"}`), so a
client has the vendor's string code as well as this protocol's numeric one.

| Code | Name | When |
| --- | --- | --- |
| `-32700` | parse error | A line that is not valid JSON. Answered with no id; the line is skipped. |
| `-32600` | invalid request | `initialize` twice; a create while another response is running; a create during shutdown. |
| `-32601` | method not found | An unknown method sent as a request. An unknown *notification* is ignored instead. |
| `-32602` | invalid params | Malformed params, an unknown model, a missing `harness.cwd`, an unknown permission mode, a function tool from a client with no `function_calls` capability, a continued create that changes model. |
| `-32603` | internal error | A failure inside this process. |
| `-32000` | not initialized | Any method before the handshake completes. |
| `-32001` | response not found | A response id this process never minted, or has deleted. |
| `-32002` | response not running | An append or delete against a response that is not `in_progress`. |
| `-32003` | credentials missing | `responses.create` with no API key in the environment. |
| `-32004` | unsupported | `agent`; a server-side tool type; an `mcp_server` tool with no MCP client; image input. |
| `-32005` | session not found | A `harness.resume_session_id` this process's state directory holds no session for. |
| `-32006` | toolset mismatch | A resuming create whose tools do not reproduce the array the session froze. |

Failures *during* a run are not method errors — the create has already been
answered. They arrive as an `error` notification followed by
`response.completed` with `status: "failed"` and the message repeated in
`response.errors`.

### Response statuses

| `status` | Meaning |
| --- | --- |
| `in_progress` | Running. |
| `completed` | The run ended on its own terms. `harness.reason` says which way. |
| `incomplete` | The run hit `harness.max_sub_turns`. Continuing it with `previous_response_id` is the sensible next move. |
| `cancelled` | `responses.cancel`, `shutdown`, or stdin closing. |
| `failed` | The loop could not finish: a model error, a transport failure, a store failure. |

`harness.reason` on a completed response is `complete` (the agent called
`Complete`), `no_tool_calls` (it answered and asked for nothing else),
`max_sub_turns`, `complete_rejected` (its structured result failed the schema),
or `cancelled`. The surface's status enum does not separate an agent that finished
from one that ran out of room, and the difference decides whether a parent
offers to continue.

## Lifecycle

| Event | What happens |
| --- | --- |
| **stdin closes** | The read loop ends, a run in flight is cancelled, and the process waits for it to record its terminal event before exiting. The response's final status is `cancelled`. This is the normal way a parent ends a session. |
| **stdout breaks** | Writes fail silently — there is nowhere to report a failure to write. The read side discovers the same break and ends the session. |
| **the parent exits** | Both pipes break; as above. The process does not outlive its parent. |
| **a tool is running at cancel** | Its context is cancelled, which signals the whole process group of a `Bash` child. A grandchild holding the output pipe is bounded by the tool layer's own wait delay rather than waiting forever. |
| **the model errors mid-turn** | The stream ends, the loop records the failure, and the client gets `response.failed` carrying the whole response, then `response.completed` with `status: "failed"`. A reasoning-starved response — the budget spent before any answer text — is retried once at double the budget before that, and both attempts are billed and both appear on `harness.usage`. |
| **the parent stops reading stdout** | Frames queue in this process rather than being dropped. There is no ceiling: a parent that has stopped reading has stopped hosting the session, and stdin closing is what ends it. |

A run's transcript survives under the state directory. With the default state
directory it is removed on exit unless `-keep-state` is set; a directory the
parent named with `-state-dir` is always kept.

## Deviations from the HTTP surface

Each of these is a place a client written against the Responses API's docs would be
surprised.

**1. `responses.create` returns immediately even when streaming.** The HTTP surface
streams the events inside the HTTP response body. A JSON-RPC result is one
frame, so it cannot carry a stream: the create is answered with the response
as at `response.created` and the events follow as notifications. `stream:
false` restores the surface's shape by holding the answer until the run ends.

**2. A response is a whole agentic run, not one model turn.** On the HTTP
surface a `function_call` item ends the response with status
`requires_action`, the client runs the tool, and a second `responses.create`
with `previous_response_id` and a `function_call_output` continues it. Here the
loop does all of that internally: one response covers as many model turns
as the task takes, and the `function_call` and `function_call_output` items of every
one of them stream out as they happen. That is the whole point of the binary —
a client that wanted to drive the tool loop itself would call the provider directly.
`harness.sub_turn` on each item is how a client recovers the round-trip
boundaries.

**3. Output items overlap.** The HTTP stream never has two items open at once. Here a
`reasoning` item stays open while the assistant `message` item that follows it
streams. The reason is where the thought signature comes from: the harness
learns it at the *end* of a sub-turn, after the answer text has already
streamed, so closing the reasoning item before the answer starts would leave
its own text nowhere to go. Key items by `output_index`.

**4. `instructions` is prepended to the first user input.** This harness
renders its own system prompt, and that prompt plus the tool array is the
frozen shared prefix every request of a session sends — it is what the prompt
cache is built on, and moving a byte of it costs a full-price re-read of the
whole conversation. A client's instruction therefore lands on the first user
message instead, which is where a host's mode fragment or session preamble
belongs anyway. It is not silently dropped and it is not sent as a system
instruction.

**5. `previous_response_id` continues a *session*, not a stored
response.** The HTTP surface stores responses server-side and chains them —
DeepSeek's implementation does not even do that, calling the field "not
supported (stateless API)". Here
the chain is one local session resumed in place: the model sees the whole
conversation replayed, and the run continues from where it left off with its
sub-turn count, its plan and its history intact. Every response in a chain
reports the same `harness.session_id`. A stored id outlives any one process
because the provider holds it; these do not, which is why there is a second field
for reattaching to a session — see
[Resuming across process restarts](#resuming-across-process-restarts).

**6. `store` is ignored.** The surface lets a caller opt out of storage. This
process cannot: the loop's state machine is its event log, and a run that kept
nothing could not be resumed, folded into a request, or told what it had
already done. Whether that log survives the process is `-state-dir` and
`-keep-state`, not this field.

**7. `harness.cwd`.** The surface has no field for a working directory,
because there the model never touches one. This process works in a
directory that already exists on the parent's machine. Overloading
`environment` for it would leave nowhere for a real `environment` to go if one
ever became meaningful.

**8. Usage is reported per request as well as at the end.** See
[`harness.usage`](#harnessusage).

**9. `responses.cancel` is not restricted to background responses.**
The surface's cancel only applies to responses started with `background: true`.
Here it is the interrupt, and it applies to whatever is running.

**10. There is a `responses.append`.** The surface has no verb for adding input
to a response in flight, because on its surface there is no window in
which to add anything.

**11. `model` may name a model from either of two providers, and
`reasoning.effort` may then take a value one of them does not have.** One DeepSeek model is hosted here
([above](#which-models-this-process-hosts)), and its thinking levels are
`low`, `high`, `max` — DeepSeek's own `reasoning_effort` values, which the
level maps onto. `max` is the deviation: it is a legal value of this field
for that model and for no Gemini model. `minimal` moves the
other way, being legal there and refused here for that model.

Nothing about the frame changes, and nothing about the field's meaning
changes — it is still "how hard should this model think". A client that reads
`model_details.reasoning_efforts` rather than assuming one provider's set needs no
special case for either, which is the same discipline the Gemini models
already require of it, since `gemini-3.7-flash` refuses `minimal` and its
siblings accept it.

## Deviations from Codex's app-server

This binary was specified to mirror the method and notification shapes of
OpenAI's `codex app-server`, so that a client with a translator for Codex
reuses it. That was reversed deliberately: the payload vocabulary is the
Responses API's
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
| `thread/start`, `thread/resume` | `responses.create`, with and without `previous_response_id` | The surface already has a multi-turn model. A thread concept beside it would be a second name for the same chain. |
| `turn/start` | `responses.create` | A turn and a response are the same object under two names. |
| `turn/steer` | `responses.append` | Same semantics — input into a turn in flight, applied at a sub-turn boundary. |
| `turn/interrupt` | `responses.cancel` | The surface has a cancel. |
| `item/started`, `item/updated`, `item/completed` | `response.output_item.added`, the delta frames, `response.output_item.done` | Identical lifecycle, the surface's names. |
| `turn/started`, `turn/completed` | `response.in_progress`, `response.completed` | The surface's frames, with `harness.sub_turn` carrying what Codex's per-turn notifications carried. |
| token-usage notification | `harness.usage` | The surface's usage object rather than Codex's. |
| dynamic tools registered at `thread/start` | the `tools` array with `function` and `mcp_server` members | The surface's own tool union already has both shapes. |

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
- **More than one response at a time.** One process hosts one session. A
  parent that wants two sessions spawns two processes, which is also how it
  gets two working directories.
- **`response.in_progress` for anything but sub-turn boundaries.** The surface
  emits it on response-level transitions; here it only ever reports
  `in_progress` at the start of a sub-turn.
- **`harness.tool_output` for tools other than `Bash`.** Nothing else in the
  harness produces incremental output today.
