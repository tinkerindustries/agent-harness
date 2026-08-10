---
name: deepseek-flash-task
description: Delegate a coding task to a DeepSeek V4 flash agent running with full permissions in the deepseek-harness, which branches off whatever base branch you name, commits, pushes, opens a draft PR, and returns a schema-validated report of what was done, what was deferred, how it was verified, and what went wrong. Use this whenever the user wants deepseek, flash, or "the harness" to do a piece of work; whenever they say delegate, hand off, farm out, or "get an agent to" do something on a branch; whenever they name deepseek_agent or deepseek_result; whenever they ask for work to be done on a named branch by something other than you; and whenever the work has to start from an existing branch rather than main. Reach for it even when the user does not name this skill, as long as the work is meant to happen in a harness run rather than in this session.
---

# Delegating a task to a flash agent

A harness run is a fire-and-forget agent session on the other side of a NATS
queue. It clones the repositories you name into a directory of its own inside
the harness container, works there with the tool set the permission mode
allows, and publishes one result. You never see that directory, which is what
makes the push mandatory rather than a nicety. The workspace does outlive the
run — it sits under `workspaces/<session-id>/` on whatever machine the harness
is on — but nothing in the MCP surface reaches into it, so a commit that was
never pushed is not destroyed so much as stranded somewhere you would have to go
and dig it out of by hand.

Two mechanics do the heavy lifting, and both are worth knowing before you write
the prompt.

`result_schema` is enforced in Go against the `Complete` tool's arguments. A
payload that misses a field comes back to the model as a tool error and it
tries again, so the shape of the report is guaranteed in a way that asking
nicely in prose is not. The schema is also pasted verbatim into the agent's
opening message, so the field descriptions are prompt text — that is where the
detailed instructions about each field live, and why the prompt below does not
repeat them.

The run is thinking-mode flash. `temperature` and friends are accepted and
ignored, and `tool_choice` cannot force a tool call, so wording is the only
lever you have. `docs/PROMPTING.md` in the deepseek-harness repo records what
is actually known about that wording; the prompt rules below are its
consequences.

## Step 1 — settle the inputs

Four things, and you can usually work out three of them yourself:

- **The task.** Whatever the user said, in their words. Step 2 turns it into the
  brief the agent actually receives, so all you need here is to notice a whole
  requirement that is missing and ask for it.
- **The branch name.** The caller provides it. If they did not, propose
  `deepseek/<short-slug>` and confirm in the same message where you confirm the
  scope, rather than making it a separate round trip.
- **The clone URL.** `git remote get-url origin` in the working repository.
  Rewrite ssh to https — `git@github.com:owner/repo.git` becomes
  `https://github.com/owner/repo.git` — because the container authenticates with
  a `GITHUB_TOKEN` credential helper for `https://github.com` and has no ssh
  key. An ssh URL fails at clone time and burns the run.
- **The base branch.** Whatever the work should start from, passed as
  `repos[].branch`. It reaches `git clone --branch`, which takes any ref that is
  already pushed — a release branch, someone else's feature branch, the branch a
  previous run left behind — not just the repository's default. The agent
  branches from whatever it finds checked out, so this is the argument that
  decides what its diff is against.

  Decide it rather than defaulting to it. `main` is right for a self-contained
  change, and wrong whenever the task builds on code that has not landed yet:
  point it at the branch holding that code, or the agent will re-implement it or
  conflict with it. A chain of runs where each one continues the last is what
  `deepseek-flash-plan` is for.

Name the repository, branch, and base in the same message where you show the
rewritten brief at the end of step 2. A run costs minutes and dollars, and a
wrong base is only visible afterwards.

## Step 2 — rewrite the brief

The user's words are the input to the prompt, not the prompt. A brief written
for you is written for someone who can ask a follow-up question; the agent
cannot, so ambiguity you pass through becomes a guess you inherit an hour
later. Put the brief through the same rules as the scaffolding before it goes
into `## Task`.

What that means concretely:

- **Split it under headers once it has more than one part.** Long contexts lose
  their middle, and a header is what makes a requirement findable from the end
  of the prompt.
