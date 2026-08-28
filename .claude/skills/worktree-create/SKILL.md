---
name: worktree-create
description: >
  Creates a fresh, fully isolated git worktree of agent-harness — pre-flights the main checkout,
  pulls the latest main, creates the worktree at .claude/worktrees/<slug> on branch <slug>, and
  runs `wt init` to allocate this worktree's own harness HTTP port, Vite port, compose project
  and workspace root, then bring the stack up and seed its credentials, so it cannot collide
  with any other worktree or with the deepseek-harness-prod stack. Takes an optional short slug
  (e.g. `/worktree-create tunnel-retry`); generates a random `adjective-animal` slug when
  omitted. Pass `--no-env` to create the worktree without allocating anything or starting
  compose — useful for docs-only changes. Use this skill whenever the user says "set up a
  worktree", "create a worktree", "spin up a worktree", "new worktree for X", "make me a
  worktree", "I want to work on this in parallel", or any equivalent ask for an isolated
  checkout. Do not trigger when the user is already inside a worktree and wants to keep working
  there.
---

# worktree-create — agent-harness

Creates one isolated worktree and brings up its own environment. This is
scaffolding, not an execution agent: it hands back a ready worktree and stops.

Isolation comes from `wt init`, which allocates this worktree a slot and
derives its harness HTTP port, its Vite port, its compose project name and its
workspace root from it. The compose project is the one that matters most —
it namespaces the containers, the network and the `harness-data` volume that
holds this worktree's whole SQLite store, so a worktree that skips `init`
does not merely share ports, it can attach to another worktree's containers
and write into another worktree's database.

This repo's facts — the descriptor filename, the band bases, what stays shared
— are in the managed block at the end of this file. Regeneration refreshes
that block and preserves edits to this text.

## Arguments

- **`<slug>`** *(optional)* — short kebab-case name (e.g. `tunnel-retry`).
  When omitted, generate a memorable `<adjective>-<animal>` slug, both words
  at most eight characters. Adjectives: brisk, calm, eager, fleet, grand,
  hasty, lucky, nimble, quick, rusty, swift, zesty. Animals: badger, cougar,
  falcon, heron, jaguar, lemur, otter, panda, raven, tiger, walrus, zebra.
  State the chosen slug up front so the user can `cd` there. The branch is
  named exactly `<slug>`, with no prefix.
- **`--no-env`** *(optional)* — skip `wt init` entirely. **Always say so
  explicitly in the report**; a worktree with no allocation collides with the
  main checkout and every other worktree, and the failure looks like an
  unrelated bug.

---

## Phase 1 — Pre-flight

Run from the **main checkout**, on `main`, with a clean tree:

```bash
git rev-parse --show-toplevel
git branch --show-current
git status --porcelain
```

A dirty tree, a branch other than `main`, or a cwd already inside a worktree
— **stop and say so**. Never auto-stash, never auto-checkout, never silently
merge. The user's work in progress is worth more than this skill finishing.

```bash
git pull --ff-only origin main
```

`--ff-only` surfaces a diverged local branch as an error rather than quietly
merging. It also refreshes the remote refs the slug check below reads.

---

## Phase 2 — Choose the slug

Check all three namespaces, not just the two that are local:

```bash
# --show-toplevel returns the WORKTREE root when run from inside one, which
# reports every existing worktree as free. --git-common-dir always points at
# the main repository's .git, from anywhere.
ROOT=$(dirname "$(cd "$(git rev-parse --git-common-dir)" && pwd)")

test -e "$(wt spec path --slug <slug>)"                    # directory
git show-ref --verify --quiet refs/heads/<slug>            # local branch
git ls-remote --exit-code --heads origin refs/heads/<slug> # REMOTE branch
```

The remote check is the easy one to skip and the expensive one to miss: the
local checks pass, the worktree gets made, work gets committed, and the
collision only surfaces at `git push` as a rejected non-fast-forward against
somebody else's branch.

- **Generated slug collides** → regenerate silently, up to three attempts,
  then ask.
- **Supplied slug collides** → stop and ask. The existing worktree may hold
  real work, and recreating it silently is how that gets lost.

---

## Phase 3 — Make the worktree

`wt spec path` is what decides where it goes — this repository's own
convention, read from `wt.yaml`, rather than a path typed here:

```bash
git worktree add "$(wt spec path --slug <slug>)" -b <slug>
cd "$(wt spec path --slug <slug>)"
```

Everything after this runs from inside the worktree.

---

## Phase 4 — Attach the environment (skip if `--no-env`)

```bash
wt init --description "<one line on what this worktree is for>"
```

That one command does the whole sequence, because the hooks in `wt.yaml`
carry it: allocate the slot, write `wt-env.json` and the managed block in
`.env`, `npm --prefix web ci`, build the image, `docker compose up -d --wait`
under this worktree's own project name, seed the credentials, and health-check
the result.

`init` is idempotent — if it reports an existing allocation it has reconciled
it, which is fine; carry on.

**The seed step can fail without the worktree being broken.** It copies the
DeepSeek API key and the GitHub App credential from the main checkout's
running stack (`scripts/wt-seed.sh`), and the usual reason it fails is that
the main checkout's stack is not up, which `docker ps` confirms. Say so in the
report and carry on: the worktree works, it just needs its credentials set
from its own settings screen before a run in it can call a model or clone
anything private.

---

## Phase 5 — Report

Read every value back rather than assuming one:

```bash
wt show --json
```

State plainly:

- Worktree path and branch.
- Slot, and the harness URL built from the descriptor's `harness_http` — the
  web UI, `/api/...` and `/mcp` are all on it. The Vite port too, for
  `npm --prefix web run dev`.
- Credentials: seeded from the main checkout's stack, or the reason they were
  not.
- **What this worktree still shares.** Read the descriptor's shared block and
  repeat it. Writes to those escape the worktree and whoever works here needs
  to know before they make one. The host docker socket is always on that
  list, and through it a session in `full` permission mode can reach every
  other worktree's containers.
- Current working directory.

Then stop. The caller takes it from here.

---

## Hard rules

- **Never run on a dirty tree or a non-`main` branch.** Stop instead; never
  auto-stash.
- **Never silently reuse an existing worktree or branch.** Ask.
- **Never skip `wt init` without saying so.**
- **Never hardcode a port when reporting URLs.** Read `wt show`.
- **Never produce an ExitPlanMode block.** This is scaffolding, not planning.

---

## Resuming an existing branch

The flow above branches from `main` and is the wrong tool for resuming work
that already exists. For the review-feedback case: make the worktree on the
existing branch by whatever mechanism, then `wt init` — never branch from
`main` and silently discard the feedback the existing branch was meant to
address.

# --- managed by wt; edits below are overwritten ---
# wt-field: app=agent-harness
# wt-field: band harness_http=8700
# wt-field: band vite=5700
# wt-field: descriptor=wt-env.json
# wt-field: resources=harness_http, vite, compose, workspaces
# wt-field: shared=the host docker socket, the deepseek-harness-prod stack, harness-unity-state (docker volume), harness-unity:latest (docker image), the DeepSeek account and the GitHub App installation, the Go module cache and the npm cache
# wt-field: worktrees=.claude/worktrees/{slug}
# --- end ---
