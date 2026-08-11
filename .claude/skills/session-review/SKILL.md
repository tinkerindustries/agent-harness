---
name: session-review
description: Review one deepseek-harness session end to end from its session id — pull the transcript from the running stack, read every sub-turn, and produce a detailed assessment separating mechanical failures (harness bugs, tool contract problems, configuration mistakes) from where the model struggled, where it did well, what it missed, and where it lost the thread over a long piece of work. Use this whenever someone gives you a session id and asks what happened, how a run went, why a session failed or stalled or cost so much, whether a problem was the harness or the model, or asks to review, assess, audit, post-mortem, or dig into a session, run, or transcript. Reach for it even when they only paste a bare sess-... id, and when they ask about a run they watched go wrong.
---

# Reviewing a harness session

The value of this review is attribution. Anyone can see that a session went
badly; the question worth answering is whether the harness let the model down or
the model let itself down, because those have different owners and different
fixes. Getting that wrong is worse than not reviewing at all — it sends someone
to rewrite a prompt when the bug is in a validator.

So the standard throughout is evidence. Every claim in the report cites the
sub-turn it came from. A finding you cannot cite is a hypothesis, and the report
says so.

## Fetch first

```bash
python3 .claude/skills/session-review/scripts/fetch_session.py <session-id> --out <workdir>
```

Use a temp directory for `<workdir>` — the fetch output is bulky and disposable.
The script defaults to production (`http://127.0.0.1:8180`); pass
`--base-url http://127.0.0.1:8080` for the dev stack, or a worktree's port. If
it cannot connect, check the stack is up before assuming the id is wrong; if the
id is genuinely unknown, `curl -s http://127.0.0.1:8180/api/sessions | python3 -m json.tool`
lists what is there.

It writes `session.json` and `events.jsonl` verbatim, a `metrics.md`, and
`transcript/01.md`…`NN.md`. The transcript is split at sub-turn boundaries, so
part boundaries never cut a turn in half.

Long tool results and reasoning blocks are trimmed in the middle, with the
omitted byte count in place. When a trimmed block turns out to matter, re-render
that turn alone rather than re-fetching everything:

```bash
python3 .claude/skills/session-review/scripts/fetch_session.py <session-id> --turn 214 --full
```

## Orient before reading

Read `metrics.md` and `session.json` first, in that order, and write down what
you expect *before* you open the transcript. This is worth the two minutes: a
reviewer who reads 300 sub-turns with no prior forms their impression from
whatever they happened to notice, and the last thing read wins.

From `metrics.md`: the flagged anomalies, the cost and time split, the tool
table, and the shape of the per-sub-turn table. Every flag is a *candidate* — the
script recognises patterns, it does not know whether they were problems — so
treat each as a question to answer from the transcript.

From `session.json`: the model, effort, permission mode, and the `plan` array,
which is the model's own statement of what it set out to do. Read the plan
carefully. It is the yardstick for the drift question later, and it is much
harder to reconstruct after you have absorbed the transcript's own account of
events.

Then read `references/failure-modes.md`. It is the signature catalogue: what
each kind of failure looks like from inside a transcript, and how to tell a
mechanical one from a behavioural one. Read it now rather than when you hit
something confusing, because the mechanical signatures are easy to miss if you
are not already carrying them.

## Read every part, taking notes as you go

Read `transcript/01.md` through the last part, in order. Do not skim to the
flagged sub-turns — the flags are the things a script can see, and most of what
this review is for is the things it cannot: a model quietly re-deriving what it
already knew, a plan item abandoned without comment, a warning in a tool result
that went unremarked.

**Keep a notes file and append to it after each part.** Write it to
`<workdir>/notes.md`. This is not bookkeeping. A long session's transcript will
not fit comfortably in your head, and the early parts fade exactly as the later
ones arrive — which is, with some irony, the same failure you are reviewing the
model for. Notes are what make the final assessment reflect the whole session
rather than its last third.

After each part, append entries in this shape, one line each:

```
[part 07, sub-turn 214] MECH? Complete rejected a result matching result_schema — 3rd time
[part 07, sub-turn 219] GOOD  noticed its own earlier edit had not applied, re-read before proceeding
[part 08, sub-turn 245] MISS  test output warned about the deprecated flag, model moved past it
[part 08, sub-turn 251] DRIFT re-read fold.ts, already read at sub-turn 96
```

Four tags: `MECH?` for a suspected mechanical failure, `GOOD` for something done
well, `MISS` for something in front of it that it did not act on, `DRIFT` for
losing the thread. The question mark on `MECH?` is deliberate — attribution
happens after the read, when you can see the whole arc, not in the moment.

Note, as you pass them, the sub-turns where the model's own reasoning diagnoses
something — and tag those `MECH?` too, however confident the model sounds. Its
reasoning is the best guide to what it was thinking and no evidence at all about
the harness. A model that has failed several times often names a cause with
conviction, and that sentence usually survives into the run's final report where
a requester reads it as fact. Checking those claims is among the most valuable
things this review does, in both directions.

## Then attribute, then confirm

With the read done, work through the `MECH?` notes. For each one, ask the
question `references/failure-modes.md` opens with: could a competent operator
have done that, with those tools, in that session? Where the answer points at
the harness, confirm it against the code — the table at the end of that
reference says where each kind of cause lives. A hypothesis you could not
confirm still goes in the report, marked as one, with a note on what would
settle it.

Then look for the arc. Long sessions usually have one thing that went wrong
first and a trail of consequences, and a report that lists twelve findings when
there was one cause and eleven symptoms is harder to act on than one that says
so. Where a mechanical failure and a stretch of model struggle overlap, say
which came first.

## The report

Write it to `docs/reviews/<session-id>.md` (create the directory if needed), then
give a short verdict in chat with the path — a few sentences on what the session
was, how it went, and the single most actionable finding. Not the whole report:
the file is the report.

Use this structure:

```markdown
# Session review — <session-id>

<one paragraph: what the run was asked to do, what it produced, and the
honest verdict on how it went>

| | |
| --- | --- |
| Model / effort / mode | |
| Outcome | |
| Sub-turns / cost / wall clock | |
| Verdict | |

## Mechanical failures

<Each with: what happened, the sub-turns it happened at, the evidence, whether
it is confirmed against the code or still a hypothesis, and what it cost the
run in sub-turns and dollars. Ordered by cost to the run.>

## Where the model struggled

## Where the model did well

## What it missed

## Where it lost the thread

<Only if the session is long enough for this to mean anything. Cite the sub-turn
distance between where something was established and where it was re-derived.>

## Ranked fixes

<Numbered, most impactful first. Each names the file or config it touches and
what it would have changed about this run. Mechanical fixes and prompt or tool
description changes both belong here — labelled, since they have different
owners.>
```

Two habits keep this report honest. **Quantify what you can**: "the Complete
loop burned 11 sub-turns and 2 cents, 7% of the run's cost" is actionable in a
way that "the Complete tool was problematic" is not, and `metrics.md` has the
numbers. **Rank by cost to the run, not by how interesting the bug is** — a
subtle attribution puzzle that cost thirty seconds ranks below a boring
misconfiguration that cost ten minutes.

Where the session was simply fine, say so plainly and keep the report short.
Padding a clean session with minor observations trains the reader to skim, and
then the one that matters gets skimmed too.

## If the session is still running

`status: running` in `session.json` means the log is a snapshot of an unfinished
run. Review it if asked — the flags and the read work the same — but say in the
report that it is partial, and skip the ranked fixes for anything the run might
still do. Do not stop or steer the run; this skill reads.
