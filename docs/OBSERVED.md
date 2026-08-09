# Observed API behaviour

Measured against the live API on 2026-08-09, `deepseek-v4-flash`, thinking mode
enabled, effort `low`. [VALIDATION.md](VALIDATION.md) records what the docs say;
this file records what the API does. Where they disagree, this file wins.

Server build during these runs:
`fp_a18b46594c_prod0820_fp8_kvcache_20260402`. Re-measure if that changes —
several findings below are properties of the serving configuration, not the
protocol.

Everything here was measured on flash. Pro is untested.

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

So thinking mode supports the two permissive values and rejects the two
coercive ones. Forcing a specific tool is impossible while thinking is on, which
rules out a class of design — no forced `TodoWrite` at plan time, no forced
structured output via a named tool.

Our rule of never sending `tool_choice` remains safe, because `auto` is the
default when tools are present. But `none` is available and legitimate for a
text-only turn.

## Smaller findings

**`prompt_tokens_details.cached_tokens` exists** and mirrors
`prompt_cache_hit_tokens`. Undocumented; the OpenAI-shaped alias of the same
number. Either can be read.

**`delta.content` is explicitly `null`** while reasoning streams, rather than
being absent. The Go struct needs a nullable string, not an empty-string
default, or an early `content == ""` check will fire on every reasoning frame.

**A single tool definition cost about 280 prompt tokens.** A one-line user
message with one small tool schema came to 293 prompt tokens against 9 for the
same message with no tools. Ten tools should land in the 2–3K range, which is
the stable head worth caching.

**Reasoning dominates output even at `low` effort.** A prompt answered with the
literal two-token string `ok` produced 51 completion tokens, 49 of them
reasoning. Budget output cost on reasoning volume, not answer length.

**No keep-alive comments appeared** in any of these runs. They are documented
for queued requests, and nothing here queued. The parser still has to handle
them.

**Latency** was 1.9s for a trivial non-streaming call at low effort.

## Still untested

Pro, at any effort. Whether pro honours `low` yet. Effort changes across a
session and their effect on the cache. Long-context behaviour near 768K.
Keep-alive frames under real queueing. Whether the 128-token block size holds
on pro.
