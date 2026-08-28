---
name: worktree-remove
description: >
  Tears down a finished agent-harness worktree — runs `wt rm <slug>`, which checks for unsaved
  work, destroys the worktree's containers, network and volume, frees its allocated ports and
  slot, and removes the git worktree. The remote branch is left alone because the PR points at
  it. Takes the worktree slug as its argument (e.g. `/worktree-remove tunnel-retry`). Use this
  skill whenever the user says "finish worktree", "clean up worktree", "tear down worktree",
  "remove worktree", "done with worktree", "delete worktree <slug>", or "free up that worktree's
  slot". Do not trigger while there is still active work in the worktree, or when the run failed
  — leaving it alive is the right answer in those cases.
---

# worktree-remove — agent-harness

Destroys a finished worktree and frees its slot for the next one. `wt rm` does
the whole job in the right order: the safety checks, then the teardown of
every container, network and volume docker labelled with this worktree's
compose project, then the deallocation in the coordinator, then `git worktree
remove`. Doing the git removal first is what leaves a container holding a port
nothing can account for.

The remote branch is **left alone** — that is where the PR points.

This repo's facts are in the managed block at the end of this file.
Regeneration refreshes that block and preserves edits to this text.

## Arguments

- **`<slug>`** *(required)* — the worktree slug. Extract it from surrounding
  text if needed.

With no argument, **stop and ask**. Never auto-pick "the most recent
worktree"; that is how someone's work in progress gets destroyed.

---

## Phase 1 — Resolve the target

```bash
git worktree list
wt list --json
```

Cross-check both, and name what you found before doing anything. A slug in
one and not the other is a partial state, not a reason to improvise: `wt rm`
handles the registry side even when the directory is already gone, and if
neither lists the slug then it is already cleaned up or the slug is wrong —
say so and stop.

---

## Phase 2 — Remove

From the **repo root**. You cannot remove the worktree you are standing in:

```bash
cd "$(dirname "$(cd "$(git rev-parse --git-common-dir)" && pwd)")"
wt rm --slug <slug>
```

`wt rm` runs three safety checks first — uncommitted changes, unpushed
commits, an open PR via `gh` — and what each does on a hit is this
repository's own policy, set in `wt.yaml`'s `removal:` block.

**Exit 3 means a refusing check hit. Stop and ask.** Show the user what it
said and let them decide. Do not reach for `--force`: it is not a remedy for
a refusal, it is a way of discarding the thing the refusal was protecting.
Exit 4 means a check could not run at all — `gh` missing, most likely — and
it fails closed on purpose; install the missing piece rather than working
around the check.

---

## Phase 3 — Verify and report

```bash
wt doctor
```

A clean `doctor` confirms nothing leaked — no container still labelled with
the project, no slot still held. Then report the path removed, the slot
freed, and that the branch remains on the remote because the PR points at it.

If the user is mid-review-cycle, mention that the worktree can be recreated
from the same branch: make the worktree on the existing branch, then `wt
init`. `/worktree-create` always branches from `main`, so it is the wrong
tool for resuming.

---

## Hard rules

- **Never clean up on a failure path.** Failed smoke, no PR,
  mid-investigation — leave it alive.
- **Never force a git worktree removal.** Git refusing is signal: it means a
  check missed something, usually a build artifact and sometimes a local
  config the user wants.
- **Never delete the remote branch.** The PR points at it. Cleanup is local.
- **Never delete a worktree you are standing in.** `cd` to the repo root
  first.
- **Never auto-pick a worktree to delete.** Require an explicit slug.
- **Never tidy up by hand after a failed teardown.** If `wt rm` stops half
  way, report what is left and stop; deleting containers or volumes yourself
  frees a slot whose resources still exist.
- **Never produce an ExitPlanMode block.** This is cleanup, not planning.

# --- managed by wt; edits below are overwritten ---
# wt-field: app=agent-harness
# wt-field: band harness_http=8700
# wt-field: band vite=5700
# wt-field: descriptor=wt-env.json
# wt-field: resources=harness_http, vite, compose, workspaces
# wt-field: shared=the host docker socket, the deepseek-harness-prod stack, harness-unity-state (docker volume), harness-unity:latest (docker image), the DeepSeek account and the GitHub App installation, the Go module cache and the npm cache
# wt-field: worktrees=.claude/worktrees/{slug}
# --- end ---
