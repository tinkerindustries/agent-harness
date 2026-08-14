---
name: deepseek-flash-autopilot
description: Execute an existing written plan to completion, unattended, one phase at a time — hand each phase to a DeepSeek V4 flash run, check the diff it produced, merge it, reconcile the remaining plan against what actually landed, and go again until every phase is done, with a frozen plan-scope document as the only authority on what may and may not be built. Use this whenever a plan already exists and the user wants it carried out rather than designed — when they say execute this plan, run the plan, work through the phases, do it end to end, do it overnight, don't ask me just do it, or hand you a plan document and a scope document together. Reach for it whenever a run has to make its own calls because nobody will be there to answer, and whenever scope creep across a long chain of delegated runs is the thing to guard against. Not for work whose phases have not been decided yet, and not for one self-contained piece of work — that is deepseek-flash-task.
---

# Running a plan on autopilot

This skill executes a plan somebody else already wrote. The decomposition is
not yours, the requirements are not yours, and — the part that changes
everything — there is nobody to ask. A question you would normally put to the
user is a question you have to answer yourself and write down.

That constraint is what the whole design turns on. An unattended chain of
delegated runs fails in two ways that an attended one does not. It drifts: each
phase adds a little of what seemed sensible at the time, and eight phases later
the thing built is not the thing planned. And it forgets: a six-phase run is
hours long, your context gets compacted somewhere in the middle, and a plan you
are holding only in the conversation is a plan you will lose.

So there are two instruments, and neither is optional:

- **The plan-scope document** is frozen and answers every question of the form
  "should this be built". You never edit it. It is the one input to the run that
  you did not write and cannot rationalise your way around, which is precisely
  what makes it worth something.
- **The ledger** is a file on disk that you rewrite after every phase. It, not
  the conversation, is the live state of the run. After a compaction it is how
  you find out where you were.

Read `deepseek-flash-task` before you start. It owns the brief, the result
schema, the launch arguments, and how to read a report honestly, and every phase
here is one of its runs. What is here is the topology, the loop, the scope gate,
and what to do when something goes wrong and there is no one to ask.

If the phases have not been decided yet, this is the wrong skill. Deciding a
boundary is the one thing in this process that is expensive to revisit and that
the user has context on that you do not, so it belongs in a conversation with
them, not in an unattended run.

## The branch topology

```
main
 └─ deepseek/<feature>                       ← cut once, PR to main at the end
      ├─ deepseek/<feature>-p1-<slug>  → PR → merged into the integration branch
      ├─ deepseek/<feature>-p2-<slug>  → PR → merged
      └─ deepseek/<feature>-p3-<slug>  → PR → merged
```

`main` is never the base of a phase. An agent clones its base branch at launch
time, so a phase pointed at `main` cannot see the code the phase before it
wrote — it will re-implement that work or conflict with it, and you find out in
the diff rather than in the run. Pointing every phase at the integration branch
costs one extra argument and removes the failure entirely.

Be clear-eyed about the compounding before you start. Each phase is an
independent chance to fail, so a flash run that gets a phase right 85% of the
time gives a 61% chance of a clean three-phase chain and 44% for five. That
arithmetic is why every phase is read before the next one launches rather than
queued and hoped for, and why the failure budget below is finite.

## The gate: no scope document, no run

Before anything else, confirm both documents exist and that the scope document
is real. `references/plan-scope.md` is the specification and a worked example.
The three sections the loop actually consults are:

- **what the finished system does** — the referent for every later judgement
- **non-goals** — absolute, and the one thing that outranks your own reasoning
- **fences** — concrete paths and behaviours that must not change

A document missing any of those three, or carrying them as generalities you
could argue either side of, does not constrain anything. Stop and say so, name
which section is missing, and point at `references/plan-scope.md`. Do not write
the missing sections yourself and proceed: a scope document you authored is one
you will find reasons to reinterpret at 2am on phase five, and it defeats the
purpose of having an authority outside your own judgement. Refusing here costs
nothing — nothing has been spent yet.

Everything else is your problem to solve rather than to ask about. A plan with
vague phase boundaries, a missing verification story, an ordering that will not
work — resolve it, record the decision as an assumption in the ledger, and go.

