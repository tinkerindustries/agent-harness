---
name: deepseek-flash-plan
description: Orchestrate a piece of work that is too big for one harness run across a chain of DeepSeek V4 flash phases — decompose it into phases, cut one integration branch off main, run each phase as its own deepseek-flash-task against that branch, merge each phase before launching the next, and open a single pull request to main at the end. Use this whenever the user wants deepseek, flash, or the harness to build something that spans several stages; whenever they say break this into phases, do it in stages, plan it out and then delegate, or ask for a chain of PRs, stacked branches, or an integration branch. Reach for it whenever you are about to launch a second deepseek_agent run that needs the first one's code, and whenever you catch yourself pointing two harness tasks at main when they have to see each other's work. For one self-contained task, use deepseek-flash-task instead.
---

# Orchestrating a phased plan across flash runs

A harness run is one agent, one workspace, one push. Work that does not fit in a
single run is not made to fit by writing a longer prompt: flash loses the middle
of a long context, and the run ends when its sub-turn budget does regardless of
how much of the task is left.

The way through is to split the work into phases and give each phase its own
run, on a branch that accumulates. This skill is the orchestrator for that. The
`deepseek-flash-task` skill is what you invoke for each individual phase, and it
owns the prompt template, the result schema, the launch arguments, and how to
read a result honestly. Read it. What is here is only what changes when a run is
one of several: where the branch comes from, what the agent is told about the
phase before it, and what you do in the gap between runs.

If the work is one phase, this is the wrong skill. Go straight to
`deepseek-flash-task`; a plan with one item is overhead.

## The shape

```
main
 └─ deepseek/<feature>                       ← cut once, PR to main at the end
      ├─ deepseek/<feature>-p1-<slug>  → PR → merged into the integration branch
      ├─ deepseek/<feature>-p2-<slug>  → PR → merged
      └─ deepseek/<feature>-p3-<slug>  → PR → merged
```

`main` is never the base of a phase. That is the whole point: an agent clones
the base branch at launch time, so a phase pointed at `main` cannot see the code
the phase before it wrote, and you find out when its diff conflicts or
re-implements what already exists. Pointing every phase at the integration
branch instead costs one extra argument and removes the failure entirely.

The other thing to be clear-eyed about before you start: each phase is an
independent chance to fail. A flash run that gets a phase right 85% of the time
gives you a 61% chance of a clean three-phase chain and a 44% chance of a clean
five-phase one. That arithmetic is the argument for the shortest plan that still
leaves each phase verifiable, and for reading every result before launching the
next rather than queueing them all and hoping.

## Step 1 — decide the phases

Read the code before you propose a split. A plan written from the task
description alone invents boundaries the codebase does not have, and a boundary
is the one thing you cannot cheaply change once phase 1 has merged.

What makes a phase boundary hold up:

- **Each phase leaves the tree green.** The agent runs the repository's own
  tests at the end of its run, and a phase that cannot pass them is a phase that
  reports failure for a reason you designed in. If a piece of work only makes
  sense half-finished, it belongs inside one phase, not across two.
- **Split by capability, not by activity.** "Write the tests" then "write the
  code" gives you a first phase that cannot pass its own suite and a second that
  can silently rewrite the tests to suit itself. "Parse the config" then "use it
  in the poller" gives you two phases that each stand up.
- **The first phase owns the interface the others touch.** A wrong signature
  found in phase 2 costs one rewrite; found in phase 4 it costs three. Front-load
  the decision that everything else depends on, even when it is the least
  interesting work in the plan.
- **Do not plan a phase whose output a later phase rewrites.** A tidy-up before
  a rewrite is a run spent on code that will not survive.
- **Cross-cutting changes are one early phase.** A rename or a config parameter
  threaded through twelve files is miserable to merge if three phases each do
  part of it.
- **Two to five phases.** Fewer and you did not need this skill. More and the
  compounding above says you are unlikely to reach the end without intervening,
  so plan the first three and re-plan when they land.

Name what each phase leaves behind, in a sentence, as you write the plan. That
sentence is not decoration — it becomes the context the *next* agent gets, and
writing it now is what tells you whether the boundary is real.

Then show the user the plan before you spend anything: the phase list with one
line each, what each leaves behind, and the honest cost — a phase is five to
twenty minutes, so a four-phase plan is most of an hour and they can walk away.
Ask specifically about the boundaries rather than the plan in general. That is
the decision that is expensive to revisit, and it is the one they have context
on that you do not.

## Step 2 — cut the integration branch

One command, and deliberately not a checkout:

```bash
git fetch origin main
git push origin origin/main:refs/heads/deepseek/<feature>
```

That creates the branch on the remote without touching the working tree, the
current branch, or anything uncommitted. It matters because the orchestration
runs for an hour alongside whatever the user was already doing, and a skill that
moves them onto another branch to do its bookkeeping is a skill they will not
run twice.

Do not open the pull request to `main` yet. GitHub refuses a pull request whose
head has no commits the base lacks, and right now the integration branch is
`main`. That PR is step 6, once there is something to show.

## Step 3 — run one phase

Follow `deepseek-flash-task` from its step 2 onwards. Four things differ, and
the first of them is the one that decides whether the chain works at all:

