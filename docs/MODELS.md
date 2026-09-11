# Models and thinking settings

DeepSeek renamed its flash model on 2026-09-10: `deepseek-v4-flash` is now a
retired alias for `deepseek-flash` (DeepSeek-V4.1-Flash), which reads images
natively (docs/DEEPSEEK-VISION.md). DeepSeek's API still serves
`deepseek-v4-pro` too, unaffected for now, though DeepSeek has said it will
start routing it to `deepseek-flash` as well from 2026-09-14 — but this
harness has separately stopped routing `deepseek-v4-pro` at all, so
`deepseek-flash` is the only model `internal/provider` and `model.default`
know. Every `deepseek-v4-flash` and `deepseek-v4-pro` below predates both
changes; nothing in this document has been re-measured since.

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
compact at 768K. We ship those defaults except on effort, which is `high` for
the reason measured below.

Their advice on effort is not consistent across harnesses, though. Claude Code
gets `max`, Codex gets `model_reasoning_effort = "high"`, Deep Code documents
`"max"` or `"high"`, and Oh My Pi locks its selector to high and xhigh. Their own
published benchmark runs used max. So max is the more common recommendation and
the benchmarked one, which is why it took a measurement to move off it — see
"Why the main loop defaults to high, not max" below.

## The pro default, settled by the GA release

For most of the preview window this was genuinely contested. `updates.md` for
2026-07-31 announced V4-Flash-0731 with "benchmark results far exceeding
V4-Pro-Preview", and said the same update left the pro endpoint untouched. On
those numbers flash beat the model pro was actually serving, at a third of the
cost and five times the concurrency.

The 2026-08-13 entry ends that. Pro left preview as V4-Pro-0813, and its
published numbers beat flash-0731 on every benchmark the two entries share:

| | flash-0731 | pro-0813 |
| --- | --- | --- |
| Terminal Bench 2.1 | 82.7 | 87.9 |
| NL2Repo | 54.2 | 61.5 |
| Cybergym | 76.7 | 83.3 |
| DeepSWE | 54.4 | 62.7 |
| Toolathlon verified | 70.3 | 74.1 |
| DSBench-FullStack | 68.7 | 71.1 |
| DSBench-Hard | 59.6 | 67.2 |

Both sets are DeepSeek's own, so they establish an ordering rather than an
absolute. Pro stays the default and now has a benchmark argument behind it as
well as DeepSeek's recommendation. Flash remains the cheaper alternative and the
switch is still one click.

## The parameters

OpenAI format, which is the endpoint we use:

- `thinking: {"type": "enabled"}` or `{"type": "disabled"}`. Enabled by default.
- `reasoning_effort: "low" | "high" | "max"`. Defaults to `high`.

`medium` and `xhigh` are accepted for compatibility and remapped. The Anthropic
format spells effort as `output_config.effort`, and the Responses API spells it
as `reasoning.effort` with an extra `none` value that disables thinking. Neither
applies to us.

## The effort mapping, one table for both models

The 2026-08-13 GA release unified it. As of the doc mirror fetched that day:

| Requested | Runs at |
| --- | --- |
| `low` | low |
| `medium` | high |
| `high` | high |
| `xhigh` | high |
| `max` | max |

Two things fall out. `low` now buys a genuinely cheaper run on pro, which it did
not during the preview — the old table collapsed pro's `low` into `high`, and
[OBSERVED.md](OBSERVED.md) measured that collapse at under 1%. And `xhigh`
remains a step down from `max` on both models, so asking for the ceiling means
asking for `max`.

We have not re-measured against pro-0813. The claim above is DeepSeek's, and
OBSERVED.md's numbers were taken against the preview build. Read
`third_party/deepseek-docs/guides/thinking_mode.md` rather than trusting a table
compiled into the binary.

The Codex model catalogue DeepSeek publishes declared `supported_reasoning_levels`
of low, high, and max for pro throughout the preview, before the server honoured
them. Measurement on 2026-08-09 disagreed with it: on one coding task, pro
produced 10635 reasoning tokens at `low` and 10537 at `high` — within 1% —
against 25777 at `max`. The catalogue described the picker, not the server.

The GA table has since caught the server up to the catalogue. Whether pro's
`low` now runs cheaper than its `high` is unmeasured on our side.

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
| `Task` subagent | flash | enabled | max | 20000 |
| `WebFetch` extraction | flash | disabled | — | 4000 |
| Compaction summary | flash | disabled | — | 8000 |
| Session title | flash | disabled | — | 200 |

All of these are configuration, not constants. The main-loop row's model,
effort, and `max_tokens` are settings in the harness's settings table
(`model.default`, `model.flash`, `model.effort`, `run.max_tokens`) — the
numbers in the table are the defaults an operator starts from. The `Task` and
`WebFetch` rows' token budgets are code-level choices that stay as they are.

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
comparison is still open, alongside the flash-versus-pro question it resembles.

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

Settled by observation on 2026-08-09 and re-checked on 2026-08-13. `GET /models`
against the live API returns exactly two entries, `deepseek-v4-flash` and
`deepseek-v4-pro`, both `owned_by: deepseek`. No suffixed variant exists. The
suffix is shim convention; send plain IDs.