## Preflight

Four things, once, before phase one.

**Read the code the plan touches.** Not the whole repository; the surfaces the
plan names. You will be judging every phase's diff against your understanding of
this codebase, and a judgement formed from the plan document alone is a
judgement about a codebase that does not exist.

**Cut the integration branch** without disturbing the working tree:

```bash
git fetch origin main
git push origin origin/main:refs/heads/deepseek/<feature>
```

**Set up a verification worktree**, once, and reuse it for the whole run:

```bash
git worktree add <scratch>/verify deepseek/<feature>
```

This is where you build and test between phases. It exists because checking a
merge means running something, and running something in the user's checkout
means disturbing a tree they are probably using. Install whatever the repo needs
to build there now — dependencies, generated assets — so that a phase check is
seconds rather than a setup. Do not bring stacks up in it: this worktree has no
port allocation of its own, and in deepseek-harness itself anything involving
`docker compose` needs `harness worktree init` first. Tear it down at the end
with `git worktree remove`.

**Write the ledger.** `references/ledger.md` is the format. Put it beside the
plan document as `<plan-basename>-ledger.md`, leave it untracked, and fill in
the live plan from the plan document — one line per phase, each saying what that
phase leaves behind. That live plan, not the original file, is what you brief
from once the run starts. The plan document is never edited: it is the baseline
you diff intent against, and keeping it clean is how you can still tell, at the
end, what changed and why.

Then tell the user the phase count, the honest cost — five to twenty minutes a
phase, so a six-phase plan is well over an hour — the ledger path, and, if the
plan creates something new, the foundations and library versions you settled on
below, in a line or two. Not as a question: as the one chance they have to say
"not that one" before six phases are built on it. That is the last thing you
need them for.

## Foundations, when the plan creates something new

A phase that adds a function to an existing package inherits everything about
how that package works. A phase that stands up a new application, service, or
package inherits nothing — and every decision the surrounding code would
normally have settled is instead settled by whichever phase agent reaches it
first. Attended, you notice at the second one and correct it. Unattended, you
get a service whose handlers log through three libraries, validate in two
styles, and are each tested in a way of their own.

So settle these once, at preflight, and write them into the ledger. They cost a
paragraph in each brief afterwards and they remove a whole class of divergence
that no per-phase diff check reliably catches, because each phase looks
internally consistent on its own.

**Start from what the repository already does.** A new package inside an
existing repo is not greenfield: the repo's logger, its error convention, its
test layout, its build, and its dependency set already have opinions, and most
of the answers are a directory away. Introducing a second way to do something
the repo already does is a cost paid by every future reader for a preference the
plan never asked for. That holds even where the existing choice is not the one
you would make.

Genuinely new ground — a new deployable, a new language in the tree — is where
you have to decide. The concerns worth an answer before phase one are the ones
that run through every file rather than sitting in one, because those are the
ones that cannot be retrofitted by a later phase without touching everything the
earlier phases wrote:

- **Errors** — wrapped, sentinel, or typed, and what a caller is meant to do
  with one.
- **Configuration and secrets** — where they come from, and what happens when
  one is absent.
- **Logging and observability** — the library, structured or not, and what a
  request or job carries through it.
- **Persistence** — the store, the migration story, who owns the schema.
- **The boundary** — input validation, authentication, authorisation, on
  anything with a surface. Adding authn across handlers written without it is a
  rewrite, not a phase.
- **Lifecycle** — startup, readiness, cancellation, shutdown.
- **Tests** — the runner, where tests live, what one looks like, how fixtures
  work. A phase that cannot see the convention invents one, and then there are
  two.
- **Build and run** — how it compiles, how it runs locally, how it is packaged.

Not all of them apply; a new internal package has no boundary and no lifecycle.
Answer the ones that do, in a sentence each. A record that will not fit a
sentence a line is a design document, and this is not that.

This is not licence to build the foundations as a phase zero the plan does not
have. Every one of these is a *decision* recorded in the ledger and repeated in
briefs. The code for it lands in the phase that needs it, and it is subject to
the scope gate like anything else.

### Libraries, and the versions they get pinned to

