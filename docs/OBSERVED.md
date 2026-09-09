# Observed API behaviour

Measured against the live API on 2026-08-09, both models, thinking mode enabled.
[VALIDATION.md](VALIDATION.md) records what the docs say; this file records what
the API does. Where they disagree, this file wins.

Server build during these runs:
`fp_a18b46594c_prod0820_fp8_kvcache_20260402`. Re-measure if that changes —
several findings below are properties of the serving configuration, not the
protocol.

Each section names the model it was measured on. The cache block size,
`tool_choice` behaviour, and the reasoning finding were checked on both and
agree; the token-volume figures differ sharply between them.

## The cache is blocked at 128 tokens

The single most useful measurement. Cached prefix length is always the largest
multiple of 128 that fits inside the common prefix:

    hit = floor(common_prefix_tokens / 128) × 128

Five prefix sizes, all exact:

| prompt tokens | hit | miss | floor(n/128)×128 |
| --- | --- | --- | --- |
| 609 | 512 | 97 | 512 |
| 919 | 896 | 23 | 896 |
| 1699 | 1664 | 35 | 1664 |
| 2609 | 2560 | 49 | 2560 |
| 3893 | 3840 | 53 | 3840 |

The trailing partial block — 0 to 127 tokens — never hits.

Three consequences, and they revise [CACHE.md](CACHE.md).

**One prior request is enough.** A request diverging in its suffix hit 3840 of
3893 tokens after exactly one previous request. Common-prefix detection, which
the docs present as needing two diverging requests, is not on the critical path.
Fixed-interval persistence at 128 tokens covers it.

**Divergence wastes at most 127 tokens beyond the divergence point.** The
docs' Example 2 — where `A + B` then `A + C` misses entirely — only bites when
`A` is under 128 tokens. At coding-harness prefix sizes it does not apply.

**The invariants still matter completely.** The block size bounds tail waste; it
does nothing about early divergence. A clock at token 50 of the system prompt
sets the common prefix to 50, so `floor(50/128) × 128 = 0` and the entire
conversation misses. Cheap tail, catastrophic head. The discipline in CACHE.md
stands; only the warmup was wrong.

## Kimi K3 over-predicts the cache hit

The churn detector predicts a sub-turn's hit as `floor(previous / 128) × 128`
and reports churn when the actual miss exceeds the prediction by more than the
provider's tolerance. On DeepSeek expected and actual agree to within one
block. On Kimi K3 the prediction consistently overshoots. Thirteen sub-turns
across three live `kimi-k3` sessions:

| run | sub-turn | prompt | detector expected hit | actual hit | residual |
| --- | --- | --- | --- | --- | --- |
| 1 | 2 | 4120 | 3712 | 3584 | −128 |
| 1 | 3 | 4282 | 4224 | 4096 | −128 |
| 1 | 4 | 4399 | 4352 | 4096 | −256 |
| 2 | 2 | 4100 | 3712 | 3584 | −128 |
| 2 | 3 | 4498 | 4224 | 4096 | −128 |
| 2 | 4 | 4600 | 4480 | 4352 | −128 |
| 3 | 2 | 4088 | 3712 | 3584 | −128 |
| 3 | 3 | 4462 | 4352 | 3840 | −512 |
| 3 | 4 | 4749 | 4480 | 4352 | −128 |
| 3 | 5 | 5120 | 4992 | 4608 | −384 |
| 3 | 6 | 5448 | 5120 | 5120 | 0 |
| 3 | 7 | 5726 | 5504 | 5376 | −128 |
| 3 | 8 | 5828 | 5632 | 5632 | 0 |

Residuals: −128 ×8, 0 ×2, −256 ×1, −384 ×1, −512 ×1. Never positive — the
detector never under-predicts on Kimi.

**What this does and does not mean.** Every actual hit is a multiple of 256,
and the over-prediction is bounded at 512 across every observation. The cache
demonstrably works: within a session the hit rate climbed 0% → 88% → 91% → 94%
→ 97%, and the eight-sub-turn run ended at 83.1%. Real churn would collapse
that curve, not improve it.

No block size explains the residuals. A 512-token block was hypothesised and
falsified — hits 3840, 4352, and 5376 are not multiples of 512. Why the
over-prediction varies between one and four blocks is unknown, and no mechanism
is claimed here.

The detector's Kimi tolerance is therefore 512 — the largest observed
over-prediction, so no observed healthy sub-turn reports churn. That is an
empirical bound, not a property of Kimi's cache, and it rests on thirteen
observations from three sessions. A later run at different prompt sizes could
exceed it.

## Streaming tool calls arrive incrementally

Settled. Deltas use OpenAI's indexed form, so the assembler design was right.

Opening frame carries identity and an empty argument string:

```json
{"index":0,"id":"call_00_Xwh0...","type":"function",
 "function":{"name":"get_weather","arguments":""}}
```

Every later frame carries only the index and a fragment:

```json
{"index":0,"function":{"arguments":"{"}}
{"index":0,"function":{"arguments":"\""}}
{"index":0,"function":{"arguments":"location"}}
{"index":0,"function":{"arguments":"H"}}
{"index":0,"function":{"arguments":"ob"}}
{"index":0,"function":{"arguments":"art"}}
```

Arguments fragment mid-token and mid-string. Accumulate by index and parse only
once the call completes.

The vendored streaming schema lists only `content`, `reasoning_content`, and
`role` in the delta. It is incomplete: `tool_calls` is present.

## Assembled arguments are occasionally one brace out

Measured on 2026-08-13, over the production HTTP logs for 10–13 August: 83
sessions, 10,542 recorded exchanges, 11,766 assembled tool calls, all but two
sessions on flash.

Five calls — 0.04% — assembled into arguments that are not valid JSON. Every
one of them was `Complete`, on flash, on an arguments object between 7.5 kB and
11 kB, and every one stopped with `finish_reason: "tool_calls"`. None was
truncation: a run out of `max_tokens` reports `length`, and none of these did.
Each is a single brace in the wrong place.

| Shape | Seen | Repairable |
| --- | --- | --- |
| One closing brace short at the end | 2 | Yes, by appending it |
| One closing brace too many after a complete object | 1 | Yes, by dropping it |
| Object closed one key early, then another key follows | 2 | No — see below |

The third shape is `{"result":{…},"error":""},"summary":"…"}`. Deleting either
brace produces valid JSON, and the two readings disagree about whether `error`
belongs inside `result` or beside it. Nothing in the bytes decides it; only the
tool's schema does. `deepseek.RepairArguments` therefore repairs the first two
shapes and leaves the third, on the grounds that a wrong guess is worse than a
rejection — a rejected call costs a sub-turn, an accepted call carrying a shape
the model never wrote is a wrong answer.

