# Evals

A prompt change is a guess until something measures it. An eval runs a suite
of tasks under two or more prompt variants and compares what the sessions did.

Runs go through the WORK stream like any other work request, so they are
claimed by the same workers, get the same workspaces, and use the same tools.
A result therefore says something about the harness people actually use. It
also means an eval costs real tokens and real minutes.

## Running one

```sh
harness eval variants                       # what this build can compare
harness eval run -suite evals/search.json -variants base,search-first -n 3
harness eval run ... -judge -out report.json
harness eval score -report report.json      # re-score from stored events
```

`-n` is replicates per task per variant. Total runs is `tasks × variants × n`,
and `-concurrency` (default 2) bounds how many are in flight, so an eval does
not fill every worker slot.

Runs are interleaved across variants rather than run arm by arm. A change in
the machine or in the API partway through then hits both arms alike instead of
landing entirely on one.

## Variants

A variant is a set of exact-substring replacements against the shipped prompt,
defined in `internal/promptvariant/variants.go`. Holding replacements rather
than a second copy of the prompt means an edit to the shipped text reaches
every variant, and a variant whose anchor text has disappeared fails loudly
instead of rendering unchanged and being scored as though it had applied.

The package holds no prompt text of its own and imports nothing internal but
the wire vocabulary. That is what lets `internal/queue` reject a misspelled
variant on a work request without the agent loop joining the HTTP server's
dependency set.

`base` renders the shipped prompt byte for byte, asserted by a test. A work
request with no `prompt_variant` gets it, which is every production request.

Two variants in flight means two cached prefixes rather than one. That costs a
second cold prefill and nothing after it: each prefix is an independent unit
and every session's first request misses regardless (`docs/CACHE.md`).

## Reminders

A rule stated once in the system prompt decays as the context grows. Measured
over 85 production sessions:

| Context | Searches | Via the search tools | Edits | Refused for want of a read |
| --- | --- | --- | --- | --- |
| 0–16k | 36 | 41.7% | 2 | 0% |
| 16–32k | 88 | 33.0% | 32 | 0% |
| 32–64k | 371 | 29.6% | 121 | 0.83% |
| 64–128k | 573 | 15.0% | 755 | 1.59% |
| 128k+ | 1185 | 3.5% | 1895 | 1.95% |

Both rules decay monotonically. The association is confounded — a session
orienting in a fresh repository searches differently from one running builds
an hour in — which is what an eval is for: the task is held constant and only
the context length varies.

A reminder re-states a rule further down the conversation. Cadence is in
context tokens rather than sub-turns, because tokens are the variable the
decay is against: one tool call returning a build log adds more context than
thirty small ones. A policy has a floor (`afterTokens`) below which the rule
holds unaided, and an interval (`everyTokens`) the context must grow before
the next reminder.

The reminder is appended at the tail through the same path a steer takes, as a
`steer_applied` event with no source. Nothing earlier in the conversation
moves, so the cached prefix survives and the reminder costs its own tokens
plus the trailing partial block.

Its message role is a policy field rather than a decision. DeepSeek's schema
accepts a system message at any position in `messages`, and whether one
carries further than a user message here is unmeasured — `search-64k` and
`search-64k-system` are otherwise identical, so an eval can answer it.

A variant may name a reminder policy, which makes head and tail one axis:
`search-remind` is the shipped prompt plus reminders, `search-first-remind`
is both.

## What gets scored

Mechanical metrics come from the session's stored events, so they are
deterministic and can be recomputed later — `harness eval score` re-runs them
over a finished report, which means a metric added today applies to a run from
last week.

| Metric | What it is |
| --- | --- |
| `search_via_tool` | share of searches made with Grep or Glob rather than through Bash |
| `searches_total` | searches of any kind, as a check that a variant did not simply search less |
| `tool_error_rate` | share of tool calls that came back an error |
| `read_before_edit_misses` | Edit or Write calls refused because the file had not been read |
| `search_via_tool_decay` | change in search-tool share from the run's first half to its second, split at its own median context size |
| `edit_miss_rate_decay` | the same, for edits refused for want of a read |
| `context_tokens_max` | the largest request the run made |
| `tool_calls`, `sub_turns`, `cost_usd` | what the run cost to get there |

Read a decay beside `context_tokens_max`. A decay of zero on a run that never
passed 20k says nothing about what happens at 200k. The split is at the run's
own median rather than a fixed token figure, so a short run and a long one
both yield a number.

`-judge` adds a model reading the rendered transcript and scoring it 1–5
against the suite's rubric, plus a completed flag. It catches a variant that
improves a counter while doing the work worse — a run that searches beautifully
and answers wrongly. The judge is stochastic and its numbers carry noise the
counters do not, so read the counters first and treat a judge difference
smaller than its spread as no difference. It never sees which variant produced
a transcript.

A metric with nothing to measure is absent rather than zero. A run that never
searched has no search share, and averaging a zero in would report a behaviour
that did not happen.

## Reading the table

Each cell is a mean and the standard error of that mean. The delta column
compares the last variant to the first and marks a difference larger than the
two standard errors combined with `*`.

That mark is a prompt to look, not a p-value. With replicates in the single
digits the spread is usually the story, and the honest response to an
unmarked delta is more replicates rather than a conclusion.

## Suites

A suite is a JSON file under `evals/`, loaded with unknown fields rejected so
a misspelled key fails rather than being dropped in silence. Tasks default to
`readonly`, since a suite measuring how a model looks for things has no reason
to let it write.

`evals/search.json` is the first one. Every task is answerable by searching and
reading, and none needs a shell, so a Bash call to grep is a choice rather than
a necessity — which is what makes `search_via_tool` mean anything on it.

## Why not an online A/B

Splitting production traffic was the alternative. Against the read-before-edit
guard's 1.8% base rate it needs about 77 sessions per arm to catch a halving,
and roughly 360 per arm for a quarter — a fortnight of traffic for the first,
months for the second. An eval buys a controlled comparison on the same day,
at the cost of measuring chosen tasks rather than whatever people happen to be
doing. Both are worth having; this is the one that answers a question in an
afternoon.