**Choosing.** Prefer what the repo already depends on. Then the standard library
over a dependency that saves ten lines. Then something maintained and widely
used, over the one with the nicer API. Before committing, check the thing is
alive — last release, whether it is archived, whether the issues are answered —
because a dependency picked at preflight is one that every later phase builds
on, and it is free to change now and expensive at phase four.

**Pinning, which is the one that actually bites.** An agent told to add a
dependency writes a version number from memory, and that memory is a snapshot
from training. What comes back is a version a year old, or one that never
existed, or a current number attached to code written against the API two majors
ago. None of it looks wrong in the report, and in a stack without a compiler
nothing catches it until runtime.

So resolve versions yourself, at preflight, against the registry rather than
against anyone's recollection — including your own — and put what you resolved in
the ledger and in every brief that installs something:

```bash
npm view <pkg> version                  # node
go list -m -versions <module>           # go
pip index versions <pkg>                # python (or read the PyPI page)
cargo search <crate>                    # rust
```

Latest *stable*, not the newest tag: a pre-release, or a major published last
week, is a version whose problems nobody has hit yet and whose answers are not
searchable. Where the current major is newer than a model is likely to remember
— a rewrite, a renamed API, a changed import path — say so in the brief and name
the migration note or doc page that covers it. Otherwise the agent writes
fluent, confident code against the previous major, and you are the only reader
who would know.

## The loop

For each phase in order. Never two at once, never out of order, never past a
phase that did not land.

### 1. Brief

Compose the phase brief with `deepseek-flash-task` step 2, from the ledger's
live plan rather than the original document. Four sections are specific to
running unattended, and all four are worth the tokens:

**What is already there** — taken from the previous phase's *report and diff*,
never from the plan. The plan is what you intended; the diff is what exists, and
the next agent needs the second one. Name the symbols it will call and the
signatures they actually have.

**Not this phase** — the fences from the scope document, verbatim where they are
concrete, plus the phases that come after this one. A capable agent given no
picture of phase four will do a third of phase four badly, and unattended you
will not notice until the plan no longer matches the branch.

**Foundations** — where the phase creates something new, the relevant lines from
the ledger's foundations record, and the pinned version of anything it installs.
Repeat them every phase rather than assuming the last one's diff carries the
convention; an agent sees the code it clones, but a convention is much easier to
follow when it is stated than when it has to be inferred from four files. Where
the phase adds no new surface, say so and name what it extends instead — that is
a shorter line and it does the same job.

**Hands off the documents** — the plan, the scope document, and the ledger are
not the agent's to touch. Add it to the constraints:

```
Do not modify <plan-path>, <scope-path>, or any *-ledger.md file. Do not merge,
rebase, or push anything other than your own branch.
```

Base is the integration branch, everywhere `deepseek-flash-task` says `<base>` —
the `repos[].branch` and the `gh pr create --base` both. Branch is
`deepseek/<feature>-p<n>-<slug>`. Ask for a normal pull request rather than a
draft; you are merging it in a few minutes.

### 2. Launch and wait

`deepseek_agent` per `deepseek-flash-task` step 4, then `deepseek-harness-wait`
for the wait — a background poll that wakes you when the run ends, rather than a
turn per check. Pass on the transcript URL when it arrives. Unattended does not
mean silent: one line when a phase starts and one when it lands is the
difference between a user who can look in and a user who has to guess.

Every launch carries the phase's identity, so the session list reads as the
chain it is rather than a wall of raw prompts: `title` (the phase's name, at
most 10 words), `description` (what the phase changes, at most 50 words), and
the position `phase` N / `total_phases` M — N is the phase being launched, M
the chain's total, both known from the ledger's live plan. The harness UI
shows the title bold with the description beneath and a `phase N/M` chip, and
your own between-phase line can quote that same title instead of a request id.

### 3. Check the change

Before it merges, not after. Cheap by default, because you will do this six
times, and thorough the moment something looks off.

The cheap check, every phase:

```bash
gh pr view <n> --json state,baseRefName,headRefName,files
gh pr diff <n>
```

Four questions, in order of how often they catch something:

- **Is the base right?** A phase PR based on `main` rather than the integration
  branch is the failure this topology exists to prevent, and it looks completely
  normal until the merge.
- **Does the diff do the phase?** Read it against the brief. A stub, a
  `TODO`, or a test asserting the thing it was meant to test has been skipped,
  all coexist happily with a confident report.
- **Did it cross a fence?** Check the file list against the scope document's
  fences and against "not this phase". This is the drift check, and it is the
  one nobody but you is running.
- **Did it bring anything in?** Read the manifest lines in the diff —
  `go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`. A dependency you did
  not pin, or a version other than the one the brief named, is an agent's memory
  reaching the tree, and it is one of the few things here that a passing build
  actively hides. While you are there, check the phase used the foundations
  rather than a second copy of them: a new logger, a second config path, its own
  error style.
- **Do the report's claims hold?** `pushed: true` with an empty
  `pull_request_url` means the branch is there and the PR is not. `git fetch
  origin <phase-branch>` settles the branch; the PR either exists or it does not.

Then merge and run the cheapest check in the repository that would catch a
broken merge — a build, a vet, a unit run — in the verification worktree:

```bash
gh pr merge <n> --squash --delete-branch
git -C <scratch>/verify fetch origin && git -C <scratch>/verify reset --hard origin/deepseek/<feature>
```

Squash so the integration branch carries one commit per phase, which is what
makes the final pull request readable.

**Escalate to the repository's full test suite** — still in the verification
worktree — whenever any of these is true, rather than on a schedule:

- the diff check found anything you had to think about
- the phase's own report named a check it could not get passing
- the diff touched an interface a later phase depends on, or a file more than
  one phase in the plan will change
- the phase changed existing tests rather than only adding them

A phase that changed a test it was not asked to change is the highest-value
thing on that list. Adjusting the assertion until it passes is the cheapest way
out of a failing suite, and it is invisible in a summary.

If the full suite fails on the integration branch after a merge, that is a
failed phase that happens to have landed. Do not launch the next one on top of
it — go to the failure policy below and treat fixing the breakage as the
relaunch.

### 4. The scope gate

Every phase turns up work the plan did not name. Sort each piece into exactly
one of three buckets, and write the bucket down:

**In plan.** The plan or the scope document already covers it. Build it.

**Load-bearing.** Not written down, and the thing the scope document says is
being delivered does not work without it. Build it, in the phase that needs it,
and record it in the ledger under load-bearing additions so the final pull
request declares it.

**Adjacent.** Everything else. Record it in the ledger under adjacent, and do
not build it.

The test that separates the second from the third, and it is stricter than it
feels at the time: *without this, does the delivered system fail to run, fail
its own tests, or fail to do what the scope document says it does?* If the
honest answer is that it would be uglier, slower, less consistent, harder to
extend, or incomplete-but-working, that is adjacent. "While I am in here" and
"this is obviously better" are adjacent every single time. So is a refactor that
would make the next phase easier — the next phase can carry its own weight.

Two rules on top of the test:

- **A named non-goal is never load-bearing.** If you conclude the delivered
  system cannot work without something the scope document explicitly rules out,
  you have not found load-bearing work — you have found a plan that contradicts
  its own scope. That is the one thing you may not decide alone. Stop the run
  and write the handover.
- **A new dependency runs the gate like anything else.** One the foundations
  record already names is in plan. One that arrived because a phase wanted it is
  load-bearing only if the deliverable does not work without it, and it goes in
  the ledger with its resolved version either way. "Cleaner than writing it
  ourselves" is adjacent, and a dependency is the most permanent kind of
  adjacent work there is.
- **Dropping planned work is a scope change too.** The gate runs in both
  directions. A phase quietly reduced to something achievable is drift wearing a
  different hat, and it belongs in the ledger as an amendment with its reason.

### 5. Reconcile the plan

This is the step that makes an orchestrator worth more than a shell script, so
do not skip it because the phase came back clean.

Read the remaining phases against what actually landed — the diff, not the
report's summary of it. Interfaces come out differently than planned. Work gets
deferred. A phase turns out to have absorbed half of the next one. Where the
plan no longer matches the branch, edit the ledger's live plan now, while you
still remember why, and put a line under amendments saying what changed and what
forced it.