- **Replace a quality word with a check.** "Make the poller faster" gives the
  agent nothing to aim at or to verify against; "cut the poll interval to 5s
  without raising CPU above its current idle" does both.
- **Turn a prohibition into a pair.** A bare "don't break the API" is reported
  to do nothing. "Wrong: renaming the exported field. Right: adding the new one
  and leaving the old in place" names the actual mistake.
- **Name the first line of anything it writes.** Flash drifts into preamble, so
  a brief that asks for a document or a report should say what its opening line
  is.
- **Resolve the ambiguity, or state the assumption.** Decide it yourself where a
  careful colleague would, and write the decision into the brief. Where you
  genuinely cannot, say so in the brief and give the agent the reading to
  prefer, rather than leaving it open.
- **Keep the user's specifics verbatim.** Paths, symbol names, error text, and
  version numbers survive the rewrite untouched. A paraphrase that loses a file
  path costs more than the rewrite gains.
- **Add no scope.** A rewrite that invents requirements spends the run on work
  nobody asked for, and you will not find out until the diff arrives.

**Example**

Input: "the CSV importer chokes on big files, sort it out"

Output:

```
## Task

importer/csv.go reads the whole file into memory before parsing, so a 2 GB
export takes the process out with an OOM.

Stream it instead: parse row by row, so peak memory stays flat as the file
grows. Demonstrate it against a generated 2 GB fixture, not a small one.

Leave the column-mapping behaviour alone. Wrong: changing how a missing header
is treated because the parser moved. Right: the same rows in, the same records
out, with the existing importer tests still passing unmodified.
```

Show the user the `## Task` section you composed before you launch, and nothing
else — the boilerplate is the same every time and reading it again wastes their
attention. A rewrite that quietly changed the meaning is cheap to catch here and
expensive to catch in the result.

## Step 3 — write the task prompt

Fill the template below, with the rewritten brief under `## Task`. The rules the
rest of it embodies, so you can adapt it rather than copy it blindly:

- **Anchor the language.** Flash drifts into Chinese without an explicit English
  instruction. One line covers it.
- **Anchor the ending.** Flash also drifts into preamble unless the prompt names
  the finishing move. "Finish by calling Complete" is that anchor.
- **Pair the counterexample.** "Wrong: X / Right: Y" is reported to work where a
  bare prohibition does nothing, so spend the words on the failure modes that
  actually cost you: claiming a test passed without running it, and reporting
  success on an unpushed branch.
- **Recap the constraints at the end.** Long contexts lose the middle. The last
  lines of the prompt are the ones that survive.
- **Skip the persona.** "You are a meticulous senior engineer" is reported to
  reduce consistency, and the harness system prompt has already told the model
  what it is.
- **Do not restate the schema.** It arrives immediately after your task text,
  and duplicating it wastes tokens and invites contradiction.

Two failure modes are worth pre-empting in the prompt every time, because both
end a run that was otherwise fine. Git has no identity configured in the
container, so the first `git commit` fails with "please tell me who you are".
And the workspace root holds one directory per cloned repository, so the agent
has to `cd` into the repo before any git command.

