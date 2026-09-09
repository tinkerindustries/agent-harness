# The DeepSeek Responses API

Implemented and verified against the live API 2026-09-10 — see §4. The
vendored reference is `third_party/deepseek-docs/api/create-response.md` and
`third_party/deepseek-docs/guides/responses_api.md`, both fetched 2026-08-27.

DeepSeek serves two surfaces at the same base URL. `internal/deepseek` now
speaks both: `POST /chat/completions` (intent.go, stream.go) and
`POST /responses` (responses.go, responsesstream.go, responsesclient.go). One
provider, one transport, one narrow seam above them — two dialects
underneath.

`harness stdio-session` posts to `/responses`. `harness serve` still posts to
`/chat/completions`, and §5 says why that is a decision rather than an
oversight.

---

## 1. Why a second dialect at all

The stdio entry point speaks the Responses vocabulary to its parent
([`STDIO-PROTOCOL.md`](STDIO-PROTOCOL.md)). Speaking it to the provider as
well makes one vocabulary run the length of the process: the parent's
`function_call` item and the provider's are the same shape under the same
name, so a client learns one set of shapes rather than two.

It does not make the process a proxy. Both ends are rendered independently
from the session's own event log, and the outbound one passes through
`internal/fold`'s `[]wire.Message` — the loop's provider-neutral vocabulary,
which is modelled on Chat Completions — before `inputFromMessages` turns it
into input items. The translation in §2 is real work happening on every
request; what changed is that the shape it produces is the shape the parent
already speaks.

Two things fall out of it that are worth having on their own.

**The vision path stops resting on undocumented behaviour.** A Chat
Completions tool message's `content` is a string in DeepSeek's own schema,
with no array variant, and `guides/vision.md` says images ride in `user`
messages only. The harness sends an image inside a tool message anyway, on a
live measurement that contradicts both
([`DEEPSEEK-VISION.md`](DEEPSEEK-VISION.md) §2), and that risk was accepted
knowingly. The Responses API **documents** `input_image` parts inside a
`function_call_output`'s `output`. Same image, same base64 data URL,
same place in the conversation — and a schema that says so.

**Usage arrives without asking.** Chat Completions needs
`stream_options.include_usage` to report tokens on a stream; here the
terminal event carries the whole response object, usage included.

## 2. The mapping

The loop states `wire.ChatIntent` and never learns which surface answered
it. Everything below happens inside `internal/deepseek`.

| The loop's intent | Chat Completions | Responses |
| --- | --- | --- |
| system prompt | `messages[0]`, role `system` | `instructions` |
| user message | `messages[]`, role `user` | `input[]` item, `type: message` |
| assistant text | `messages[]`, role `assistant` | `input[]` item, `type: message`, `output_text` parts |
| assistant reasoning | `reasoning_content` on the message | its own `input[]` item, `type: reasoning` |
| tool call | `tool_calls[]` on the message | its own `input[]` item, `type: function_call` |
| tool result | `messages[]`, role `tool` | `input[]` item, `type: function_call_output` |
| image in a tool result | an `image_url` part in a tool message (undocumented) | an `input_image` part in the item's `output` (documented) |
| image URL envelope | `{"image_url": {"url": "data:..."}}` | `"image_url": "data:..."` |
| thinking on/off | `thinking: {"type": "enabled"/"disabled"}` | `reasoning: {"effort": ...}`, `none` is off |
| effort | `reasoning_effort` | the same `reasoning.effort` field |
| token ceiling | `max_tokens` | `max_output_tokens` |
| tool schema | `{"type":"function","function":{...}}` | flattened: `{"type":"function","name":...}` |
| usage on a stream | ask with `stream_options` | on the terminal event |
| cache split | two figures, hit and miss | one input total, hit nested; miss derived |
| stream framing | chunks, ending at `data: [DONE]` | semantic events, ending at `response.completed` |

**The reasoning item is not optional.** DeepSeek requires an assistant turn's
chain-of-thought to be replayed in every later turn of a request that
carries `tools`, and answers 400 when it is missing
(`third_party/deepseek-docs/guides/thinking_mode.md`, "Tool Calls"). Every
request this harness sends carries tools. `inputFromMessages` therefore emits
a `reasoning` item before the assistant message it belongs to, which is where
Chat Completions' `reasoning_content` field goes on this surface.

