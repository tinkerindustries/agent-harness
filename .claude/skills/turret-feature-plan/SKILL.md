---
name: turret-feature-plan
description: Write a feature plan and its companion feature memory under docs/plans/, split into phases with explicit dependencies so an orchestration agent can hand each phase to a separate implementation session, ending with a scope-creep review that strips anything the user did not ask for. Use this whenever the user wants a feature planned, scoped, phased or broken up — "plan this feature", "write an implementation plan", "break this into phases", "this is bigger than one session", "get this ready for agents to pick up", "scope out X before we build it" — and whenever work is about to be delegated to other sessions and nothing yet says what they are each meant to build. Reach for it even when the user never says "plan": a request to work out how to build something large, in what order, and what proves each step done, is this skill. Pair it with feature-orchestrate, which runs the plan it writes.
---

# Feature plan

You are writing two files that a fresh session will act on without you there to explain
them. Everything the implementation agents need has to be on the page.

| File | Holds |
|---|---|
| `docs/plans/<slug>.md` | The plan: what gets built, in what order, what proves each phase done |
| `docs/plans/memory/<slug>.md` | The feature memory: the shared facts every agent on this feature needs, kept small |

Read [docs/plans/README.md](../../../docs/plans/README.md) first. It is the repo's own word
on what a plan is and where reports land, and it wins over anything here that contradicts it.

## Interview the user, more than once

Find out what is actually being asked for. Ask about the parts that change the shape of
the work, and settle the rest by reading code — a question whose answer is in the
repository spends the user's attention on something you could have looked up.

Ask with `AskUserQuestion`, in rounds. Each round is at most four questions with concrete
options, so the user picks rather than composes. Do not write the plan off one round.

**Round one — the shape of the feature.** Before you have read much:

- What does the finished feature do, from the outside?
- What is deliberately out of scope?
- Is there an existing subsystem this has to fit into, or a design document that already
  covers part of it?
- Does any of it need a look on screen before it can be built?

**Then read the code the feature touches.** A plan written without reading the code invents
seams that already exist under different names, and every agent that picks it up pays for
that.

**Round two — what the code turned up.** Reading changes the questions worth asking. The
first round asks what the user wants; the second asks them to decide the things only the
code could raise:

- A seam that already exists and nearly fits: extend it or build beside it?
- Two places the behaviour could live, where the choice sets which phases can run in
  parallel.
- An assumption from round one the code contradicts. Say what you found and ask what wins.
- A phase boundary that only makes sense one way once you have seen how the files are
  split.

Then draft the plan. Keep going round as long as a round is still changing the shape of
it: a third round is cheap, and a phase built on a guess is not. Stop when the remaining
unknowns are ones the implementation agents can settle for themselves, and put those under
the plan's **Open questions** heading.

**Show the shape back before the detail.** Once the phases have names and dependencies but
before you write them out in full, put the phase list and the parallelism in front of the
user and ask whether the split is right. A phase list is a few lines to change now and
three sessions to change later.

Do the interview and the reading before drafting. A plan you rewrite after the first
question was answered is cheaper than one three agents have already started on.

## Explore here, not in a planning agent

Do not hand this to the `Plan` agent or to any other planning subagent. A subagent comes
back with a plan in its reply and takes its exploration with it, so you end up transcribing
someone else's conclusions into two files whose whole purpose is to carry what was found.
The interview above is also yours: the user answered you, and those answers are half of
what the plan encodes.

Explore in this session instead, under the discipline a planning agent works to. Until you
have a plan the user has agreed to, treat the repository as read-only:

- Read files, list directories, search with `grep` and `find`, and read git history with
  `git status`, `git log`, `git diff` and `git show`.
- Write nothing. No new files, no edits, no `mv`, `cp`, `rm`, `mkdir` or `touch`, no
  scratch files anywhere including `/tmp`, no `>`, `>>` or heredocs, no `git add` or
  `git commit`, no installs, and nothing that runs the build or changes what is on disk.

The reason is the size of the mistake. Planning is where you understand the feature least,
and a file written from a half-formed idea is one an implementation agent later reads as
settled. Reading costs nothing and can be redone; a wrong file gets built on.

While exploring, do what an architect does with the code:

- Find the existing patterns and conventions this feature has to sit inside.
- Find the closest feature that already works, and trace how it is put together.
- Follow the code paths the feature touches end to end, rather than reading the entry
  points and guessing at the middle.
- Note the trade-offs you are choosing between, and say in the plan which way you went.

Read the official documentation for anything outside this repository the feature leans on —
an API surface, a library, a framework setting, a platform feature. Fetch the vendor's own
docs rather than working from memory. Versions move, and a plan built on a remembered API
sends every agent that picks it up down the same wrong path. Do this while you explore, not
after the phases are drafted: what a setting actually does often decides where the phase
boundary goes.

Prefer the vendor's own pages over blog posts and answers. When the docs and the installed
version disagree, the installed version wins — check it in the lockfile or the package
itself and say in the plan which one you went with.

