# What the failures look like in a transcript

A signature catalogue. It exists because the same underlying fault reads very
differently depending on where you catch it: a tool contract the model cannot
satisfy looks, from inside the transcript, exactly like a model that has stopped
being able to follow instructions. Getting the attribution wrong is the main way
a session review goes wrong — it sends someone to fix the model's prompt when
the bug is in a validator, or the reverse.

Contents:

- [Telling mechanical from behavioural](#telling-mechanical-from-behavioural)
- [Mechanical signatures](#mechanical-signatures)
- [Behavioural signatures](#behavioural-signatures)
- [Known behaviour that is not a finding](#known-behaviour-that-is-not-a-finding)
- [Where to check a mechanical hypothesis](#where-to-check-a-mechanical-hypothesis)

## Telling mechanical from behavioural

One test settles most cases: **could a competent operator have done what the
model was trying to do, in that session, with those tools?**

If yes, and the model failed anyway, it is behavioural. If no — the tool refused
a well-formed call, the result came back empty when the file was not, the same
input produced a different answer each time — it is mechanical, and the model's
struggle downstream of it is a *consequence*, not a second finding.

That distinction matters for the report because the two have different owners.
A mechanical failure is a bug someone fixes in Go or in a config. A behavioural
one is a prompt, a tool description, or a limit of the model. Filing the first
as the second wastes a prompt iteration on something no wording can fix.

When a session contains both — and long ones usually do — say which came first.
A model that was working well for 200 sub-turns and fell apart after a tool
started failing has one root cause, not two findings.

## Mechanical signatures

**A tool rejects a well-formed call, repeatedly.** The model calls a tool, the
result is an error, the model reads the error, adjusts, calls again, gets the
same error. Three or more of these in a row is the `error-loop` flag. This is
the single most expensive failure mode a session can have — it burns whole
sub-turns at full context length — so when you find one, price it: multiply the
failing sub-turns' cost and time from `metrics.md` and put the figure in the
report.

Before deciding whose fault it is, **diff the arguments across the attempts and
check them against the tool's declared parameters**. A model in a retry loop
varies what it thinks is wrong, which is not always what is wrong: attempts that
differ only in size, wording, or formatting while the structure stays constant
are the model repeating one mistake, not the tool refusing valid input. Pull the
arguments out of the raw log and look at the key set, not the prose:

```bash
python3 -c "
import json
for l in open('<workdir>/events.jsonl'):
    e=json.loads(l)
    if e['kind']=='tool_call' and e['payload']['name']=='<Tool>':
        a=e['payload']['arguments']
        try: print(len(a), sorted(json.loads(a).keys()))
        except Exception as x: print(len(a), 'UNPARSEABLE', x)
"
```

If the key set is identical every time and the error names a field, compare that
key set against the tool's schema in `internal/tools/definitions.go`. A field the
schema wants nested that the model is passing at the top level produces an error
message that is completely accurate and completely unhelpful, and the model will
read it as a harness fault. Only once the arguments really do vary in the
dimension the error names — and the error text does not move — is the bug in the
validator rather than the call.

**A result contradicts the workspace.** A Read returns fewer bytes than a
preceding `ls -l` said the file has; a Grep finds nothing where a later Bash
`grep` finds matches; an Edit reports success and a subsequent Read shows the
old text. Look for the model noticing this itself — it usually does, and its
reasoning is often a better bug report than anything you would write.

**Harness-truncated results.** `**harness-truncated**` on a result block means
the model never saw the rest of that output. If the model then acts on a wrong
conclusion, the truncation is the cause and the wrong conclusion is the symptom.
Note how much was cut and whether the important part was in it.

**Output cut at max_tokens.** `finish_reason: length` means the model was cut
off mid-answer. Anything it was about to say is gone, including tool calls it
had not yet emitted. A run that stalls right after one of these is stalling
because its own plan was truncated.

**A retried sub-turn.** `retry attempt N` on a sub-turn means the loop re-sent
the request. Both requests were billed. Occasional retries are the reasoning
starvation path working as designed; a cluster of them is worth naming.

**Cache churn.** `cache churn at messages[i]` means the request's prefix
diverged from the previous request's at message `i`, so everything from there
was re-billed at the miss rate. One churn early in a session is ordinary. Churn
at a low index, or repeated churn, means something is mutating the frozen head —
which `docs/CACHE.md` treats as a serious bug, because the head is shared. Check
the cache hit percentages in `metrics.md` around the flagged sub-turn: a drop
from 99% to something much lower confirms it cost real money.

**Time that is neither model nor tool.** The `metrics.md` header splits wall
clock three ways. The third figure covers workspace preparation, queueing and
any genuine stall. A large one on a session that was otherwise busy is worth
chasing; a large one before the first sub-turn is workspace setup and is
probably fine.

**Permission denials.** A `⛔ DENIED` block names the rule that refused the
call. Judge whether the refusal was correct. A denial that blocks work the
session was explicitly asked to do is a configuration failure — the run was
launched in the wrong mode — even though the deny machinery worked perfectly.

**The run ends without Complete.** `no_tool_calls` means the model stopped
talking without calling the completion tool, so nothing validated its result.
Read the final sub-turns: sometimes the model gave up, sometimes it believed it
had finished, and sometimes it was trying to call Complete and could not.

## Behavioural signatures

These need the transcript. No script finds them.

**Losing the thread over distance.** The mark is a model re-deriving something
it already established — re-reading a file it read 80 sub-turns ago, re-running
a test whose result it already has, restating a constraint it had already
written down. Some re-reading is prudent; the tell is when the reasoning shows
no memory of the earlier pass. Note the sub-turn distance between the two, since
that is the number that says when the drift starts.

**Plan drift.** `session.json` carries the plan (`plan`, the todos array). Read
it before the transcript, then check what actually happened against it. Three
different things live here and the report should separate them: a task marked
complete that the transcript does not support; a task quietly dropped; and work
done that was never in the plan. The third is not automatically bad — but
unplanned work that displaced planned work is.

**Narrowing.** A model that hits an obstacle and responds by shrinking the task
until it fits. Watch for the goal being restated in the reasoning in weaker
terms than the opening message used. The final report often reads as a success
because it is measured against the shrunken goal.

**Verification theatre.** Claiming a thing works without running anything that
could have shown it did not. Check every claim of the form "tests pass" or
"verified" against the tool calls in the same neighbourhood — the tool result is
right there, and either the command ran or it did not.

**A confident wrong diagnosis.** The model's reasoning is the best guide you have
to what it was thinking, and it is *not* evidence about the harness. A model that
has failed at something several times will often name a cause — "the harness is
dropping this field", "the API truncated my output" — with real conviction, and
that sentence tends to end up in the run's final report, where a requester reads
it as fact. Treat every such claim as a hypothesis the review has to settle
against the log and the code, and settle it: a run that ends by asserting a
harness bug that does not exist has done more damage than the failure it was
describing. When the model turns out to be right, say so and cite it; when it is
wrong, the correction is the most valuable line in your report.

**Things it missed.** The hardest and most valuable category, because there is
no marker in the log for a thing that never happened. The reliable way in is
through what the model *did* see: a warning in a tool result it did not comment
on, a file the task implied it should read that never appears in a Read, an
error it acknowledged and then moved past. When you claim something was missed,
cite the sub-turn where the evidence was in front of it.

**Working well.** Report this with the same rigour, not as a courtesy. The
useful instances are specific: recovering from a failure without being told,
noticing an inconsistency in its own earlier work, choosing a cheap check over
an expensive one, stopping to re-read rather than guessing. These are what a
prompt change might destroy, so they belong in the record.

## Known behaviour that is not a finding

Read `docs/OBSERVED.md` before writing the report. It records what has already
been measured against the live API — cache behaviour below 128 tokens, how
usage and tool calls arrive when streaming, what `tool_choice` actually does,
how reasoning spends the token budget, latency at max effort. Reporting one of
those as a newly discovered bug is the most common way this review wastes
someone's time.

`docs/TOOLS.md` is the equivalent for the tool contract: what each tool promises,
what strict mode does per tool, and what the permission modes allow. Check a
suspected tool bug against it before calling the behaviour wrong — sometimes the
tool is doing exactly what it says and the description is what misled the model,
which is a real finding but a different one.

## Where to check a mechanical hypothesis

The transcript shows the symptom. The repo says whether the cause is what you
think. Confirm before reporting:

| Symptom | Where the cause would be |
| --- | --- |
| a tool rejected a valid call | `internal/tools`, and the schema in the session's `tool_schema` |
| Complete rejected a result | the run's `result_schema` in `session.json`, and `internal/tools` |
| cache churn | `internal/cache`, `docs/CACHE.md`, and whatever mutated the messages array |
| a retried sub-turn | the reasoning-starvation path in `internal/session` |
| truncated tool output | the truncation limits in `internal/tools` |
| a denial | the mode's rules in `internal/session`, `docs/TOOLS.md` "Permissions" |
| workspace or clone trouble | `internal/workspace` |
| a run that ended early | `internal/session/runner.go`, the finish reasons |

A hypothesis you could not confirm is still worth reporting — say so plainly and
name what would settle it. A confident wrong attribution is worse than an
honest open question.
