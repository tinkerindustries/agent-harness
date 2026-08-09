# Models and thinking settings

## What DeepSeek recommends

Their official Claude Code configuration is a recommendation for an agentic
coding harness, which is what we are building. It reads:

```
ANTHROPIC_MODEL=deepseek-v4-pro[1m]
ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4-flash
CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash
CLAUDE_CODE_EFFORT_LEVEL=max
CLAUDE_CODE_AUTO_COMPACT_WINDOW=786432
```

Reduced to settings: pro at max effort for the main loop, flash for subagents,
compact at 768K. Those are the defaults this harness ships with.

Their advice on effort is not consistent across harnesses, though. Claude Code
gets `max`, Codex gets `model_reasoning_effort = "high"`, Deep Code documents
`"max"` or `"high"`, and Oh My Pi locks its selector to high and xhigh. Their own
published benchmark runs used max. Max is both the more common recommendation
and the benchmarked one, so it stays the default.

## The pro default is contested

`updates.md` for 2026-07-31 announces V4-Flash-0731 with "significantly enhanced
agent capabilities, with benchmark results far exceeding V4-Pro-Preview" —
Terminal Bench 2.1 at 82.7, NL2Repo 54.2, DeepSWE 54.4, Toolathlon verified
70.3. The same entry says that update "only upgrades the DeepSeek-V4-Flash API.
The DeepSeek-V4-Pro API and the APP/WEB models are unchanged", and that "the
official release of DeepSeek-V4-Pro will follow soon."

Read plainly: as of 2026-07-31 the pro endpoint still served the preview-era
model, and the current flash beat it on agentic coding benchmarks at roughly a
third of the cost with five times the concurrency.

Against that, the pricing page names pro's version "DeepSeek-V4-Pro" rather than
"-Preview", and the Claude Code configuration still puts pro in the main slot.
Neither is dated, so we cannot tell whether they predate the flash update.

Unresolved from local docs. Pro stays the default because that is what DeepSeek
recommends for a coding agent, but flash is the benchmark-backed alternative and
the switch is one click. Worth measuring on real work rather than settling from
documents.

## The parameters

OpenAI format, which is the endpoint we use:

- `thinking: {"type": "enabled"}` or `{"type": "disabled"}`. Enabled by default.
- `reasoning_effort: "low" | "high" | "max"`. Defaults to `high`.

`medium` and `xhigh` are accepted for compatibility and remapped. The Anthropic
format spells effort as `output_config.effort`, and the Responses API spells it
as `reasoning.effort` with an extra `none` value that disables thinking. Neither
applies to us.

## The effort mapping is not identity

Current as of the doc mirror, fetched 2026-08-09:

| Requested | flash runs at | pro runs at |
| --- | --- | --- |
| `low` | low | high |
| `high` | high | high |
| `xhigh` | high | max |
| `max` | max | max |

Three things fall out of this table.

On pro, `low` buys nothing — it runs as `high`. The only real lever on pro today
is high against max.

On flash, `xhigh` is a step down from `max`, not up. Asking for flash's ceiling
means asking for `max`.

`xhigh` is the only single value that means "max on pro, high on flash". That is
useful if one effort setting is broadcast to both models, and misleading if
someone reads it as a level above high.

DeepSeek says pro gains all three levels in early August 2026. The mirror was
fetched on 2026-08-09 and still carries that as future tense, so the table is
live as of today — but this is precisely the window in which it changes. Read
`vendor/docs/deepseek/guides/thinking_mode.md` rather than trusting a table
compiled into the binary.

The Codex model catalogue DeepSeek publishes muddies this: it declares
`supported_reasoning_levels` of low, high, and max for pro as well as flash.
That file tells a client what to offer in its picker, not what the server
honours.

Measurement settles it in the table's favour. On one coding task, pro produced
10635 reasoning tokens at `low` and 10537 at `high` — within 1% — against 25777
at `max`. Pro still collapses `low` into `high`, so the only real lever there is
high against max, and the change DeepSeek promised for early August 2026 had not
landed as of 2026-08-09.

## Parameters that do nothing under thinking

`temperature`, `top_p`, `presence_penalty`, and `frequency_penalty` are ignored
in thinking mode. They do not error. The two penalties are deprecated outright
regardless of mode.

