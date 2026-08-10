---
name: worktree-create
description: >
  Creates a fresh, fully isolated git worktree for deepseek-harness — pre-flights the main
  checkout, pulls the latest main, creates the worktree at .claude/worktrees/<slug> on branch
  <slug>, installs the frontend's node_modules, and runs `harness worktree init` followed by
  `docker compose up -d --build` to allocate this worktree's own NATS/harness/MCP/test-broker/
  vite ports and its own dev and test compose project names, so it cannot collide with any other
  worktree or with the deepseek-harness-prod stack. Takes an optional short slug (e.g.
  `/worktree-create tunnel-retry`); generates a random `adjective-animal` slug when omitted. Pass
  `--no-env` to create the worktree without allocating ports or starting compose — useful for
  docs-only changes. Use this skill whenever the user says "set up a worktree", "create a
  worktree", "spin up a worktree", "new worktree for X", "make me a worktree", "I want to work
  on this in parallel", or any equivalent ask for an isolated checkout. Do not trigger when the
  user is already inside a worktree and wants to keep working there.
---

# Worktree Create — deepseek-harness

Creates an isolated worktree and brings up its own environment. This is scaffolding, not an
execution agent: it hands back a ready worktree and stops.

Isolation comes from `harness worktree init`, which assigns this worktree a slot in
`~/.deepseek-harness/worktrees.json` and derives every port and compose project name from it.
Without that step the worktree shares the main checkout's ports, containers, and compose
projects — see [`docs/WORKTREES.md`](../../../docs/WORKTREES.md) for the full allocation and
what stays shared regardless.

## Arguments

- **`<slug>`** *(optional)* — short kebab-case name (e.g. `tunnel-retry`). Sanitised to
  `[a-z0-9-]`, must start with a letter or digit (`harness worktree init` rejects anything
  else). When omitted, generate a random `<adjective>-<animal>` slug (`swift-otter`,
  `bold-lynx`), both words ≤8 characters. State the chosen slug up front so the user can `cd`
  to it. The branch created is named exactly `<slug>`, with no prefix.
- **`--no-env`** *(optional)* — skip `harness worktree init` and `docker compose up`. Say
  explicitly that you skipped it.

---

## Phase 1 — Pre-flight

Run from the **main checkout**, on `main`, with a clean tree:

```bash
pwd
git rev-parse --abbrev-ref HEAD
git status --porcelain
```

If the working tree is dirty, the branch is not `main`, or you are already inside a worktree —
**stop and say so**. Do not auto-stash or auto-checkout; the user's work in progress matters
more than this skill finishing.

```bash
git pull --ff-only origin main
```

`--ff-only` surfaces a diverged local branch as an error instead of silently merging.

---

## Phase 2 — Create the worktree

Check for collisions first:

```bash
ls .claude/worktrees/<slug> 2>/dev/null
git rev-parse --verify --quiet refs/heads/<slug>
```

- **Random slug collides** → regenerate silently, up to 3 attempts, then ask.
- **User-supplied slug collides** → stop and ask. The existing worktree may hold real work;
  recreating it silently is how that gets lost.

```bash
git worktree add .claude/worktrees/<slug> -b <slug>
cd .claude/worktrees/<slug>
```

Everything after this runs from inside the worktree.

---

## Phase 3 — Install dependencies

```bash
npm --prefix web install
```

A fresh worktree has no `node_modules` — worktrees share git objects, not build artifacts (npm's
own package cache is shared and this is fast). Go module downloads happen lazily on the first
`go build`/`go test`/`harness worktree init`, sharing the host's module cache, so there is no
separate Go install step.

If the install fails, stop and surface the output rather than working around it with force flags.

---

## Phase 4 — Allocate the slot and start the stack (skip if `--no-env`)

```bash
harness worktree init
docker compose up -d --build
```

`init` allocates the lowest free slot and writes `.worktree-env.xml` at the worktree root plus
the matching block in `.env` — read ports and URLs from the descriptor rather than assuming any
value, because they differ per worktree. `docker compose up -d --build` then reads that `.env`
to bring up this worktree's own `nats`/`harness`/`mcp` containers under its own compose project.

If `init` reports "already initialised", that's fine — it's idempotent and just reconciled the
existing slot; proceed to `docker compose up` regardless.

---

## Phase 5 — Report

State plainly:

- Worktree path and branch
- Dependency install: done
- Environment: started, with the harness HTTP URL from `.worktree-env.xml` (or read it back with
  `harness worktree show`) — or "skipped" if `--no-env`
- **Any resources this worktree still shares** — read the `<shared>` block in
  `.worktree-env.xml` and repeat it. Writes to those escape the worktree, and whoever works here
  needs to know before they make one. The docker socket and the DeepSeek/GitHub credentials are
  always on that list.
- Current working directory

Then stop. The caller takes it from here.

---

## Hard rules

- **Never run on a dirty tree or a non-`main` branch.** Stop instead; do not auto-stash.
- **Never silently reuse an existing worktree or branch.** Ask.
- **Never skip `harness worktree init` without saying so.** A worktree with no allocated slot
  collides with the main checkout and every other worktree, and the failure looks like an
  unrelated bug (a port already in use, or `docker compose up` reusing another worktree's
  containers).
- **Never hardcode a port when reporting URLs.** Read `.worktree-env.xml` (or
  `harness worktree show`).
- **Never produce an ExitPlanMode block.** This is scaffolding, not planning.

---

## Example

> User: `/worktree-create tunnel-retry`
>
> *Pre-flight passes: main checkout, on `main`, clean. Pulls latest.*
>
> *Creates `.claude/worktrees/tunnel-retry` on branch `tunnel-retry`, cds in.*
>
> *Runs `npm --prefix web install`, then `harness worktree init`, then
> `docker compose up -d --build`.*
>
> Skill: "Worktree ready at `.claude/worktrees/tunnel-retry` on branch `tunnel-retry`. Slot 3 —
> harness at http://127.0.0.1:8703, MCP on 8803, vite dev on 5703 (`npm --prefix web run dev`
> once you need it). Still shared: the host docker socket, and the GitHub/DeepSeek credentials
> copied from the main checkout's `.env`."