The read-only rule lifts once you are writing the plan, the feature memory and any issue
the review raises. Those files are the output and nothing else is.

## Choose the slug

One short kebab-case name for the feature, used for the plan file, the memory file and the
report directory. `docs/plans/reports/<slug>/` is where agents write their reports, so the
slug is the thing tying the three together. Check it is not already taken.

## Shape the phases

A phase is one agent session's worth of work. The agent that picks it up starts cold: it
reads the plan, the feature memory and the code, and nothing else. Size a phase so that
fits — a phase spanning six subsystems will be half-done when the session runs out of room.

Every phase carries:

- **Depends on** — the phases that must land first, by number, or "nothing".
- **What it builds** — the behaviour, and the files or subsystems it lives in.
- **Proof** — the command that passes, or the observation on screen, that says it is done.
  The orchestrator quotes this into the job prompt and the report answers it, so a proof
  nobody can run is a phase nobody can close.
- **Out of scope for this phase** — where a neighbouring phase's work starts, when the
  boundary is easy to walk over.

Order phases so that the ones with no dependency between them can run at the same time.
Say so explicitly in the dependency table; the orchestrator reads that table to decide what
to run in parallel, and will serialise anything it cannot prove independent.

Two phases that write the same file are not independent. Split by file boundary where you
can, and where you cannot, make the dependency explicit rather than hoping.

### Design phases

UI work that has not been designed yet gets its own phase before the phase that builds it.
A design phase's output is a document, not code: it goes in `docs/design/` and it settles
layout, states, and what the thing does when it has no data or has failed. The build phase
then depends on it and cites it.

Give a feature a design phase when someone would otherwise have to invent the interaction
while writing the component. Skip it when the surface is already covered by an accepted
design document, and cite that document instead.

A document under `docs/design/` records what a design phase settled and is a guide to
intent. It binds nothing. The code is the authority on behaviour, and an accepted document
can lag the app, so read the document against the code before citing it. A plan may change
what a design document settled: say so in the plan, and have the phase that builds the
change correct the document so one description of the surface remains. When the reason for
the change is worth keeping, it goes in `docs/decisions/design-departures.md`.

### How many phases

The orchestrator runs on one delegation, which caps how many sessions it may hold live and
how many it may create in all (`src/main/orchestration/caps.ts`, and
`docs/design/orchestration-safety.md` for why). It reuses sessions rather than spawning one
per phase, and a plan with a dozen phases still strains that. Aim for the work to divide into
a handful of substantial phases. If it will not, say so in the plan and split the feature into
two plans that land in sequence.

## Write the feature memory

The memory is loaded by every agent on this feature, so its cost is paid once per session
and its size is the whole point. One screen. What belongs in it:

- The base schema, type or interface the feature introduces, and any change to an existing
  one. This is the thing agents get wrong independently and expensively.
- Named invariants: what must stay true across phases.
- Links to the files that matter, each with a sentence on why an agent needs it.
- The vocabulary the feature uses, where a word means something specific here.
- The external facts the exploration cost you: the API shape, the setting, the version
  constraint an agent would otherwise look up again or guess at. Write the fact in a line
  or two and link the vendor page it came from, with the version it describes. The link is
  there for the agent that needs the rest of the page, so the memory does not have to
  carry it.

What does not belong in it:

- Code. A schema is written as a type or a table, and an implementation is not.
- Anything derivable by reading the file it would link to instead.
- Progress, status, or what has been built so far. That is the plan and the reports.

The orchestrator is the only thing that edits this file after you write it. Implementation
agents suggest additions in their reports and the orchestrator decides.

Templates: [assets/plan-template.md](assets/plan-template.md) and
[assets/memory-template.md](assets/memory-template.md). Keep every heading they carry, and
write "Unknown" under one you cannot answer rather than dropping it — a missing heading
reads as an oversight, and "Unknown" reads as a question for the orchestrator to close.

## Review for scope creep

Do this last, on the finished draft, as a separate pass. A plan grows while it is written:
each phase suggests a neighbour, and by the end the plan builds a system nobody asked for.
The multi-phase shape makes it worse, because a phase that quietly widened the feature is
handed to an agent who has no way to know it was never requested.

Go through the plan phase by phase and, for each thing it builds, point at the sentence in
the user's request it comes from. Anything you cannot trace:

- Delete it, if the plan works without it.
- Move it to the plan's **Not building** section, if it is a reasonable idea for later.
- Raise it in `docs/issues/open/`, if it is a real problem you found on the way.

Then check the other direction: everything in the request appears somewhere in the plan.

The **Not building** section is not decoration. It is what the orchestrator holds each
agent's work against when a report proposes going further, so write down the tempting
adjacent things and say they are out.

Tell the user what the review removed and where it went. That is the moment they get to
disagree, and it is cheaper than disagreeing after three sessions have built it.

## Hand off

Say the slug, the two file paths, the phase count, and which phases can run in parallel.
The user starts the work with `feature-orchestrate`, which needs a delegation from them
before it can spawn anything.