Where a phase deferred something, decide which of three it is — folded into the
next phase, promoted to a phase of its own, or dropped as adjacent — and record
which. Deferred work with no decision attached is how a plan finishes with a
hole in it that nobody notices until the final diff.

A plan that runs six phases unchanged is more often a plan nobody re-read than a
plan that was right.

### 6. Update the ledger

Rewrite it before launching the next phase, not at the end. The phase log entry,
the live plan with this phase ticked, any amendments, assumptions, load-bearing
additions, adjacent items, and the budget line. If your context is compacted
between here and the next phase — and on a long plan it will be — this file is
everything you get to keep.

## When a phase does not come back clean

Nobody is going to tell you what to do, so the budget is fixed in advance and
you spend it in this order.

**First failure: relaunch once, with the brief corrected.** Read `error`, or the
final assistant text when `complete_status` is empty. Most first failures are
brief failures — an ambiguity, a missing constraint, a decision the agent needed
and did not have — and they are worth one more run with that fixed. It has to be
a fresh run; `resume` exists on the CLI but not on the MCP surface. A timeout is
the exception worth handling differently: partial work often survives, so check
for a pushed branch, and if what landed is coherent as far as it goes, merge it
and make the remainder a new phase rather than paying for the whole thing twice.

**Second failure: re-split, once, if there is a seam.** Two failures on the same
brief means the boundary is wrong rather than the agent unlucky. If the reports
show a clear seam — a piece that stands alone and leaves the tree green — split
the phase in two, amend the ledger, and run the first half. If there is no
obvious seam, do not invent one. Stop.

**Third failure, or no seam, or a non-goal collision: stop the run.**

Across the whole run, **three relaunches total**. A plan that needs a fourth is a
plan that is wrong in a way more runs will not fix, and the money is better spent
on a human reading the ledger. Track the count in the ledger so it survives a
compaction.

Never skip a failed phase to try the next one. The phases are serial because
they depend on each other, and a phase built on code that was never written
produces a diff that has to be thrown away along with the one before it.

## Stopping

Stopping is a real outcome, not a failure to finish, and it has a shape:

1. Ledger updated — where it stopped, what the last thing tried was, what the
   remaining phases are as currently understood.
2. Everything that landed stays landed. Do not unwind merged phases; the
   integration branch is a real, reviewable state.
3. Open or update the draft pull request to `main` with what exists so far,
   titled so it reads as incomplete, with the blocker in the first line of the
   body.
4. Tell the user plainly: which phase, what went wrong, what you tried, what you
   think the fix is, and what it will cost to resume.

A run that stops at phase four with a clear handover is worth more than one that
kept going and produced four phases of work built on a broken third.

## The pull request to main

```bash
gh pr create --base main --head deepseek/<feature> --draft \
  --title "<feature>" --body "<from the ledger>"
```

Run the repository's full test suite in the verification worktree first, whether
or not the last phase triggered an escalation. It is the only time the whole
thing is assembled, and it is the last check before a human sees it.

The body comes out of the ledger, and it is the thing only you can write — no
phase agent saw more than its own slice, and the phase pull requests are merged
and out of sight. A line per phase with what it did and what it deferred; then
the amendments, the assumptions you made because there was nobody to ask, the
load-bearing additions, and the adjacent work you deliberately did not do. Where
the run stood something new up, the foundations and every dependency it added
with the version it landed on — a reviewer who is being handed a new service
will want to disagree about the stack first and the code second, and they can
only do that if the choices are stated somewhere other than the diff. Those
sections are the whole value of having run it this way. A reviewer who
knows what was decided unilaterally can check those decisions; one who is handed
a clean summary cannot.

Keep it a draft unless every phase landed, nothing was deferred, and the full
suite passes. Then remove the verification worktree and say where the ledger is.

## After a compaction

If you come back to this partway through and the conversation no longer holds
the run: read the ledger, then `gh pr list --state merged --search
deepseek/<feature>` and the integration branch's log to confirm the ledger
matches the branch. Trust the branch where they disagree, fix the ledger, and
carry on from the first unticked phase. Do not re-run a phase whose commit is
already on the branch.