The dated build names are not model IDs either. `deepseek-v4-pro-0813` is
rejected with a 400 naming the two accepted IDs; the alias serves the 0813 build.

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
concurrency quota, where DeepSeek applies per-`user_id` limits. A queue-driven
harness can plausibly reach that point; if it ever does, the value is one per
installation and never one per session.

### Concurrency is per-model and account-wide

500 concurrent for pro, 2500 for flash, counted across the account regardless of
which key is used. The per-model semaphore (§4.5) draws from the selected
model's pool, so a session running flash subagents under a pro main loop is
drawing on two pools at once.

Concurrent sessions multiply both. N sessions each running a main-loop
call and some number of subagents is what sets the real draw, so size the pool
against the pro limit and let flash have the headroom.

### Balance

`GET /user/balance` returns `is_available` and totals in CNY or USD. Poll on
startup and again after any 402.

A 402 mid-run means the account is empty. It is reported in those words and not
retried, because retrying an empty balance burns turns and reads as a hang. With
it stops the run rather than burning a turn,
since every one of them will hit the same wall.

## Gemini 3.7 Flash — a second provider

Everything above this section is DeepSeek's own thinking/effort mechanics.
`gemini-3.7-flash` (docs/GEMINI-INTEGRATION.md) is a genuinely different
provider behind the same `session.Client` seam, and enough of the mechanics
above do not carry over that it earns its own section rather than a
footnote.

**`effort` maps onto `thinking_level`, not `reasoning_effort`.** The
`ThinkingLevel*` constants in `internal/gemini` (`minimal`/`low`/`medium`/`high`)
are the whole mapping; there is no DeepSeek-style non-thinking mode to turn
off, and `temperature`/`top_p`/`top_k` must not be sent at all
(`internal/gemini/types.go`).

**It sees images.** `seesImages()` (`internal/session/runner.go`) resolves
through `provider.SeesImages`, one model→capability table rather than a
per-provider switch: it is true for Gemini, for Kimi K3, and for one of
DeepSeek's own two models, `deepseek-flash`
(docs/DEEPSEEK-VISION.md). Wherever it is true, the session sends the
vision-capable tool array, and `Read` and the MCP image path return an image
part rather than a path. This is the practical argument for the model —
`deepseek-v4-pro` and standard Kimi sessions describe a screenshot
secondhand through `Glance`/`Ground`/`Detect`; a Gemini session, or a
`deepseek-flash` one, looks at it directly.

**Cost sits near DeepSeek Pro's standard tier, not its discounted one** —
see the table in GEMINI-INTEGRATION.md §4. The rates are introductory and
double on 2027-01-01, unwatched by `internal/pricing` the same way DeepSeek's
own price-table date is. Gemini's context caching also carries a per-hour
storage charge with no counterpart in the harness's three-rate shape;
a Gemini cost figure here covers cached reads, not cached storage.

**The model list never calls a live endpoint.** DeepSeek and Kimi both have
a `GET /models`-shaped call of their own; the Interactions surface has none
(GEMINI-INTEGRATION.md §2). None of the three reaches this endpoint's list,
though — it always serves the static provider table
(`internal/provider.KnownModels()`) instead, the same one that validates a
create body and routes a run to its client, so a client's model list
see exactly what the rest of the harness runs on.

**No concurrency ceiling is wired for it.** `ModelLimits` (§ "Concurrency is
per-model and account-wide" above) is keyed by the *configured*
`defaultModel` and `defaultFlashModel` settings, currently DeepSeek's pro and
flash. A Gemini session draws no semaphore at all unless an operator points
one of those two settings at `gemini-3.7-flash` — the harness relies on
Google's own account-level limits rather than applying its own.

**No per-model run budget either, deliberately.** `kimi-k3` has its own
`run.max_sub_turns_kimi_k3` / `run.compaction_threshold_kimi_k3` pair
(`settings.RunBudgetKeysForModel`), sized down from the global defaults
because K3's rates are far above DeepSeek's. Gemini has no entry, so it
inherits the global 400-sub-turn / 768K-token defaults, the same as an
unrecognised model would — decided in Phase 5, on the reasoning that
Gemini's rates sit at or below DeepSeek Pro's standard tier, so the cost
argument that motivated K3's own ceiling does not apply. That reasoning
lives only in the Phase 5 commit message, not in code or in
GEMINI-INTEGRATION.md itself, which is why it is repeated here; see
`docs/OBSERVED.md`, Phase 8.

**The prompt cache warms slower and can drop mid-session.** DeepSeek hits on
a prefix's second request; Gemini took five to six live sub-turns to warm in
Phase 8's measurement, and its implicit cache went completely cold twice in
39 combined sub-turns on a request proven byte-identical to the one before
it — not a harness bug, just something Gemini's cache does on its own. See
`docs/OBSERVED.md`, "CacheSlack — measured", and `docs/CACHE.md` for what
this means for the churn diagnostic.

## Where selection happens

At session creation, from the create body's `model` and `effort` fields, and
fixed for the session's life. There is no mid-session switch:
it is a full cache miss, and the read-only UI has nobody to price that choice
for. A caller wanting a different model sends a different request.

Per-role defaults — subagent model, extraction model — live in config rather
than per request.