The repair is gated on `finish_reason`, because truncated arguments are also
one brace short and balancing them would present a partial result as a complete
one.

What it buys is a better error rather than a saved sub-turn. All five of these
calls had a second problem underneath the brace — fields nested wrongly, or a
required property missing — so the executor would have rejected them anyway.
The difference is what the model is told: `invalid arguments: unexpected end of
JSON input` names a position in a byte stream it cannot see, whereas `result:
missing required property "error"` names the fix. In the one session where the
model received that second message, it corrected itself on the next call.

## Usage does not arrive on its own chunk

The API reference says `stream_options.include_usage` adds "an additional chunk
before `data: [DONE]`" whose "`choices` field will always be an empty array."

Not what happens. Usage rides on the final content chunk, which has a populated
`choices` array and a `finish_reason`:

```json
{"choices":[{"index":0,"delta":{"content":"","reasoning_content":null},
 "finish_reason":"tool_calls"}],
 "usage":{"prompt_tokens":293,...}}
```

Then `data: [DONE]`.

A parser that waits for an empty-choices chunk will never find one. Read usage
wherever it appears and terminate on `[DONE]`.

## tool_choice is partly supported, not rejected

`oh_my_pi.md` says thinking mode "rejects the `tool_choice` parameter". That is
too broad. Measured:

| Value | Result |
| --- | --- |
| `"auto"` | 200, tool call produced |
| `"none"` | 200, text produced |
| `"required"` | 400 `Thinking mode does not support this tool_choice` |
| `{"type":"function","function":{"name":...}}` | 400, same message |

So thinking mode supports the two permissive values and rejects the two coercive
ones.

The restriction is specific to thinking mode, not to the API. With
`thinking: {"type":"disabled"}` both coercive values return 200 and the forcing
works: given the prompt "Hello, just say hi." and a weather tool, both
`required` and the named form produced a `get_weather` call against the obvious
intent of the prompt.

| | `required` | named tool |
| --- | --- | --- |
| thinking enabled | 400 | 400 |
| thinking disabled | forced call | forced call |

Consequences. The main loop runs thinking-on permanently, so it can never
guarantee a tool call — no forced `TaskCreate` at plan time, and no way to stop
the model answering in prose when action was wanted. Prompt wording is the only
lever there.

Side work is different. `WebFetch` extraction, compaction summaries, and session
titles already run thinking-off in their own flash conversations
([MODELS.md](MODELS.md)), so they can force a named tool. That is a better
structured-output mechanism than `response_format: {"type":"json_object"}`,
which guarantees only valid JSON rather than schema conformance, and which the
docs warn can occasionally return empty content.

Our rule of never sending `tool_choice` from the main loop stands, because
`auto` is the default when tools are present.

## The reasoning_content 400 does not reproduce

The most consequential result here, and a negative one.

Three sources say omitting `reasoning_content` from a tool-call assistant
message returns 400. `guides/thinking_mode.md`: "the API will return 400."
`oh_my_pi.md`: "Skipping this causes 400." `copilot_cli.md` quotes the error
text verbatim. DESIGN.md §3.1 was built on it.

It could not be provoked. Every one of these returned 200:

| Configuration | flash | pro |
| --- | --- | --- |
| Single tool call, reasoning removed from the assistant message | 200 | — |
| Single tool call, `content: null` | 200 | — |
| Two chained tool calls, reasoning removed from the first | 200 | 200 |
| Two chained tool calls, reasoning removed from the second | 200 | 200 |
| Two chained tool calls, reasoning removed from both | 200 | 200 |
| Reasoning set to `null` rather than removed | 200 | 200 |
| A second user turn with all turn-1 reasoning stripped | — | 200 |
| The same, with `tools` omitted from the request | — | 200 |

Coverage is decent but cannot prove a negative. The honest statement is that the
requirement does not enforce on build `prod0820_fp8_kvcache_20260402`, on either
model, across the configurations above. The third-party notes plainly describe
real errors, so the likeliest reading is that the API was relaxed and both the
docs and those configs describe earlier behaviour.

What follows for the design. Keep replaying reasoning — the behaviour does not
change. It is what the docs prescribe, the cost is negligible because the
replayed tokens sit inside the cached prefix, and the docs give a quality reason
("allowing the model to continue its previous reasoning") that this test says
nothing about either way.

What does change is that there is no 400 cliff to engineer around. Compaction
may drop reasoning from older turns freely. A session resumed from a store that
lost reasoning degrades rather than hard-fails. And §3.1 is a strong convention
rather than a load-bearing constraint, which is a materially different thing to
build on.

## max_tokens bounds reasoning, and reasoning spends it first

`max_tokens` caps reasoning and content together. Reasoning is generated first,
so an insufficient budget is consumed entirely by reasoning and the answer never
starts:

| max_tokens | finish_reason | completion | reasoning | content |
| --- | --- | --- | --- | --- |
| 300 | `length` | 300 | 300 | `''` |
| 1200 | `length` | 1200 | 1200 | `''` |
| 4000 (pro, hard problem) | `length` | 4000 | 4000 | `''` |

Every one of those is billed in full and returns nothing usable.

The harness must treat `finish_reason == "length"` with empty `content` as its
own condition — reasoning starved the answer — and distinguish it from an answer
that was truncated mid-sentence. The first calls for a retry with a larger
budget; the second calls for continuation. Both are wasted spend if
misdiagnosed, and at max effort on pro the waste is real money.

### How much budget is actually needed

Flash, max effort, 20000 ceiling:

| Prompt | reasoning | answer | total | wall |
| --- | --- | --- | --- | --- |
| "Say hello." | 21 | 3 | 24 | 1s |
| Reverse a slice, one function | 179 | 59 | 238 | 3s |
| A real SSE reader with error handling | 10844 | 2191 | 13035 | 117s |
| Hard combinatorics proof | 20000 | 0 | starved | 194s |

Reasoning runs about five to one against the answer on real work, so the budget
is set by reasoning volume rather than expected output size. A realistic coding
task used 65% of 20000. A hard problem consumed all of it and produced nothing.

Sizing and the retry rule live in [MODELS.md](MODELS.md).

### Latency at max effort is minutes, not seconds

117s for a real coding task, 194s for a hard problem. Neither is a fault.

This is the strongest argument for streaming everything. A non-streaming request
running three minutes is indistinguishable from a hang, and the documented
ten-minute pre-inference hold sits on top of it. The idle watchdog has to be
sized against gaps between deltas, never against total request duration.

