---
name: deepseek-flash-scope
description: Write the plan-scope document that an unattended run is held to — interview the user and read the codebase to produce a frozen statement of what the finished system does, what is explicitly not being built, which files and behaviours must not change, and which commands prove it done. Use this whenever someone needs a scope document, a plan-scope, or a statement of work before delegating; whenever deepseek-flash-autopilot has refused to start for want of one or because a section is thin; whenever a user says pin down the scope, write up the boundaries, what's in and out, or define done before we start; and whenever a plan is about to be handed to agents that will run without anybody watching. Reach for it when the worry is scope creep rather than correctness, and when the person can still answer questions — which is the whole point, because once the run starts nobody can.
---

# Writing a plan-scope document

The document produced here is the only brake on an unattended chain of agent
runs. It gets frozen before the first phase launches and consulted at every
point where an agent could plausibly build something nobody asked for. Which
means the work is not filling in a template — it is extracting, from someone who
has not thought about most of it yet, the decisions that will otherwise be made
without them at 2am on phase five.

Two things make that harder than it sounds, and the process below is shaped
around both.

**The most valuable section is the one that cannot be asked for directly.**
Non-goals are, by definition, the things the user is not thinking about. "What
are your non-goals?" reliably produces two obvious ones and silence. The
technique that works is to invert it: read the plan and the code, generate the
things a capable agent would plausibly add while it is in there, and put those
back as recognition rather than recall. People are excellent at "no, definitely
not that" and hopeless at enumerating the same set from memory.

**Every question left unanswered here becomes an assumption made without you.**
The runner does not stop to ask, because there is nobody to ask. So a vague
answer is not deferred, it is delegated — to an agent, silently, at the moment
it is least convenient. Say that to the user once, early. It changes how much
care they put into the next ten minutes.

The user is present for this skill and questions are cheap. Use them freely, in
batches, rather than guessing.

## What you need to start

The plan, if one exists — its phases are the richest source of candidate
non-goals, because each phase is a place an agent will be standing when
temptation strikes. If there is no plan yet, this still works, and writing scope
first is arguably the better order: a scope document constrains the plan that
gets written against it. Say which order you are in, and note that the runner
needs both.

Then read the code the work touches. Not the whole repository — the surfaces the
plan names, their callers, and their tests. Every fence you write has to be a
path that exists, and every command in the definition of done has to be one this
repository actually has. A scope document written from the plan alone describes
a codebase that is not there.

## Step 1 — what this delivers

One paragraph, present tense, describing the system working. This is the
referent for every later judgement: the runner's test for whether unplanned work
is permissible is *would the delivered thing still work without it*, and that
question is meaningless unless something says what the delivered thing is.

The line to hold is falsifiability. "The poller reads its interval from
config.yaml and picks up changes without a restart" can be failed by a running
system. "Improve configuration handling" cannot be failed by anything, so
nothing can be measured against it and every argument about scope is winnable
from both sides.

Draft it yourself from the plan and show it for correction. Users edit a
concrete wrong paragraph far more readily than they write a right one from
nothing — and the corrections are where the real requirements surface.

Push on three things while drafting:

- **The fallback behaviour.** What happens on the path where the new thing is
  absent, malformed, or empty. This is the single most common hole, and it is
  the one an agent will fill with a guess.
- **Who else consumes it.** A binary, a second binary, a CLI, a UI, a test
  harness. "The harness reads it" and "everything that starts up reads it" are
  different pieces of work and users often say the first while meaning to scope
  the second.
- **What existing behaviour must survive untouched.** Usually this becomes a
  fence in step 3, and it is easier to catch here while thinking about the end
  state.

## Step 2 — the candidate pass

Before asking anything, build the list. Read the plan phase by phase and, for
each, write down what a capable agent standing in that code would plausibly add
beyond the brief. `references/non-goal-prompts.md` holds the categories that
generate these reliably — generalisation, the adjacent settings, the obvious
next feature, the tidy-up, the second consumer, the compatibility shim, test and
CI expansion, docs and observability. Work the categories against each phase;
fifteen to twenty-five candidates from a four-phase plan is normal.

Cut the ones that are already unambiguous from the plan. What remains is
genuinely open, which is exactly what to put to the user.

Then ask, in batches, as multi-select: *which of these are you not building?*
Four questions of four options per call, so sixteen candidates a round. Frame
each option as the thing itself, concretely, not as a category — "hot reload:
config changes take effect without a restart" rather than "reload behaviour".
The concreteness is doing the work; a category is something people say yes to
vaguely and an agent then interprets.

