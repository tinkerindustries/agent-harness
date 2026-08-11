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

`low` and `high` land within 1% of each other, so pro still collapses `low` into
`high` exactly as `guides/thinking_mode.md` describes. The change DeepSeek
promised for early August 2026 had not landed as of 2026-08-09.

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
