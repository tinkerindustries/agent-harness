# Cache optimisation

Cache-hit input costs 50× less than cache-miss on flash and 120× less on pro.
In a coding harness the whole conversation is re-sent on every sub-turn, so hit
rate is the difference between a cheap session and a ruinous one. It also drives
latency: a hit skips prefill, which at 200K tokens is most of the wait before the
first token appears.

DESIGN.md §3.2 states the invariant. This document is the tactics.

## The mechanism is not what you probably assume

Most prefix caches do longest-match: send a prefix that agrees with a cached one
for 40K tokens and diverges after, and you get 40K cached. DeepSeek does not
work that way.

`guides/kv_cache.md`: "Each cached prefix is an independent, complete unit. A
subsequent request can only hit the cache if it fully matches a cache prefix
unit." A unit is hit only when it is wholly a prefix of the new request. There
is no partial credit against a longer unit.

### The interval is 128 tokens, measured

The docs mention persistence "at fixed token intervals" without naming the
interval. Measured on flash on 2026-08-09 it is 128 tokens, exactly, across five
prefix sizes ([OBSERVED.md](OBSERVED.md)):

    hit = floor(common_prefix_tokens / 128) × 128

The trailing partial block never hits. That is the whole penalty for a
well-behaved request: at most 127 tokens.

This is more forgiving than the docs' Example 2 suggests. That example — `A + B`
then `A + C` missing entirely — only applies when `A` is under 128 tokens. At
the prefix sizes a coding harness works with, the fixed-interval blocks are
dense enough that one prior request is sufficient. A divergent-suffix request
hit 3840 of 3893 tokens after exactly one previous call, with no wait.

### What this does and does not forgive

It forgives the tail. A partial block at the end of the prefix costs under 127
tokens, which is nothing.

It forgives nothing at the head. The formula operates on the length of the
*common* prefix, so divergence early in the request truncates that length
directly. Put a clock at token 50 of the system prompt and the common prefix is
50 tokens, `floor(50/128) × 128` is zero, and the entire conversation misses on
every single request.

Cheap tail, catastrophic head. That asymmetry is the reason the invariants below
are worth enforcing in code rather than by convention.

## Why the agent loop is already well shaped

The loop's rhythm lines up with checkpoint 1 exactly.

Sub-turn N ends with a model output, so a unit is persisted covering
`[system … assistant_N]`. Sub-turn N+1 sends `[system … assistant_N, tool_N]`,
which wholly contains that unit. It hits, and the only uncached tokens are the
tool result.

That is the ideal case, and it is the default case. The work is in not breaking
it.

One caveat from the docs: cache construction takes seconds. A loop that turns
around faster than persistence completes will miss on the immediate next
request. At max effort a model call runs long enough that this should be rare,
but it is a real effect and not a bug.

## The invariants

Breaking any of these costs the whole prefix.

**Freeze the system prompt and tool schema into the session at creation.** Store
the rendered text in the session row, not a template read from the running
binary. Otherwise upgrading the harness silently changes the prefix of every
resumable session, and every resume is cold.

**Never vary the tool array.** Permission modes gate execution, not availability
— all sixteen tools ship on every request in every mode, and a disallowed call
is refused at execution with an error result the model can read. Removing tools
per mode would give each mode its own prefix and make mode switching a cold
start. A work request's `result_schema` is the tempting exception: it belongs
in the opening user message, never in `Complete`'s definition. Evolving the
array *between releases* is different from varying it per request: when
`ReviewScreenshot` gained its `conversation_id` argument, every session shipped
the new array together, so the head changed once, paid once, and stayed shared
— the rule is that no session or request gets a head of its own, not that the
head is frozen forever.

**Never quote a configurable limit in a tool description.** The tool array is
part of the frozen head, so a number that an operator can change — a timeout,
an output cap, an image count — must not appear in a description: it would make
the head vary per installation and change under operators' feet, costing the
whole prompt cache on every change. The model discovers a bound from the tool's
refusal message, which states the actual limit. The `ReviewScreenshot`
description was the last one carrying such numbers and they were removed in the
same change that made the limits settings; check any new description against
this rule.

