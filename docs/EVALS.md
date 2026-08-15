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

`-model` and `-effort` override what every task runs on, and `-max-sub-turns`
overrides every task's own budget. It applies to all arms at
All three apply to every arm at once. That is the only safe way to change what
a run gets: extending only the arm that keeps running out hands extra budget to
whichever variant is less efficient, hiding the difference the eval exists to
measure, and a comparison across two models is not a comparison of two prompts. When runs do hit
the cap, the table says how many, because their metrics stop where the run
stopped rather than where the work did — raise the budget and run it again
instead of reading those numbers.

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
over 85 production sessions — 81 of them `deepseek-v4-flash`, all under the
prompt as it stood at v0.21.0:

| Context | Searches | Via the search tools | Edits | Refused for want of a read |
| --- | --- | --- | --- | --- |
| 0–16k | 36 | 41.7% | 2 | 0% |
| 16–32k | 88 | 33.0% | 32 | 0% |
| 32–64k | 371 | 29.6% | 121 | 0.83% |
| 64–128k | 573 | 15.0% | 755 | 1.59% |
| 128k+ | 1185 | 3.5% | 1895 | 1.95% |

Both rules decay monotonically there. Whether that is a property of the
window is not settled. The association is confounded — a session orienting in
a fresh repository searches differently from one running builds an hour in.
And the prompt those sessions ran under told the model to "search from Bash
with rg", two lines under "prefer Grep and Glob"; removing the contradiction
moved flash from 12.5% tool use to 99.5%, so the decay may be that
instruction losing to habit rather than the context length. A later run on
the current prompt showed no decay at all, at 83k.

So: watch it. `search_via_tool_decay` and `edit_miss_rate_decay` are on every
report for that reason, and a variant that holds early and not late is worth
telling from one that holds throughout. It may turn out not to be a problem.
The reminder policies below are built and untested against anything that
actually decays.

A reminder re-states a rule further down the conversation, for the case where
decay does turn out to matter. Cadence is in
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
against the suite's rubric, plus a completed flag. It runs in thinking mode
with the models' documented maximum output, so the judge is bounded by the
model rather than by us — scoring a transcript is a judgement, and the
reasoning is where it is made. The ceiling is not a reservation: a request is
billed for what it generates. It catches a variant that
improves a counter while doing the work worse — a run that searches beautifully
and answers wrongly. The judge is stochastic and its numbers carry noise the
counters do not, so read the counters first and treat a judge difference
smaller than its spread as no difference. It never sees which variant produced
a transcript.

The judge defaults to `model.judge`, which is `kimi-k3` unless an operator
changes it, and is overridden with `-judge-model`. Keep it off the model
under test: a model scoring its own transcripts rates work that reasons the
way it does more highly, and the arms of a prompt eval differ precisely in
how the model was told to work. Running sessions on flash with a K3 judge is
the current arrangement — the judge from outside the family, at $15.00/M
output against `deepseek-v4-pro`'s $0.87 (`configs/prices.json`), which is
why `model.judge`'s description says what a verbose verdict costs.

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

## Where a run runs

`harness serve` owns the orchestrator. `harness eval run` posts the spec to
`POST /api/evals` and then follows the stored rows; the verb has one
implementation, the way `harness stop` posts to the stop endpoint rather than
reaching around it (cmd/harness/stop.go).

Two things follow. A run survives the terminal that started it — closing it,
or rebuilding the container under it, no longer strands the run at whatever
member it had reached. And a caller that is not a terminal can start one.

`-detach` prints the run id and exits. Interrupting a non-detached follow also
leaves the run going; it is a reader, not a holder.

One eval at a time. A second start is a 409: two evals interleaving means each
measures a machine the other is loading, which is not a comparison either can
stand behind.

The endpoints, following the conventions in docs/DATA-API.md:

| Endpoint | Guards |
| --- | --- |
| `POST /api/evals` | write guards + bearer token → 202 `{eval_run_id}` |
| `POST /api/evals/{id}/cancel` | write guards + bearer token → 202 |
| `PATCH /api/evals/{id}` | write guards + `If-Match`; closes out a stranded run |
| `DELETE /api/evals/{id}` | write guards + `If-Match`; refuses a running run |

The two POSTs are run control and carry the token, the rule
docs/RUN-CONTROL.md sets for anything that starts or ends a run and spends
money. PATCH and DELETE are row writes and carry the version. `harness serve`
still holds no queue handle: the orchestrator enqueues through the same
one-method publisher seam the browser's start already uses, and
`internal/httpapi` reaches it through the declared `EvalController`.

Cancelling stops publishing further members and ends the ones in flight.
Members already finished keep their scores and the run lands `cancelled` with
a partial comparison, which is a legitimate result over what did finish.

## Watching one

Both eval screens are pushed rather than polled. `GET /api/evals/stream` is
the list and `GET /api/evals/{id}/stream` is one run; each frame is the whole
snapshot, not a delta. A run changes a few times a minute and its detail is a
few kilobytes, so a full snapshot costs nothing worth optimising and takes all
the merge logic out of the browser — along with any chance of the comparison
table disagreeing with the rows above it.

The orchestrator wakes the hub after every write, through an `OnChange`
callback `cmd/harness` wires to `hub.PublishEvalChanged`. That is why
orchestration had to move into `harness serve` before this could work: the hub
and the thing generating the updates are now the same process.

## Where a run is recorded

An eval writes two tables as it goes: `eval_runs` for the run and
`eval_members` for one row per (task, variant, replicate). The members exist
before anything is published, so a run that dies mid-flight still says what it
was going to do, and each member is updated as its session finishes — a report
exists for the part of an eval that has completed while the rest is still
going.

`scores` and `verdict` are stored rather than recomputed on demand. Deleting a
session removes the event log a rescore would read, and a judge verdict is a
model call that cannot be repeated for free; a member whose session is gone
still contributes its numbers to the comparison and loses only the link.

`suite_json` holds the suite as it was loaded. The file under `evals/` changes,
and a run has to keep saying what it actually ran.

The `-out` file is an export of those rows rather than the record itself.

## Why not an online A/B

Splitting production traffic was the alternative. Against the read-before-edit
guard's 1.8% base rate it needs about 77 sessions per arm to catch a halving,
and roughly 360 per arm for a quarter — a fortnight of traffic for the first,
months for the second. An eval buys a controlled comparison on the same day,
at the cost of measuring chosen tasks rather than whatever people happen to be
doing. Both are worth having; this is the one that answers a question in an
afternoon.
