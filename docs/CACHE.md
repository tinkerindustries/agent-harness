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

Their Example 2 is the one to internalise. First request `A + B`, second request
`A + C`. The second misses **entirely** — not "hits A, misses C". `A + B` is not
a prefix of `A + C`, and no unit for `A` exists yet. Only after that second miss
does the system notice the shared `A` and persist it, so a third request `A + D`
finally hits `A`.

Two consequences worth stating plainly.

Hits are quantised to checkpoints. Divergence forfeits everything back to the
previous checkpoint, not just the bytes after the divergence point.

The first divergence always pays full price. Common-prefix detection is
retrospective — it needs two requests to have already diverged before it
persists their shared head.

### Where checkpoints exist

- At the end of user input and the end of model output, on every request.
- Wherever common-prefix detection has fired.
- At fixed token intervals through long inputs and outputs. The interval is not
  documented, which is why long-context behaviour has to be measured rather than
  predicted.

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
— all ten tools ship on every request in every mode, and a disallowed call is
refused at execution with an error result the model can read. Removing tools per
mode would give each mode its own prefix and make mode switching a cold start.

**Order tool results by `tool_calls` index**, never by completion order.

**Keep volatile content out of the head.** No clock, cwd, git status, or file
listing in the system prompt. Environment context is injected once at session
start; refreshing it happens through a tool call the model makes, which appends.

**Serialise once and retry the same bytes.** A 500 or 503 retry must resend the
identical buffer. Re-serialising risks a changed byte, which both misses and
pollutes common-prefix detection with a near-duplicate.

**Use structs, not maps.** Stable field order, stable output.

**Omit `user_id`, or pin one stable value.** It partitions the cache
(MODELS.md). A per-session value cold-starts every session.

## Active optimisations

### Warm the stable prefix

`[system + tools]` is identical across every session but is never itself a
checkpoint — the end-of-user-input unit includes the first user message, so it
is too specific to reuse.

Common-prefix detection is the way in, and it can be triggered deliberately. Two
startup requests carrying the same system prompt and tool schema but different
trivial user messages make the system persist `[system + tools]` as its own
unit. From then on every session's first request hits it instead of paying full
price.

Price it honestly: the prefix is a few thousand tokens, so the saving is
fractions of a cent per session. The latency saving on first token is the better
argument. Run it once per system-prompt version, not per launch, and skip it if
the two probe requests would cost more than the sessions they serve.

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
predict the miss: roughly the token count of the new content. `usage`
`prompt_cache_miss_tokens` reports the truth. When actual greatly exceeds
expected, the prefix churned.

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