The UI hides those controls while thinking is enabled. A dial that silently does
nothing is worse than no dial.

One apparent contradiction, resolved. DeepSeek's benchmark note says the V4-Flash
runs used "max effort level, topp=0.95, and temperature=1.0", which reads oddly
next to a claim that both parameters are ignored. The reading that fits is that
those are the fixed internal sampling values, which is precisely why
user-supplied ones have no effect. Nothing to act on, but it explains the
mismatch if you hit it.

## Where non-thinking earns its place

Mechanical work with no judgement in it: session titling, `WebFetch` extraction,
compaction summaries, commit message drafts. Non-thinking is cheaper and returns
sooner.

Keep the mode fixed for the life of a conversation. Mixing thinking and
non-thinking assistant messages inside one message array interacts with the
`reasoning_content` round-trip rule in ways the docs do not specify. This costs
nothing to honour, because all of the work above already runs in its own flash
conversation.

Those thinking-off conversations get a capability the main loop does not: tool
forcing. `tool_choice: "required"` and named-tool forcing are rejected in
thinking mode and accepted without it ([OBSERVED.md](OBSERVED.md)). So side work
that needs structured output should define a tool whose schema is the output
shape and force it, rather than using `response_format: {"type":"json_object"}`.
Forcing yields schema-validated arguments; JSON mode yields only valid JSON, and
its documented failure mode is occasional empty content.

## Per-role defaults

| Role | Model | Thinking | Effort | `max_tokens` |
| --- | --- | --- | --- | --- |
| Main loop | pro | enabled | high | 48000 |
| Main loop, quality-first | pro | enabled | max | 48000 |
| Main loop, cost-conscious | flash | enabled | max | 24000 |
| `Task` subagent | flash | enabled | high | 20000 |
| `WebFetch` extraction | flash | disabled | — | 4000 |
| Compaction summary | flash | disabled | — | 8000 |
| Session title | flash | disabled | — | 200 |

All of these are configuration, not constants.

## Sizing max_tokens

`max_tokens` caps reasoning and content together and reasoning is generated
first, so an undersized budget is consumed entirely by reasoning and the answer
never starts — billed in full, `finish_reason: "length"`, empty `content`
([OBSERVED.md](OBSERVED.md)).

It is a ceiling, not a spend. Raising it costs nothing until something needs the
room, which makes generosity close to free. The only real exposure is a runaway,
and the documented one is JSON mode emitting unending whitespace until it hits
the limit.

Measured on flash at max effort against a 20000 ceiling:

| Prompt | reasoning | answer | total | share of 20000 | wall |
| --- | --- | --- | --- | --- | --- |
| "Say hello." | 21 | 3 | 24 | 0.1% | 1s |
| Reverse a slice, one function | 179 | 59 | 238 | 1.2% | 3s |
| A real SSE reader with error handling | 10844 | 2191 | 13035 | 65% | 117s |
| Hard combinatorics proof | 20000 | 0 | 20000 | 100%, starved | 194s |

Reasoning outweighs the answer roughly five to one on real work, so the budget
is set by reasoning volume rather than expected output length.

Pro is much heavier. The same SSE reader task, pro at max effort, came to 25777
reasoning and 2364 answer — 28141 tokens, against flash's 13035. Pro reasons
about 2.4× more for identical work, which means a 20000 ceiling starves an
ordinary coding task on the model the harness defaults to. That measurement is
why the main loop gets 48000 rather than the 32000 flash alone would justify.

The starvation case is recoverable rather than fatal. On `finish_reason` of
`length` with empty `content`, retry once at double the budget and surface both
the retry and its cost. Bound it at one retry — a second failure means the task
needs decomposing, not a bigger ceiling.

Those wall-clock figures are the other reason the harness always streams. The
pro run above took 583 seconds — nearly ten minutes for a single sub-turn. An
agent turn with eight sub-turns at that rate runs over an hour, and a
non-streaming request of that length is indistinguishable from a hang.

## Why the main loop defaults to high, not max

DeepSeek's Claude Code configuration says `max`. We default to `high`, and the
measurement is the reason. Same task, pro, same 40000 ceiling:

| effort | reasoning | answer | total | wall |
| --- | --- | --- | --- | --- |
| `low` | 10635 | 1762 | 12397 | 260s |
| `high` | 10537 | 1659 | 12196 | 277s |
| `max` | 25777 | 2364 | 28141 | 583s |