```
Work in English. Write all output, commit messages, and your final report in English.

## Task

<the rewritten brief from step 2>

## Repository and branch

The workspace root holds one directory per cloned repository. Work inside
<repo-dir>/. Every git command below runs there.

Before your first commit, give git an identity — it has none in this container
and the commit will fail without one:

    git config user.email "deepseek-harness@localhost"
    git config user.name "deepseek-harness"

Branch off <base> and do your work on <branch-name>:

    git checkout -b <branch-name>

Commit in logical steps rather than one lump at the end. Then push and open a
draft pull request against <base>:

    git push -u origin <branch-name>
    gh pr create --draft --base <base> --title "<title>" --body "<what and why>"

Nothing reaches into this workspace once the run ends, so work that is committed
but not pushed cannot be collected. Treat the push as part of finishing the
task, not as a follow-up.

## Verification

Find out how this repository builds and tests itself — a README, a scripts
directory, a Makefile, or the CI config — and run those commands against your
change. Record each one, exactly as you ran it, in the verification field.

If a check fails and you cannot fix it, that is a result worth reporting, not a
reason to hide the check.

Wrong: reporting a test suite as passing because the code looks correct.
Right: running the suite, and reporting "failed" with the failing assertion when
it fails.

## Reporting

Finish by calling Complete with a result matching the schema below this task.

Wrong: error left empty after a run that stopped halfway, because the summary
mentions the problem.
Right: error carrying what blocked you, what you tried, and what state the branch
was left in — and empty only when the branch is pushed and the work is done.

If you get stuck badly enough that you cannot continue, still call Complete,
with status "gave_up", the fields filled in as far as they go, and the reason in
error. A run that ends silently tells the requester nothing.

## Constraints

- Work only inside <repo-dir>/ in the workspace.
- Branch off <base>, name the branch <branch-name>, push it, open a draft PR.
- Run the repository's own build and test commands and report each one.
- English throughout.
- Finish with Complete.
```

## Step 4 — launch

Call `deepseek_agent`:

- `description`: a short label, visible in `deepseek_runs`.
- `prompt`: the filled template.
- `repos`: `[{ "url": "<https clone url>", "branch": "<base>" }]`. Pass `branch`
  every time, even when it is `main`. Leaving it out works, but it hides the one
  argument most worth seeing in the call you are about to make, and a base that
  was defaulted rather than chosen is only visible in the diff an hour later.
- `profile`: `"flash"`.
- `permission_mode`: `"full"`. The run needs Bash, Write, and network access to
  push. It also runs as root and, in this image, holds the host's docker
  socket — so `full` is a real grant, worth naming to the user rather than
  passing silently.
- `result_schema`: the contents of `references/result-schema.json`, read and
  passed as a JSON object.
- `max_sub_turns`: leave it out for anything an experienced engineer would
  finish in an hour; the server default is 400 — about 40 minutes of flash
  sub-turns, inside the default hour-long deadline. Raise it only for work
  that will clearly need more, and say so, since the run ends as `timeout`
  when the budget runs out.

The call returns immediately with a `request_id` and usually a transcript URL.
Give both to the user in your next message. Runs take minutes, and a user who
has the URL can watch instead of waiting on you.

## Step 5 — collect

While the run is in flight, poll `deepseek_status` with `request_id` — it never
blocks, and returns where the run is up to: what it is working on, its todo
list, and cost so far. Pass on its status line each time rather than polling in
silence. Once the run is done, call `deepseek_result` with the same
`request_id` to collect the final outcome; it never blocks either, and finds a
persisted result even long after the run finished, from any session. After
about ten minutes with no result, tell the user where it has got to and ask
whether to keep waiting; the run continues either way.

## Step 6 — read the outcome honestly

`status` and `complete_status` are independent, and the difference matters:

- `status: "ok"` with `complete_status: "done"` — the run finished on its own
  terms. Read `result.error` anyway; that is the agent's own verdict, and it is
  the field most likely to contradict a cheerful summary.
- `status: "ok"` with `complete_status: "gave_up"` — the run completed the
  protocol and failed the task. Lead your report with `result.error`.
- `complete_status: ""` — the model stopped without calling `Complete`, so
  `result` is null and there is no structured report at all. You have the final
  assistant text in `text`. Say plainly that the structured result is missing
  rather than reconstructing it from prose.
- `status: "timeout"` — hit `max_sub_turns` or the deadline. Check whether a
  branch was pushed before the cutoff; partial work often survives.
- `status: "failed"` with `error.code: "workspace_setup"` — a clone was refused
  or the base branch does not exist. Nothing ran. Usually an ssh URL that should
  have been https, or a base branch that exists only in your checkout — the
  clone is from the remote, so a base you have not pushed is a base the run
  cannot see.

When you relay the result, check the parts that are cheap to check. `pushed:
true` with an empty `pull_request_url` means the branch is there and the PR is
not, and the reason belongs in your report. If the branch was pushed, `git fetch
origin <branch-name>` confirms it exists rather than taking the agent's word for
it.