## Pro matches flash where it matters

| Property | flash | pro |
| --- | --- | --- |
| Cache block size | 128 tokens | 128 tokens |
| `tool_choice: auto` / `none` | accepted | accepted |
| `tool_choice: required` / named | 400 | 400, same message |
| reasoning round-trip enforced | no | no |

Pro block sizes measured at two prefix lengths: 850 → 768, and 2410 → 2304. Both
are exactly `floor(n/128) × 128`. CACHE.md's arithmetic holds on the model we
actually default to.

Pro's effort mapping is settled, on the third attempt. An easy problem did not
discriminate and a hard one saturated the ceiling; a realistic coding task with
a 40000 ceiling did:

| effort | reasoning | answer | total | wall |
| --- | --- | --- | --- | --- |
| `low` | 10635 | 1762 | 12397 | 260s |
| `high` | 10537 | 1659 | 12196 | 277s |
| `max` | 25777 | 2364 | 28141 | 583s |

`low` and `high` land within 1% of each other, so pro collapses `low` into
`high` exactly as `guides/thinking_mode.md` described at the time.

These numbers were taken against the preview build. Pro went GA as V4-Pro-0813
on 2026-08-13 and the mapping table now gives `low` its own level on both
models, so this row needs re-measuring before it can be quoted again.

`max` is 2.4× the reasoning and 2.1× the wall-clock. That is what moved the
harness default to `high` ([MODELS.md](MODELS.md)).

## Parallel tool calls, confirmed

One assistant message returned two calls for the prompt "What is the weather in
Hobart, and also in Perth?":

```
call_00_dmW4ptK0w1ETVL...  get_weather({"location": "Hobart"})
call_01_NgT8bWg9dyxlcS...  get_weather({"location": "Perth"})
```

The identifier prefix encodes the array index — `call_00_`, `call_01_`. Useful
for asserting ordering in tests, though the `index` field is the thing to key
on.

This is the case the ordered-append rule exists for ([CACHE.md](CACHE.md)).

## Smaller findings

**`prompt_tokens_details.cached_tokens` exists** and mirrors
`prompt_cache_hit_tokens`. Undocumented; the OpenAI-shaped alias of the same
number. Either can be read.

**`delta.content` is explicitly `null`** while reasoning streams, rather than
being absent. The Go struct needs a nullable string, not an empty-string
default, or an early `content == ""` check will fire on every reasoning frame.

**A single tool definition cost about 280 prompt tokens.** A one-line user
message with one small tool schema came to 293 prompt tokens against 9 for the
same message with no tools. Eleven tools should land in the 2–3K range, which is
the stable head worth caching.

**Reasoning dominates output even at `low` effort.** A prompt answered with the
literal two-token string `ok` produced 51 completion tokens, 49 of them
reasoning. Budget output cost on reasoning volume, not answer length.

**No keep-alive comments appeared** in any of these runs. They are documented
for queued requests, and nothing here queued. The parser still has to handle
them.

**Latency** was 1.9s for a trivial non-streaming call at low effort.

## Still untested

Whether pro honours `low` distinctly — attempted, inconclusive, and abandoned as
not decision-changing. Effort changes mid-session and their effect on the cache.
Long-context behaviour near the 768K compaction threshold. Keep-alive frames
under real queueing, which cannot be provoked on demand. Whether the reasoning
round-trip requirement returns on a later server build.

## Gemini 3.7 Flash — Interactions API

Measured against the live API on 2026-08-21: `POST
https://generativelanguage.googleapis.com/v1beta/interactions`, model
`gemini-3.7-flash`, `store: false` throughout (the stateless-replay decision
[GEMINI-INTEGRATION.md](GEMINI-INTEGRATION.md) §5.3 makes), `thinking_level:
"low"` except where noted. This section resolves that plan's §6 "Open" list.
Where a finding contradicts it, said so explicitly — GEMINI-INTEGRATION.md's
own header states this file wins where they disagree.

### `thinking_level: "minimal"` is refused by this model

Measured 2026-09-08 through `harness gemini-session` against the live API.
A create carrying `generation_config: {thinking_level: "minimal"}` is answered
`400`:

```
{"error":{"message":"'minimal' is not a supported thinking level for this
model. Allowed values are: medium, low, high.","code":"invalid_request"}}
```

The levels are a property of the **model**, not of the API.
`third_party/gemini-docs/thinking.md`, "Levels Supported", agrees and gives
the split: `gemini-3.7-flash` and `gemini-3.1-pro-preview` take `low, medium,
high`; `gemini-3.6-flash`, `gemini-3.5-flash` and `gemini-3.5-flash-lite`
take `minimal` as well.

Two places in this repository had it wrong, both now corrected:
`docs/STDIO-PROTOCOL.md` documented the four-value enum for a surface whose
only model is `gemini-3.7-flash`, and `google.vision_thinking_level` offered
`minimal` although the vision default is the same model — pinning it would
have failed every `Glance`, `Ground` and `Detect` call. `internal/gemini`
now holds the per-model table (`LevelsFor`, `LevelSupported`), the stdio
handshake advertises it as `thinking_levels`, and a create naming a level its
model refuses is `-32602` before the run starts rather than a 400 that fails
the interaction mid-stream.

Raw request/response captures live under `internal/gemini/testdata/` as
`.sse` fixtures for Phase 4; each subsection below names the one it produced.

### `tool_choice` is nested in `generation_config`, not top-level

An earlier draft of this section concluded `tool_choice` "does not exist on
this surface". It does; it was being sent in the wrong place. Re-measured
2026-08-21, all seven shapes:

| request | result |
| --- | --- |
| top-level `"tool_choice": "auto"` | `400 {"error":{"message":"Unknown parameter 'tool_choice'.","code":"invalid_request"}}` |
| `generation_config.tool_choice: "auto"` | 200, steps `[thought, function_call]` |
| `generation_config.tool_choice: "any"` | 200, steps `[thought, function_call]` |
| `generation_config.tool_choice: "none"` | 200, steps `[thought, model_output]` — call suppressed |
| `generation_config.tool_choice: "validated"` | 200, steps `[thought, function_call]` |
| `generation_config.tool_choice: {"allowed_tools":{"mode":"any","tools":["get_weather"]}}` | 200, steps `[thought, function_call]` |
| any of the above with header `Api-Revision: 2026-05-20` | 200, no observable difference |