Three things to watch for in the answers:

- **A "yes, do that" is a plan change, not a scope entry.** If they want it, it
  belongs in the plan as work, and the plan may need a phase for it. Say so
  rather than quietly writing it into the scope document as in-scope, where
  nobody will schedule it.
- **A "maybe" or "only if it's easy" is a non-goal.** There is nobody to
  adjudicate "easy" mid-run. Write it as a non-goal and note it as a candidate
  for a follow-up piece of work; that is a real answer, whereas a conditional is
  an invitation to decide unilaterally.
- **An answer that surprises you means the delivers paragraph is wrong.** Go
  back and fix step 1 before continuing.

## Step 3 — fences

Concrete paths, packages, and behaviours that must not change, each with the
reason it must not. Concrete is load-bearing: the runner checks a pull request's
file list against this section mechanically, so `internal/queue/**` is a fence
and "don't break the queue" is a sentiment that checks nothing.

Your job here is the translation. The user says "leave the queue alone" and you
resolve it to the paths, then verify each one exists — a fence with a typo in it
matches nothing and silently protects nothing.

Three sources, and the second is the one people forget:

- **What the user names.** Directly asked, and usually the shortest list.
- **What the plan implies.** Anything a phase sits next to but is not meant to
  touch. If two phases both change a file, that is not a fence but it is worth
  noting — it is where a merge will hurt.
- **What is fenced by circumstance.** Work in flight on another branch, a
  release in progress, a package someone else owns. Ask specifically; this never
  comes up unprompted and it is the fence whose violation is most expensive.

Add behavioural fences where a file-level one would not catch it: a wire format,
an exported signature, a database schema, a CLI flag that deployments pass. An
agent can preserve a file and still break the contract inside it.

## Step 4 — definition of done

The commands that prove the whole piece of work, not each phase. The runner
executes these on the integration branch before opening the final pull request,
and whether they pass decides whether that PR is a draft.

Get them from the repository rather than from memory — a CLAUDE.md, a scripts
directory, a Makefile, the CI config — and then run each one on the current tree
to confirm it works before it goes in the document. A definition of done
containing a command that does not exist, or that already fails on `main`, gives
the runner a failure it cannot distinguish from its own.

Include at least one check that is about behaviour rather than the suite passing.
"`harness serve` with no config.yaml behaves as it does on main" catches a class
of thing no unit test was written for, and it is the kind of check a human would
have made by hand.

## Step 5 — rehearse the gate

Do not skip this because the document looks complete. It is what separates a
scope document from a filled-in template.

Walk the plan again, phase by phase, holding the draft. For each phase ask: what
is the most plausible thing that goes wrong here, and *does this document
already answer it* — in scope, non-goal, or fence? Anything it does not answer
either way is a hole, and a hole is where the run makes its own call.

Two specific rehearsals worth running every time:

- **The interface phase.** Whichever phase defines the signatures the others
  call. Does the document say enough for a later agent to know whether changing
  that signature is allowed?
- **The last phase.** By then the branch has accumulated five phases of other
  agents' code, and the temptation is cleanup. Is cleanup ruled out? It should
  be, explicitly, and it almost never is in a first draft.

## Step 6 — write it, and freeze it

Write the file in the shape `references/template.md` specifies, beside the plan
document. The three sections the runner's gate requires are *what this
delivers*, *non-goals*, and *fences*; a document missing any of them, or
carrying it as generalities, will be refused before anything is spent.

Then show the user the non-goals list on its own, one more time, and say plainly
that from the moment the run starts this is not negotiable without stopping it.
That last read is cheap and it is the last one they get.

Commit it. A scope document in the working tree is frozen by promise; one
committed before the run starts is frozen by `git diff`, and the difference
matters when the thing it constrains is a process that edits files for hours.

## What a weak one looks like

Recognisable at a glance, and worth naming because a first draft usually has at
least two:

- **Non-goals that restate the plan.** "Not building a web UI" for a change with
  no UI anywhere near it. A non-goal is only worth a line if somebody might
  plausibly have built it.
- **Fences without reasons.** The reason is what lets the runner tell a real
  collision from a coincidental file match, and it is what stops a fence being
  read as decoration.
- **A delivers paragraph describing activity.** "Refactor the config layer" is
  work, not a delivered system. If it cannot be observed running, it cannot
  serve as the referent.
- **Definition of done that is just the test suite.** True of every change ever
  made to the repository, so it distinguishes nothing about this one.
- **Nothing about the absent or malformed path.** The most reliable hole, and
  the one an agent fills most confidently.