- **The base is the integration branch, not `main`.** It is an ordinary base as
  far as `deepseek-flash-task` is concerned — pass it as `repos[].branch` and use
  the same branch everywhere the template says `<base>`, in the `gh pr create
  --base` as well as the clone. Every phase after the first depends on this
  being right, and nothing in the run will tell you when it is not.
- **Name the phase in the branch.** `deepseek/<feature>-p2-<slug>` keeps the
  phases sorted together and readable in the branch list a week later.
- **Ask for a normal pull request, not a draft.** You are going to merge it in a
  few minutes, and a draft has to be marked ready first. Draft is the right
  default for the PR to `main` at the end, where a human reviews it; here it is
  just a step to undo.
- **Tell the agent what is already there, and what is not its phase.** Two short
  sections, both worth the tokens:

```
## What is already there

Phase 1 landed on this branch: internal/config/parse.go exposes
Load(path) (*Config, error), and the poller still reads its interval from the
flag it always did. The config file is parsed and validated but nothing consumes
it yet — that is your phase.

## Not this phase

Phase 3 replaces the retry logic in internal/queue. Leave it alone, even where
it obviously wants the new config. Wrong: threading the config into the retry
path because you are already in the file. Right: the poller reads its interval
from Load, the retry path is untouched, and phase 3 has a clean starting point.
```

Take "what is already there" from the previous phase's **report**, not from your
plan. The plan is what you intended; the report is what happened, and the gap
between them is exactly what the next agent needs to be told. A capable agent
with no picture of the phase after it will helpfully do part of that phase
badly, and you will not find out until the plan no longer matches the branch.

## Step 4 — land the phase before you launch the next

```bash
gh pr merge <n> --squash --delete-branch
```

Squash so the integration branch carries one commit per phase, which is what
makes the final PR to `main` readable. If the repository forbids squash merges,
`--merge` is fine; the readability is a preference, the merge is not.

The merge is also your existence check, and it is free. `pushed: true` is the
agent's claim about a directory you cannot see; a pull request that merges is a
fact. When a report claims a push but carries no PR URL, `git fetch origin
<phase-branch>` settles it before you believe either way.

Then the ordering rule the whole scheme rests on: **the next phase does not
launch until this one is merged.** The next agent clones the integration branch
at the moment it starts, so an unmerged phase is invisible to it — and a chain
where phase 3 silently built on phase 1 is the failure this skill exists to
avoid. If you want two phases in flight at once, they have to be genuinely
independent — disjoint files, no ordering — and even then they merge one at a
time and the second may conflict.

## Step 5 — re-read the plan before launching the next phase

This is the step that makes a human orchestrator worth more than a shell script,
so do not skip it because the phase came back clean.

The report tells you things the plan assumed wrongly: work deferred, an
interface that came out differently, a test that had to change to accommodate
it. Read the remaining phases against what actually landed and edit them now.
Where something was deferred, decide which of three things it is — folded into
the next phase, promoted to a phase of its own, or dropped — and say which to
the user rather than leaving it to be discovered in the final diff.

A plan executed unchanged through four phases is more often a plan nobody
re-read than a plan that was right.

## Step 6 — the pull request to main

```bash
gh pr create --base main --head deepseek/<feature> --draft \
  --title "<feature>" --body "<the plan as executed>"
```

The body is the plan as executed, not as written: a line per phase saying what
it did, what it deferred, and what verification the agent reported running. That
is the thing the reviewer needs and the only thing you have that nobody else
does — no phase agent saw more than its own slice, and the individual phase PRs
are already merged and out of sight.

Keep it a draft if anything was deferred or any phase reported a check it could
not get passing. The draft flag is how you say "this is complete as a change but
not as a task", which is worth more than a green PR that quietly omits a
requirement.

## When a phase does not come back clean

Stop. Do not launch the next phase — it would clone a branch missing the code
its brief assumes, and you would spend a second run producing a diff that has to
be thrown away with the first.

Then read what kind of failure it was, because the responses differ:

- **Timed out.** Partial work often survives. Check for a pushed branch and a
  PR; if the work is coherent as far as it goes, merge it and make the remainder
  its own phase rather than re-running the whole thing.
- **`gave_up`, or stopped without calling `Complete`.** Read `error`, or the
  final text when there is no structured result. A phase that failed on its
  brief — an ambiguity, a missing constraint, a task that turned out to need a
  decision — is worth relaunching once with that fixed. It has to be a fresh
  run: `resume` exists, but only on the CLI, not on the MCP surface.
- **Failed twice.** The boundary was wrong, not the agent unlucky. Re-split the
  work or do that piece yourself; a third identical run is the most expensive
  way to learn the same thing.
- **`workspace_setup`.** Nothing ran. Almost always the base branch: check the
  integration branch actually exists on the remote and that you spelled it the
  same way in `repos[].branch` as you did in `git push`.

Tell the user what happened and what you propose before spending another run.
Minutes and money are the units here, and the choice between re-running,
re-splitting, and taking over is theirs to weigh.

## While it is running

Use `deepseek-harness-wait` for each phase — a background poll that wakes you
when the run ends, rather than a conversation turn per check. Hand over the
transcript URL as soon as `deepseek_agent` returns it, so the user can watch
instead of waiting on you.

Between phases, say which one just landed and which is starting. Across an hour
of runs, silence is indistinguishable from a stall, and the user cannot see the
queue you are working through.