The top-level 400 is real and is what misled the first pass — it is the API
correctly rejecting a misplaced field, not the absence of a feature. Both the
prose docs ("Control how the model uses tools using `tool_choice` in
`generation_config`",
`third_party/gemini-docs/interactions/function-calling.md`) and
`openapi.json`, where `tool_choice` is a property of `GenerationConfig`, agree
with the measurement.

`"none"` genuinely suppresses the call rather than merely discouraging it,
which makes it usable as a hard off-switch.

This corrects GEMINI-INTEGRATION.md §3's table and §5.3's example body, which
showed the field at the top level. The harness still has no reason to *send*
it — `auto` is the default and is what the agent loop wants — so the practical
rule is unchanged from DeepSeek's: omit it.

### Thought signatures — the load-bearing findings

**Happy path, confirmed end to end.** Capture the signature from turn 1,
replay it verbatim in turn 2 alongside the `function_call` and
`function_result` steps: 200, coherent answer, every time this was tried.

Where the signature appears:

- **Streamed:** as its own `step.delta` frame on the `thought` step, between
  that step's `step.start` and `step.stop`:
  ```
  event: step.delta
  data: {"index":0,"delta":{"signature":"...","type":"thought_signature"},"event_type":"step.delta"}
  ```
  Matches GEMINI-INTEGRATION.md §5.2 exactly.
- **Unary (`stream: false`):** the signature sits directly on the step
  object in the response's `steps` array, not nested in a delta:
  `{"type":"thought","signature":"..."}`. That is also exactly the shape a
  replayed request expects back in `input` — the wire shape the API emits
  unary is the wire shape it wants replayed.

**Omitting it — the most important measurement in the phase.** Always a 400.
The exact wording depends on what's wrong:

| What's missing from the `thought` step | HTTP status | Message |
| --- | --- | --- |
| Step dropped from `input` entirely | 400 | `Request contains an invalid argument.` |
| Present, `signature: ""` | 400 | `Request contains an invalid argument.` |
| Present, `signature` field absent | 400 | `Request contains an invalid argument.` |
| Present, signature replaced with unrelated garbage text | 400 | `Corrupted thought signature.` |

**The error body itself is a finding.** All four of these arrive
**SSE-framed**, not as the plain `{"error":{...}}` JSON envelope
`internal/gemini/client.go`'s `parseAPIError` expects:

```
event: error
data: {"error":{"message":"Corrupted thought signature.","code":"invalid_request"},"event_type":"error"}
```

— even though the HTTP status code is a plain 400. Compare the `tool_choice`
rejection above, which *is* plain JSON with no SSE framing: that one is caught
by pre-flight request validation, before a stream starts; a bad thought
signature is only caught once generation begins, so the error surfaces
inside the stream that was already opened. `parseAPIError` as it exists today
will not parse this — `json.Unmarshal` fails on the `event: error\ndata: ` line
and it falls into the raw-body fallback, so the operator sees `unexpected
status 400: event: error\ndata: {...}` instead of the actual message. Phase 4's
client must check for an SSE-framed error before, or instead of, the
plain-JSON path. Fixture: `internal/gemini/testdata/stream-error-corrupted-signature.sse`.

**Both documented bypass values work.** Replacing the real signature with
either string, independently:

| Bypass value | Result |
| --- | --- |
| `context_engineering_is_the_way_to_go` | 200, valid answer |
| `skip_thought_signature_validator` | 200, valid answer |

This settles GEMINI-INTEGRATION.md §5.2's sharp edge: **compaction can work for
Gemini sessions.** A compacted history's synthetic thought step can carry
either bypass string as its `signature`, and the API accepts it and answers
normally. Both bypass calls reported `total_thought_tokens: 0`, versus a
nonzero count on every real thought step measured elsewhere — consistent with
the model accepting the placeholder rather than trying to reconstruct
anything from it.

**The parallel-call signature rule holds.** One prompt provoking three
`get_weather` calls produced exactly one `thought` step (index 0, carrying
the signature) followed by three `function_call` steps (indices 1–3), none of
which carried a signature of their own. Replaying that single thought
signature plus all three function_calls and function_results round-tripped
successfully (200, correct three-city answer). Signature is per-turn, not
per-call, on this surface — GEMINI-INTEGRATION.md §5.2's "documented for the
legacy surface, unverified here" note is now verified. Fixture:
`stream-parallel-calls.sse`.

### Full `function_call` → `function_result` round trip

Confirmed shape end to end, two requests, `store: false`, manual replay. The
`function_result` step:

```json
{
  "type": "function_result",
  "name": "get_weather",
  "call_id": "call_3554686",
  "result": [{"type": "text", "text": "Sunny, 18C, light breeze."}]
}
```

matches GEMINI-INTEGRATION.md §3 exactly — `name`, `call_id`, and `result` as
an array of typed content blocks. The `function_call` step it replays:

```json
{"type": "function_call", "id": "call_3554686", "name": "get_weather", "arguments": {"location": "Hobart, Tasmania"}}
```

`arguments` is a genuine JSON object on the wire (both directions), confirming
§3's table entry.

`interaction.id` is the empty string `""` throughout, on both
`interaction.created` and `interaction.completed`, whenever `store: false` —
never populated. `previous_interaction_id` was not needed and not attempted;
full manual replay works without it. Fixtures:
`stream-tools-function-call.sse` (turn 1: thought+signature, one
function_call), `stream-thought-signature-replay.sse` (turn 2: replayed
thought+signature followed by the final `model_output` text).

### `arguments_delta` never fragmented — contradicts the streaming guide's wording

GEMINI-INTEGRATION.md §5.4 quotes the streaming guide: "You must accumulate
these deltas to get the full arguments," which reads as multi-frame
fragmentation the way OpenAI-format `tool_calls` deltas are confirmed to
fragment above. Not observed here, at any size tried:

| Call | Arguments size | `arguments_delta` frames |
| --- | --- | --- |
| `get_weather({"location":"Hobart, Tasmania"})` | 33 bytes | 1 |
| Three parallel `get_weather` calls | 33–35 bytes each | 1 each |
| `save_document` essay | ~48.5 KB body | 1 |
| `save_document` longer essay | ~69.4 KB body | 1 |

Six function calls across five different requests, 33 bytes to 69 KB, every
one delivered as exactly one `arguments_delta` frame carrying the complete,
valid JSON string.

This does not make `wire.ToolCallAssembler` the wrong tool: its `Add` folds
whatever arrives by index and one frame is just the degenerate case of
"several," so it stays correct either way and should still be reused. What it
does mean is the plan's implied justification — that something is needed to
reassemble fragments *because* they always fragment — doesn't hold for
`gemini-3.7-flash` on this endpoint, at least up to 69 KB. Whether a
larger-still generation ever splits across frames is untested; treat the
assembler as defensive plumbing here, not confirmed-necessary plumbing.