**Order tool results by `tool_calls` index**, never by completion order.

**Keep volatile content out of the head.** No clock, cwd, git status, or file
listing in the system prompt. Nothing from a work request either — workspace
path, task instructions, and result schema all go in the opening user message.
Environment context is injected once at session start; refreshing it happens
through a tool call the model makes, which appends.

**Keep the churn diagnostic per-session.** Its previous-request hashes are
mutable state, and several sessions run at once. Sharing them across sessions
produces churn reports that name the wrong message.

**Serialise once and retry the same bytes.** A 500 or 503 retry must resend the
identical buffer. Re-serialising risks a changed byte, which both misses and
pollutes common-prefix detection with a near-duplicate.

**Use structs, not maps.** Stable field order, stable output.

**Omit `user_id`, or pin one stable value.** It partitions the cache
(MODELS.md). A per-session value cold-starts every session.

## Active optimisations

### There is no warmup to do

An earlier draft of this document proposed firing two probe requests at startup
to trigger common-prefix detection on `[system + tools]`. Measurement killed it.

The 128-token blocks are persisted by any single request that contains them, so
the first real request of the first session already warms the stable head, and
every session after that hits it. A warmup would buy one request's worth of
benefit, once, ever. Not worth the code.

The corollary is worth keeping though: size the stable head so it is comfortably
over 128 tokens. Sixteen tool schemas keep it in the 2–3K range (an estimate,
not a re-measured figure — the four plan tools replaced the single whole-plan
tool, so the head grew by three schemas' worth of tokens), so this takes care
of itself.

### Concurrency helps here

Sessions run several at a time and every one of them sends the same rendered
system prompt and tool array. The head is persisted by whichever request lands
first and every session after that starts warm on it, so a busy queue is cheaper
per run than an idle one. This only holds while the head is genuinely identical,
which is the reason nothing per-request may appear in it.

Two sessions starting simultaneously from cold both miss, since cache
construction takes seconds. That costs the head once, not once per session.

### Compact into a new prefix, deliberately

Compaction rewrites history and therefore resets the cache. That is unavoidable
at 768K, so the question is only where the summary lands.

Put it in the new session's system prompt. It then sits in the stable head where
it can become a checkpoint, rather than in a user message where it is just more
body text.

### Keep subagents on a shared prefix

Flash subagents run in their own conversations. Give them one shared system
prompt so common-prefix detection persists it after the second subagent runs,
and every subsequent one starts warm.

## Measurement

Hit rate alone is a bad metric. A request with a 200K cached prefix and a 500
token tool result reports 99.8% and tells you nothing, because it would report
that whether or not anything is wrong.

The useful signal is expected miss against actual miss.

The harness knows exactly what it appended since the previous request, so it can
predict the miss precisely: the new content, plus the partial block left over
from the previous prefix, so under 128 tokens of slack. `usage`
`prompt_cache_miss_tokens` reports the truth. When actual exceeds expected by
more than a block, the prefix churned.

The 128-token granularity is what makes this diagnostic sharp. Expected and
actual should agree to within 127 tokens on every healthy sub-turn, so a
disagreement of thousands is unambiguous rather than a judgement call.

That turns into a real diagnostic. Keep a per-message hash of the previous
request's serialised messages array. On a churn detection, compare against the
current one and report the first index that differs. The answer is almost always
a specific message the harness mutated when it should have appended.

Surface three numbers per turn: miss tokens, expected miss tokens, and the
churn-point message index when they disagree. Those are more actionable than a
percentage, and the third one names the bug.

## Deliberate resets

Some resets are correct: compaction at 768K, a model switch the user chose, a
new system prompt after an upgrade. Each is fine as long as it is visible.

Show the cost at the moment it is incurred, the way MODELS.md prices a model
switch. An unexplained cost spike is a support problem; a priced, user-chosen
one is not.
