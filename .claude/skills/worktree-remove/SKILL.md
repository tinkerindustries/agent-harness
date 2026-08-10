---
name: worktree-remove
description: >
  Tears down a finished deepseek-harness worktree — checks for unsaved work, runs
  `harness worktree rm <slug>` to destroy the worktree's containers, network, and volumes for
  both its dev and test compose projects and free its allocated ports, then
  `git worktree remove .claude/worktrees/<slug>`. The remote branch is left alone because the PR
  points at it. Takes the worktree slug as its argument (e.g. `/worktree-remove tunnel-retry`).
  Use this skill whenever the user says "finish worktree", "clean up worktree", "tear down
  worktree", "remove worktree", "done with worktree", "delete worktree <slug>", or "free up that
  worktree's slot". Do not trigger while there is still active work in the worktree, or when the
  run failed — leaving it alive is the right answer in those cases.
---

# Worktree Remove — deepseek-harness

Destroys a finished worktree and frees its slot so the next worktree can take it. Both halves
are needed: `harness worktree rm` releases the containers/volumes/network and the port
allocation, `git worktree remove` releases the directory. Skipping the first leaks docker
resources and a held slot; skipping the second leaves a stale directory and a dangling
`git worktree list` entry.

The remote branch is **left alone** — that is where the PR points.

## Arguments

- **`<slug>`** *(required)* — the worktree slug. Extract it from surrounding text if needed.

With no argument, **stop and ask**. Never auto-pick "the most recent worktree"; that is how
someone's work in progress gets destroyed.

---

## Phase 1 — Resolve the target

```bash
git worktree list
harness worktree list
```

Cross-check both. Partial states are common and each needs different handling:

- **Directory gone, registry entry remains** → run only `harness worktree rm <slug>`. This is
  the usual case when someone ran `git worktree remove` by hand first.
- **Directory remains, no registry entry** → run only `git worktree remove`.
- **Neither** → already cleaned up, or the slug is wrong. Say so and stop.

---

## Phase 2 — Defensive checks

Run these from inside the worktree, then return to the repo root. Each one stops the skill and
asks; the cost of an unnecessary confirmation is far below the cost of destroying real work.

1. **Uncommitted changes** — `git status --porcelain`. Any output means work would be lost.
2. **Unpushed commits** — `git log @{u}..HEAD --oneline 2>/dev/null || git log --oneline`. The
   fallback fires when the branch was never pushed, which is itself a stop signal.
3. **PR open** — `gh pr view --json url,state -q '.url + " (" + .state + ")"' 2>/dev/null`. No
   PR usually means the work has not shipped yet.

All three clear → proceed silently.

---

## Phase 3 — Tear down

From the **repo root** — you cannot remove the worktree you are standing in:

```bash
cd <repo-root>
harness worktree rm <slug>
git worktree remove .claude/worktrees/<slug>
```

`harness worktree rm` removes every container, network, and volume docker compose labelled with
this worktree's dev *and* test compose project names, then frees the slot in the registry. It
works from the registry alone — by docker compose project label, not by reading a compose file —
so it is still correct if the directory is already gone.

If it reports resources left behind (a docker daemon that was unreachable, say), it deliberately
leaves the registry entry in place rather than freeing a slot for containers that still exist.
Fix the underlying issue and re-run `harness worktree rm <slug>` rather than editing the registry
by hand.

`git worktree remove` refusing because the worktree is dirty is real signal, not an obstacle —
**stop and look** if that happens. It means Phase 2 missed something: usually a build artifact,
sometimes a local config the user wants. Do not pass `--force` to override it automatically.

---

## Phase 4 — Verify and report

```bash
harness worktree doctor
```

A clean `doctor` confirms nothing leaked. Then report:

```
✓ Removed .claude/worktrees/<slug> and freed slot <N>.
  Branch <slug> remains on the remote (PR points at it).
```

If the user is mid-review-cycle, mention that the worktree can be recreated from the same
branch: `git worktree add .claude/worktrees/<slug> <slug>`, then `npm --prefix web install` and
`harness worktree init`. (`/worktree-create` always branches from `main`, so it is the wrong
tool for resuming an existing branch.)

---

## Hard rules

- **Never clean up on a failure path.** Failed smoke, no PR, mid-investigation — leave it alive.
- **Never delete the remote branch.** The PR points at it. Cleanup is local only.
- **Never delete a worktree you are standing in.** `cd` to the repo root first.
- **Never auto-pick a worktree to delete.** Require an explicit slug.
- **Never pass `--force` to `git worktree remove` automatically.** Git refusing is signal.
- **Never skip `harness worktree rm`.** Removing only the directory leaks the environment
  (containers, network, volumes) and holds the slot, and the leak is invisible until slots run
  out or `docker ps` gets crowded.
- **Never produce an ExitPlanMode block.** This is cleanup, not planning.

---

## Example

> User: `/worktree-remove tunnel-retry`
>
> *`git worktree list` shows `.claude/worktrees/tunnel-retry` on `tunnel-retry`;
> `harness worktree list` shows it holding slot 3.*
>
> *Defensive checks from inside the worktree: clean tree, nothing unpushed, PR #412 open.*
>
> *Back at the repo root: `harness worktree rm tunnel-retry`, then
> `git worktree remove .claude/worktrees/tunnel-retry`, then `harness worktree doctor` — clean.*
>
> Skill: "✓ Removed `.claude/worktrees/tunnel-retry` and freed slot 3. Branch `tunnel-retry`
> remains on the remote (PR #412 points at it)."

> User: `/worktree-remove docs-fix`
>
> *Worktree exists, but `git status --porcelain` returns `M src/foo.ts`. **Stop.***
>
> Skill: "Worktree `docs-fix` has uncommitted changes:
>
> ```
> M src/foo.ts
> ```
>
> Removing it loses them. Proceed anyway, or handle them first?"