### `RepairArguments` — no work found to do

Zero malformed-JSON arguments across all 6 calls measured, up to 69 KB. An
absence-of-evidence result, not a proof: recorded as "no work found", not "no
work exists".

### `max_output_tokens` exists, is honoured, and yields `status: "incomplete"`

Corrects an earlier draft of this section, which said "no `generation_config`
field for capping total output tokens was found — only `thinking_level` exists
on it", and concluded from that there was no lever to provoke a starvation
case. **Both halves were wrong.** The first was a failed search reported as a
fact; `openapi.json` lists `max_output_tokens` on `GenerationConfig` alongside
`seed`, `stop_sequences`, `thinking_summaries` and `tool_choice`. Re-measured
2026-08-21:

| `generation_config` | `total_output_tokens` | `status` |
| --- | --- | --- |
| `{thinking_level: low}` | 678 | `completed` |
| `{thinking_level: low, max_output_tokens: 50}` | 46 | **`incomplete`** |
| `{thinking_level: low, max_output_tokens: 2000}` | 696 | `completed` |

So the cap is real and enforced, and truncation is reported as
`status: "incomplete"`. That is the Gemini spelling of OpenAI-format's
`finish_reason: "length"`, and it makes `IsReasoningStarved` implementable on
the same terms DeepSeek uses (`FinishLength` with empty content) rather than
the no-op an earlier draft called for.

The general lesson is worth keeping: a measurement that something could not be
*found* is not evidence it does not *exist*, and must not be recorded as
though it were. Where a probe comes up empty and `openapi.json` documents the
field, the contradiction is unresolved, not settled.

### `status` never says `"requires_action"` — but it does say `"incomplete"`

Contradicts GEMINI-INTEGRATION.md §3's table, which lists Gemini's `status` —
`completed`, `requires_action`, … — as the analogue of `finish_reason`. Every
measurement here, including turns that ended on a pending `function_call` step
with no `function_result` yet supplied, reported `status: "completed"`. **The
signal that a tool call is pending is the presence of a `function_call`-typed
step, not the status field.** A client must scan returned steps for an
unanswered `function_call` rather than branch on `status`.

An earlier draft of this section generalised that to "`status` is always
`completed`". That is too strong, and it was an artefact of never sending
`max_output_tokens`: with a cap in place the interaction returns
`status: "incomplete"` (see above). So `status` carries exactly one piece of
information this harness needs — whether output was truncated — and carries no
information about pending tool calls.

### Context window: at least 1,000,011 input tokens, ceiling still unknown

A single request of roughly 4.5M characters of filler text plus a one-line
question was accepted and answered normally:

```json
"usage": {"total_tokens":1000337,"total_input_tokens":1000011,"total_output_tokens":3,"total_thought_tokens":323, "..."}
```

This only establishes a lower bound. Finding the actual ceiling means sending
requests large enough to be rejected, and the cost scales with how close you
get — bisecting it was judged not worth the spend for this phase.
`gemini-3.7-flash` accepts at least 1,000,011 input tokens; whether the true
limit is exactly 1M or larger is still open. No output-length ceiling was
found either, for the same reason as the section above: there is no
token-capping request field to push against.

### Image in a `function_result` — accepted and actually attended to

A `function_result` whose `result` is
`[{"type":"image","mime_type":"image/png","data":"<base64>"}]`, with no
accompanying text block, is accepted (200), and the model correctly describes
the image rather than ignoring it — tested with a solid-red 32×32 PNG, answer
came back "solid **red** (RGB: #FF0000)". Confirms
GEMINI-INTEGRATION.md §5.3's "function results are multimodal" claim and the
concrete shape it names.

Cost note, not decision-relevant to this phase: usage on that call broke out
`input_tokens_by_modality` as `[{"modality":"image","tokens":1089}]` for a
32×32 image — clearly a fixed per-image floor rather than anything
proportional to pixel count at this size. An earlier attempt with a
degenerate 1×1 pixel image got the model's colour guess wrong, suggesting it
doesn't attend well to images that small; not investigated further.

### Every image costs ~1,120 tokens, whatever its size — the default is `high`

Measured 2026-08-21 against prod session
`sess-35da6920ca8e5ca590acb3a46341c924`, a Blender modelling run that read 34
full-size renders into context over 107 sub-turns. Isolating the sub-turns
whose only new tool result was image data (under 400 bytes of accompanying
text), the prompt-token delta per image was:

| images in the turn | windows | tokens per image |
| --- | --- | --- |
| 1 | 21 | 1,112 – 1,313 |
| 2 | 2 | 1,110, 1,126 |
| 3 | 2 | 1,107, 1,108 |
| 4 | 1 | 1,105 |