`max` costs 2.3× the tokens and 2.1× the wall-clock. On output price alone that
is roughly 2.4c against 1.1c per sub-turn, which is affordable; the latency is
not. An eight-sub-turn agent turn runs about 37 minutes at high and 78 at max.

What the measurement does not cover is quality. `max` may well write better code,
and DeepSeek benchmarked on it. But their own Codex configuration uses
`model_reasoning_effort = "high"`, so their advice is already split, and an hour
per turn is hard to defend for interactive work.

So: `high` by default, `max` as a documented one-line switch for hard problems,
and a real quality comparison on actual tasks as the thing that settles it. That
comparison belongs in phase 2, alongside the flash-versus-pro question it
resembles.

Max effort is expensive in a loop, and the reason is worth stating plainly.
Reasoning tokens bill as output when generated, then bill again as input on
every later sub-turn of the same turn, because reasoning round-trips whenever
`tools` are present (DESIGN.md §3.1). A turn that takes eight sub-turns re-sends
its first reasoning block seven times. A stable prefix means the cache absorbs
nearly all of that, which is why §3.2 matters most at max effort.

## Model selection

### Populate the list from the API

`GET /models` returns the live model IDs. Fetch on startup, cache, and fall back
to a configured list if the call fails. Prices are not in that response and stay
in config (§4.9).

### The `[1m]` suffix

DeepSeek's Claude Code configuration uses `deepseek-v4-pro[1m]`. That form shows
up only in the Anthropic-shim environment variables. The pricing page and the
Chat Completions `model` enum both list plain `deepseek-v4-pro` and
`deepseek-v4-flash`, with 1M context standard on both, and the Codex model
catalogue DeepSeek publishes uses the plain slugs with
`"context_window": 1048576`.

Settled by observation on 2026-08-09. `GET /models` against the live API returns
exactly two entries, `deepseek-v4-flash` and `deepseek-v4-pro`, both
`owned_by: deepseek`. No suffixed variant exists. The suffix is shim convention;
send plain IDs.

### Switching mid-session costs the cache

The prompt cache is per-model — cached entries are tensors computed from a
specific set of weights, so there is nothing for a 284B model to reuse from a
1.6T one. Switching model mid-session makes the next request a full cache miss
across the entire conversation.

That is not a reason to forbid the switch. It is a reason to price it at the
moment of the click, which the harness can do exactly: context size comes from
the last `usage` block and the rate comes from the config price table.

> Switch to flash — 187K tokens will miss cache, about $0.026

Same instinct as the cache hit gauge in §4.9. An invisible cost becomes a
visible one, and the user decides.

Changing `reasoning_effort` mid-session should be free, because effort is a
request parameter rather than prompt content and does not alter the prefix. That
is reasoning about how prefix caching works, not something the docs state. Watch
the hit rate across an effort change and correct the assumption if it moves.

### user_id partitions the cache too

`quick_start/rate_limit.md` lists KVCache isolation among the things `user_id`
controls: it "is used to isolate KVCache for users on your business side for
privacy management."

So `user_id` is a second cache partition key alongside the model. Either omit it
entirely or send one stable value for the life of the installation. A per-session
or per-workspace `user_id` would start every session on a cold cache, which is
the most expensive mistake available in this design and an invisible one.

Omitting it is the default. The field earns its place only under a raised
concurrency quota, where DeepSeek applies per-`user_id` limits, and a single-user
harness never gets there.

### Concurrency is per-model and account-wide

500 concurrent for pro, 2500 for flash, counted across the account regardless of
which key is used. The per-model semaphore (§4.5) draws from the selected
model's pool, so a session running flash subagents under a pro main loop is
drawing on two pools at once.

### Balance

`GET /user/balance` returns `is_available` and totals in CNY or USD. Poll on
startup and again after any 402.

A 402 mid-run means the account is empty. The UI says that in those words and
does not retry, because retrying an empty balance burns turns and reads to the
user as a hang.

## Where selection appears in the UI

Session creation picks model and effort. The session header shows both and
allows changing either, with the cache-miss estimate attached to a model change.
Per-role overrides — subagent model, extraction model — live in settings rather
than in the session header.