## 3. What the loop above never sees

Four things are the provider's rather than the surface's, and
`ResponsesClient` embeds `*Client` so they stay one implementation: the
retry classification, `UsageSplit`, `CacheSlack`, and both quirk repairs
(`IsReasoningStarved`, `RepairArguments`). A test pins that they carry over.

Two are translated so the loop's own vocabulary still fits:

- **Finish reasons.** This surface reports a `status` plus, when truncated,
  an `incomplete_details.reason`. `finishReasonFor` maps that onto
  `wire.Finish*` — `incomplete`/`max_output_tokens` becomes `length`, which
  is what makes the reasoning-starvation retry fire here too.
- **Tool call assembly.** A chat chunk carries its call's index on every
  fragment. Here the call is announced once by `response.output_item.added`
  and its arguments arrive on later events; both carry `output_index`, so
  `wire.ToolCallAssembler`'s own keying is enough and the decoder holds no
  state.

## 4. Verified against the live API

Four probes on 2026-09-10, against `deepseek-v4-flash` and
`deepseek-v4-flash-vision-exp`, driving the harness's own client rather than
hand-built JSON. Recorded in [`OBSERVED.md`](OBSERVED.md); the request body
those probes sent is pinned byte for byte by
`TestResponsesRequestBody`.

| probe | result |
| --- | --- |
| streaming, with a reasoning item and a `function_call`/`function_call_output` pair replayed | 200; the model read the tool result and answered from it |
| streaming, first turn, tool forced | 200; call assembled with its id, name, and valid JSON arguments; finish `tool_calls` |
| streaming, image inside `function_call_output`, vision model | 200; a solid `rgb(0,153,102)` image named correctly |
| non-streaming | 200; output items flattened back to one assistant message; prefix cache hit 256 of 357 input tokens |
| **a whole agentic session over the stdio protocol** | completed in 2 sub-turns; `Read` called and its result used; 6,222 input tokens with 3,072 cached |

The cache hits are worth noting on their own: prefix caching works on this
surface, so the property the whole request shape is built around survives the
move.

The last row is the one that matters most, and it is what closed the gap the
first four left. A parent drove `harness stdio-session` over a pipe with a
task requiring a tool call — read a file, report what is in it — and the run
completed on its own terms: `response.created`, a user message item, a
`reasoning` item, `function_call(Read)`, `function_call_output`, an assistant
message, `function_call(Complete)`, `response.completed` with status
`completed` and reason `complete`. One vocabulary from the parent's frame to
the provider's request and back.

That run is also what found the one bug this surface had that Chat Completions
did not: an intent with no token ceiling was sending
`"max_output_tokens": 0`, which DeepSeek refuses with "the valid range of
max_tokens is [1, 393216]". The field is nullable and zero is not a legal
value for it, so it is `omitempty` now and an unset ceiling means the model's
own default. `harness serve` never hit it because it always resolves a
ceiling from its settings; a hosted session takes whatever the create body
named, and a create may name none.

### Still unverified

- **Cache behaviour across a long session.** The end-to-end run hit 3,072
  cached tokens of 6,222, so the prefix survives a sub-turn boundary; that is
  not the same as staying stable the way [`CACHE.md`](CACHE.md) requires over
  hundreds of them.
- **Vision through the whole loop on this surface.** The image probe was one
  hand-built request. Nothing has yet driven `Read` on an image file through
  a hosted session on the Responses path.
- **`response.incomplete` and `response.failed` from the live API.** Both are
  decoded and unit-tested against scripted frames; neither has been seen
  from DeepSeek itself.

## 5. Why `harness serve` still posts to Chat Completions

A session speaks one surface for its whole life. The head of every request is
the frozen prefix the prompt cache is built on ([`DESIGN.md`](DESIGN.md)
§3.2), and the two dialects do not serialise alike, so a session that changed
surface mid-run would re-read its whole conversation at full price — and
`serve` resumes sessions that started before this existed.

Switching it is a one-line change in `cmd/harness/serve.go`'s client
construction. What it needs first is §4's "still unverified" list closed,
because `serve` is the production stack and a run there is not a probe.