25 independent windows, every one landing between 1,105 and 1,134 once the
single 1,313 outlier (which carried the turn's own step overhead) is set
aside. That is the 1,120-token ceiling
`docs/gemini-3.5-flash-ui-review-prompting.md` gives for `resolution: "high"`,
which settles what the unset default resolves to: **`high`**. The harness
sends no `resolution` on the agentic path (`intent.go`,
`contentBlocksFromWire` sets only `mime_type` and `data`), so this is the
default's behaviour, not a setting of ours.

This closes the question the `function_result` image note above left open. It
recorded 1,089 tokens for a 32×32 PNG and called that "clearly a fixed
per-image floor rather than anything proportional to pixel count **at this
size**". It is not a floor and the size caveat is unnecessary: a
1024-plus-pixel Blender render costs the same ~1,120 as a 32×32 one. Image
resolution is a **cap**, the default sits at that cap, and pixel count does
not enter into it below the cap.

The consequence for cost is worth stating plainly, because it is larger than
it looks. In that session images were **24.3% of the final 154K prompt** and
**20.2% of all 8.46M prompt tokens billed across the run** — an image, once
read, is resent on every later sub-turn. Almost all of those resends are
cache hits, so the money is smaller than the token share; the context
pressure is not. Dropping every image to `medium` would have saved ~841K
prompt tokens (10.0% of the run), and `low` ~1.27M (15.1%) — but this was a
visual-verification workload, where the renders are the thing being judged,
and `high` is the documented recommendation for exactly that. No change is
implied for image analysis. What is worth having is the *lever*: the guidance
for several images in one call is `high` on the one under scrutiny and
`medium`/`low` on the rest, and superseded renders that stay in context
forever at full cap are the obvious candidates.

### The Go SDK still does not expose the Interactions API

Checked `google.golang.org/genai`'s package documentation on pkg.go.dev
directly, rather than relying on the README/example absence
GEMINI-INTEGRATION.md §2 originally flagged as "medium confidence." Found
exactly one export with "Interaction" in its name — `InteractionStatus`, a
bare type with no accompanying service — and no `Interactions` client, no
`CreateInteraction` or equivalent method, no path from the SDK to
`/v1beta/interactions` at all. The plan's assumption is confirmed at higher
confidence than a documentation absence: there is no Interactions-shaped
surface anywhere in the SDK's exports, only a stray status enum. The
hand-rolled-client decision (GEMINI-INTEGRATION.md §2) stands confirmed, not
just presumed.

### Smaller findings

**A `thought` step always precedes the turn's action, even at
`thinking_level: "low"`.** Every measurement here — tool call, plain answer,
or bypass replay — began with exactly one `thought` step before whichever
`function_call` or `model_output` step(s) followed. `thinking_level: "low"`
shortens it, not removes it.

**`interaction.status_update` fires once**, immediately after
`interaction.created`, on every stream in this phase, including ones with
tools. Confirms `internal/gemini/stream.go`'s existing comment that this
undocumented frame is real, now also under tool use.

## Implicit caching works under `store: false`, after a warm-up

Measured 2026-08-21. An earlier draft of this section concluded the cache
"never hit with `store: false`" and inferred caching was tied to
`previous_interaction_id`. **That was wrong, and it was an artefact of prompt
size** — every request in that first pass was a few hundred tokens, far below
any cache floor. Re-measured with a prefix the size a real coding session
carries, the cache hits hard:

| request | `total_input_tokens` | `total_cached_tokens` | hit rate |
| --- | --- | --- | --- |
| 1 (cold) | 102,911 | 0 | 0% |
| 2 (identical prefix) | 102,911 | 0 | 0% |
| 3 (identical prefix) | 102,911 | 98,269 | 95.5% |

A ~103K-token `system_instruction` held byte-identical, `store: false`, no
`previous_interaction_id`, ~4s between requests, `thinking_level: "low"`.

Two things matter here. **Stateless replay does not forfeit context caching** —
which removes the main cost objection to GEMINI-INTEGRATION.md §5.3's
stateless decision. The vendored docs say so directly: "Implicit caching is
supported in both stateful and stateless modes"
(`third_party/gemini-docs/interactions-overview.md`), with
`previous_interaction_id` only making it easier to utilise, not a precondition.

And **the cache warms asynchronously over more than one request.** Request 2
was byte-identical to request 1 and still missed completely. Anything that
measures Gemini cache behaviour — `CacheSlack` in Phase 8 above all — must
discard the first two requests, or it will measure the warm-up and call it the
steady state. This is unlike DeepSeek, where the second request of an
identical prefix already hits.

The minimum prefix size is **4,096 input tokens** for `gemini-3.7-flash`
(2,048 for Gemini 2.5), documented at
<https://ai.google.dev/gemini-api/docs/caching> — which also resolves the TTL
question by omission: Google publishes a TTL for *explicit* caching only, and
names none for implicit. Measured to match: a 75-second gap between two
identical requests still hit 69,139 tokens, an order of magnitude longer than
anything a live session leaves between sub-turns.

**The warm-up is not reliably two requests** — Phase 8's live sessions (below)
took five to six.
This section's earlier three-request measurement used a byte-identical
prefix resent with nothing else changing; a live, incrementally-growing
conversation warms slower.

**And it can be much slower than five or six.** Prod session
`sess-35da6920ca8e5ca590acb3a46341c924` took **14 sub-turns** to see its first
cache hit, missing completely on prompts of 10,247 to 16,868 tokens — every
one of them well clear of the 4,096 floor — before turn 15 hit 16,058. From
there it tracked normally, the steady state running 2K–6K uncached against
150K prompts, and the run finished at 91.5% overall. So the rule for anything
measuring Gemini cache behaviour is not "discard the first two requests" but
"discard the warm-up, and do not assume you know how long it is". The cost is
bounded and small — those 14 turns were the cheapest in the session precisely
because the context had not grown yet — which is why this is a measurement
caveat rather than a problem to fix.

## Phase 8 — end to end on the dev stack

Measured 2026-08-21 against the dev stack (`http://127.0.0.1:8080`), two
substantial live sessions plus one throwaway compaction probe, all on
`gemini-3.7-flash`, `readonly` permission mode, real repository exploration
tasks (Read/Grep/Glob/List/Task* against this repository, no writes). Session
ids: `sess-8e5330769a57f99bb4117bf78d506249` (20 sub-turns, hit
`max_sub_turns` before calling `Complete`), `sess-17c66d71b177061881e09ca3361fa70c`
(19 sub-turns, completed normally — 36 tool calls, `$0.28`), and a third,
`sess-e39f8eb8a8197bd2ca8100e9b97794b0` and its six compaction children
(below). `internal/httplog`'s capture for all of them was read back with
`trace.py --dev`, confirmed to hold every Gemini request and response with
`X-Goog-Api-Key` redacted — the third of §2's three reasons for hand-rolling
the client (GEMINI-INTEGRATION.md §2) is now verified end to end rather than
resting on the unit test.

### `CacheSlack` — measured

Method, from `docs/CACHE.md`: discard warm-up, then take the largest
`actual miss − expected miss` a healthy sub-turn shows. `usage` events off
both sessions (`prompt_cache_hit_tokens`, `prompt_cache_miss_tokens`,
`expected_miss_tokens`, `churn_point_index` — the harness's own churn
diagnostic computed these live, on the provisional 8192 already in place):

| Session | Sub-turns | Cold warm-up | Steady-state max over-prediction |
| --- | --- | --- | --- |
| `…8e53…` | 20 | 1–5 (first hit at sub-turn 6) | 5,990 tokens (sub-turn 8) |
| `…17c6…` | 19 | 1–3 (first hit at sub-turn 4) | 4,767 tokens (sub-turn 17), excluding two complete misses below |

**The warm-up took longer than this document's earlier isolated measurement
implied.** That measurement was a byte-identical prefix resent three times
with nothing else changing, and hit on the third request — hence the
"discard the first two" guidance GEMINI-INTEGRATION.md §5.3 and §7 Phase 8
both carried forward. Neither live session matched it: `…8e53…` took five
complete misses before its first hit, `…17c6…` took three. The difference is
plausibly that a live session's head (system prompt + tools) is larger and
its cacheable prefix keeps growing sub-turn to sub-turn, unlike a resent,
static probe — untested which factor matters, since both differ from the
Phase 2 setup at once.

**A second, more surprising behaviour: the cache also went completely cold
mid-session, twice, on a request proven byte-identical to its predecessor.**
`…17c6…` sub-turns 7 and 13 both reported `prompt_cache_hit_tokens: 0` after
several consecutive hot sub-turns (20,225 and 69,144 tokens hit the sub-turn
immediately before each). This was checked, not assumed: the captured
request bodies for sub-turns 6/7 and 12/13
(`trace.py --dev …17c6… --seq N --field req_body`) were compared
programmatically — `system_instruction`, `tools`, and every `input` step
`Ν`'s request shares with `Ν+1` are byte-identical JSON in both cases, with
`Ν+1` a pure append of `Ν`. There is no harness-side cause: no reordering, no
re-rendering, nothing the churn diagnostic's own hash comparison would have
been wrong to call out had it looked (its `ChurnPointIndex` on those two
sub-turns pointed past the end of the shared prefix, which is the
diagnostic's honest way of saying "nothing actually diverged, the provider
still missed everything").

This was first recorded here as an unexplained anomaly. It is not one — see
"The mid-session cache miss is documented behaviour" below, which supersedes
that reading.

**Conclusion: `CacheSlack` stays 8192, now for a measured reason rather than
a guessed one.** The observed steady-state ceiling (5,990) clears with
headroom; the code comment on `internal/gemini/client.go`'s `CacheSlack`
carries the number and the two caveats above. The load-bearing caveat for
whoever reads a live churn report: a Gemini `Churned: true` is not the same
claim a DeepSeek one is. DeepSeek's 127-token bound means it reliably names a
harness bug. Gemini's can also mean the provider's cache went cold on its
own, and no `CacheSlack` value changes that — the miss in both observed
cases ran into the tens of thousands of tokens, nowhere near a slack a
churn-tolerant constant could plausibly absorb without also hiding a real
prefix bug.

### The mid-session cache miss is documented behaviour, not an anomaly

Investigated 2026-08-21, after the section above recorded it as unexplained.
Two findings, and the first settles it.

**Google documents implicit caching as best-effort with no guarantee.**
Verbatim, from
<https://ai.google.dev/gemini-api/docs/generate-content/caching>:

> Implicit caching (automatically enabled on Gemini 2.5 and newer models, **no
> cost saving guarantee**) — Explicit caching (can be manually enabled on most
> models, **cost saving guarantee**)

The Interactions-scoped page (<https://ai.google.dev/gemini-api/docs/caching>)
says only "We automatically pass on cost savings **if** your request hits
caches", and advises how to "**increase the chance** of an implicit cache
hit". No TTL, no eviction policy and no re-keying behaviour is documented for
implicit caching anywhere; those concepts exist only for explicit caching.

So a complete miss on a byte-identical prefix is the API behaving as
specified. There is no mechanism to find.

**A byte-identical replay hit at both miss points.** The strongest evidence,
and it is a controlled result rather than an inference from absence: the
literal captured `req_body` bytes of all 19 requests from `…17c6…` were
resent in order against the live API.

| sub-turn | original `total_cached_tokens` | replay |
| --- | --- | --- |
| 7 | 0 | 28,344 |
| 13 | 0 | 69,139 |

Identical content, identical ordering, comparable cadence — and both known
misses hit cleanly. The miss is therefore **not a deterministic function of
prefix content, prefix size, or growth pattern**: the same prefix at the same
position in the same sequence can go either way. That is exactly what "no cost
saving guarantee" predicts.

Everything else tested came back negative and is recorded so nobody repeats
it: no correlation with idle time (15–360 ms throughout, both misses inside
the same range as their healthy neighbours), no common content in the
preceding sub-turn (no images, no outsized tool result, no compaction
boundary), and no fixed size threshold (the two misses sit at 35,696 and
75,039 input tokens, differing by more than 2×). Backend load or routing
cannot be ruled out — no response header exposes backend identity, region or
cache generation — but nothing in the trace supports it either.

**Clustering matters more than the rate.** Both occurrences are in one
session; its 20-sub-turn sibling had none. "Two in 39 sub-turns" is arithmetic
rather than a rate — treat this as "it happens occasionally and clusters
unpredictably", not as 5%.

**Cost.** Using the churn detector's own arithmetic to price what each miss
should have cost had it hit: ≈$0.020 and ≈$0.050 excess, ≈$0.07 against that
session's $0.28 total — about a quarter of its spend in two sub-turns. Small
in absolute terms, and unbounded in principle, since the excess scales with
prefix size.

**The only documented mitigation is unavailable to us, and for reasons already
settled.** Explicit caching carries the guarantee implicit caching lacks, but
"The Interactions API only supports implicit caching. Explicit caching … is
not supported in the Interactions API. To use explicit caching, switch to the
generateContent API." Leaving Interactions is what
[GEMINI-INTEGRATION.md](GEMINI-INTEGRATION.md) §6 already declined on
independent grounds — event-log authority, resume after restart, and
compaction rewriting history. So this finding surfaces no new trade-off; it
re-prices one already taken.

### Compaction — confirmed working, and the plan's "sharp edge" does not apply

**Reading `internal/session/compact.go` first changes the question.** The
plan (GEMINI-INTEGRATION.md §5.2, §6) worried about replaying a synthetic
`thought` step with no real signature after compaction, and about whether
either bypass value (`context_engineering_is_the_way_to_go` /
`skip_thought_signature_validator`) would be accepted in its place. That
scenario does not arise: this harness's compaction is a **session
boundary**, not a history edit. `compact()` folds the retiring session,
summarises it with the configured flash model (`deepseek-v4-flash` by
default — Gemini's own `CreateChatCompletion` is not on this path unless an
operator points `run.default_flash_model` at `gemini-3.7-flash`), and starts
a brand-new session whose *system prompt* carries the summary
(`RenderCompactionSummarySystemPromptFor`) and whose opening message is
`RenderCompactionOpeningMessage` — "Continue the task described in the
system prompt's summary." No prior `thought`, `function_call`, or
`function_result` step is ever replayed into the new session's history, so
`fold.Fold` never has a signature-less thought to reconstruct and neither
bypass value is ever needed. This holds for every provider, not just
Gemini; §5.2's "sharp edge" was written against a design (message-array
replay across the boundary) this harness does not have.

Confirmed live rather than only by reading the code: `run.compaction_threshold`
was set to its allowed minimum (1024 tokens) on the dev stack for a few
minutes — verified first that the only two `running` sessions were the
known-stale rows from 2026-08-14 with no workspace lease, so nothing live was
disturbed — and a small `gemini-3.7-flash` session was published. Every
sub-turn's prompt already exceeds 1024 tokens (system prompt + tool array
alone), so the session compacted after **every** sub-turn: six compactions
in a row, `sess-e39f8eb8a8197bd2ca8100e9b97794b0` →
`…bdb88b25…` → `…a884d551…` → `…a69025e8…` → `…620c922f…` → `…700f2c38…` →
`…dfcee15b…` (`max_sub_turns`), all within one `harness publish -wait` call.
Every one of the six completed compactions made exactly one Gemini call
(200) and one DeepSeek call for the summary (200), with no error, no 400,
and no wire-shape failure at any point in the chain — confirmed both from
the session events (no `error` events, `status: "compacted"` on every
non-terminal row) and from `trace.py --dev` against each child session's
capture. `run.compaction_threshold` was unset again immediately after.

The setting was pushed to an extreme deliberately to make compaction cheap
to trigger repeatedly rather than to represent realistic behaviour — real
sessions compact once, at 768K tokens, not every sub-turn — but the
mechanism under test (fold → summarise → fork → continue) is identical
either way, and six clean repetitions is stronger evidence than one.

One thing this surfaced that is worth recording precisely, unrelated to the
signature question: **`gemini-3.7-flash` has no per-model entry in
`settings.RunBudgetKeysForModel`**, so both `run.max_sub_turns` and
`run.compaction_threshold` fall back to the DeepSeek-shaped global defaults
(400 sub-turns, 768K tokens) for Gemini sessions, the same way an unknown
model does. GEMINI-INTEGRATION.md §7 Phase 5 (not Phase 7 — the plan
document's own phase numbering drifted here) recorded the reason in its
commit message rather than in code or in this document: "K3 got its own
ceilings because its rates are 7-17x DeepSeek's; Gemini 3.7 Flash sits at or
below DeepSeek Pro's standard tier, so the global defaults already tolerate
it." That is a considered decision, not an oversight — §7 Phase 7's plan
text still lists "run budgets for the model" as in scope for that phase, but
Phase 5 had already decided against one two phases earlier. The plan text is
now corrected in place rather than left implying the work is outstanding.

## Still untested — Gemini

Whether `arguments_delta` ever fragments on a payload larger than ~69 KB.
Whether the exact input-token ceiling sits at 1M or higher. Any output-token
ceiling — no request field was found to probe for one. Whether
`previous_interaction_id` changes any of the above (deliberately unused,
per GEMINI-INTEGRATION.md §5.3). Whether a `function_result` can usefully mix
multiple images, or an image alongside `thought_signature` replay noise from
an *unrelated* turn. Pro variant behaviour — every measurement here used
`gemini-3.7-flash` only. How *often* the mid-session complete cache miss
recurs over a longer session — its cause is settled (documented best-effort
behaviour, see above) but two occurrences clustered in one session is not a
rate, and a real one needs organic traffic rather than more synthetic
probing, since replay is demonstrably unreliable at reproducing it. Whether a
real 768K-token compaction (rather than Phase 8's forced-every-sub-turn
stress test) behaves identically.

## DeepSeek Responses API

Measured 2026-09-10 against `https://api.deepseek.com/responses`, driving
`internal/deepseek`'s own `ResponsesClient` rather than hand-built JSON, so
what these establish is the shape the harness actually sends
([`DEEPSEEK-RESPONSES.md`](DEEPSEEK-RESPONSES.md)). Four requests, all HTTP
200.

**The whole message shape is accepted, reasoning item included.** One
request carried the arrangement every sub-turn after the first sends:
`instructions`, a user `message` item, a `reasoning` item, a `function_call`,
its `function_call_output`, and a `tools` array. The model answered from the
tool's result — "DONE", as the prompt asked once it had the listing. That is
the load-bearing measurement: the `reasoning` item is documented as required
in later turns when a request carries tools, and this is the shape of it
DeepSeek takes.

**A tool call assembles across its frames.** A first turn with the tool
forced returned `finish_reason: tool_calls` and one call —
`call_00_ET_zRS5z0M9YHbgLFwrqV664566`, name `List`, arguments `{"path": "."}`
— assembled from a `response.output_item.added` naming it and two
`response.function_call_arguments.delta` frames, keyed only by
`output_index`. Arguments were valid JSON.

**An image inside `function_call_output` is read.** On
`deepseek-v4-flash-vision-exp`, a 64×64 solid `rgb(0,153,102)` PNG as an
`input_image` part in a tool call's `output`, with the model asked to name
the colour: *"The image is filled with a single colour: green (a solid
sea-green / teal shade)."* This is the shape
[`DEEPSEEK-VISION.md`](DEEPSEEK-VISION.md) §2 could only get from an
undocumented allowance on Chat Completions.

**Prefix caching works here.** The non-streaming probe reported
`input_tokens: 357` with `input_tokens_details.cached_tokens: 256` — a hit on
the shared prefix of a request sent moments earlier by the streaming probe.
The cache the whole request shape is built around survives the surface
change.

**Usage needs no asking.** There is no `stream_options` on this surface and
none is sent; the terminal `response.completed` event carries the whole
response object, `usage` included. Every probe got its usage.

**A whole agentic session runs end to end over the stdio protocol.** Measured
2026-09-10: a parent drove `harness stdio-session` on
`deepseek-v4-flash-vision-exp` with a task needing a tool call, and got
`response.created`, `response.in_progress`, a user `message` item, a
`reasoning` item, `function_call(Read)` with its arguments delta,
`function_call_output`, an assistant `message` streaming its text,
`function_call(Complete)`, and `response.completed` — status `completed`,
reason `complete`, 6,222 input tokens of which 3,072 cached, 208 output, 2
sub-turns, $0.00085. The Responses vocabulary ran from the parent's frame to
the provider's request and back with nothing translating in between.

**`max_output_tokens: 0` is a 400.** "Invalid max_tokens value, the valid
range of max_tokens is [1, 393216]". The field is nullable, and an intent
that names no ceiling must omit it rather than send zero. Found by the run
above, which is the first thing to send a request built from a create body
that named no ceiling — `harness serve` always resolves one from its
settings.

### Still untested — DeepSeek Responses

Whether the prefix stays stable across a
long session the way [`CACHE.md`](CACHE.md) requires — one hit is the
mechanism working, not a rate. Whether `response.incomplete` and
`response.failed` arrive shaped as documented; both are decoded and unit
tested against scripted frames, neither has been seen from the live API.
Whether an image in a *user* message behaves as it does in a tool output.
