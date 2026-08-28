# Per-worktree environments

Two sibling git worktrees of this repo compete for the same host ports and the
same docker compose project name the moment both run `docker compose up`. The
second one either fails to bind or — worse, because nothing reports it —
quietly attaches to the first one's containers and writes into the first one's
database. `wt` gives each worktree a slot, and everything below is that slot
turned into ports, a project name and a workspace root.

`wt.yaml` at the repository root is the spec: what gets isolated, what stays
shared, and what runs to bring a worktree up. This document is the reference;
[wt-decision-record.md](wt-decision-record.md) records why each choice in the
spec was made.

There is one production stack on this machine — `deepseek-harness-prod`, fixed
on port 8180. No worktree is ever allocated onto it and no teardown here ever
touches it: it holds a host-global reservation in the band ledger, covering
both the port and the compose project name.

## The workflow

Normal use goes through the two installed skills:

- `/worktree-create <slug>` — makes the git worktree, then runs `wt init`
  inside it.
- `/worktree-remove <slug>` — runs `wt rm <slug>`, which does the teardown and
  the git removal in the right order.

The raw commands, if you are not going through the skills:

```sh
git worktree add "$(wt spec path --slug <slug>)" -b <slug>
cd "$(wt spec path --slug <slug>)"
wt init --description "what this worktree is for"
```

```sh
cd <repo root>
wt rm --slug <slug>
```

`wt list` shows every registered worktree. `wt show` prints this worktree's
own descriptor. `wt doctor` reports drift, and every finding names the command
that fixes it.

## The slot model

The main checkout is slot 0 and is never managed. It keeps
`docker-compose.yml`'s and `web/vite.config.ts`'s own committed defaults, so
nothing changes for anyone who never creates a worktree. `wt init`, run inside
a linked worktree, allocates the lowest free slot in 1..8 and derives
everything from it:

| Resource | Slot 0 (main, unmanaged) | Worktree slot N |
| --- | --- | --- |
| Harness HTTP port — web UI, `/api/...`, and `/mcp` | 8080 | 8700 + N |
| Vite dev server port | 5173 | 5700 + N |
| Compose project | `agent-harness` (the directory basename) | `agent-harness-<slug>-<N>` |
| Workspace root | `<checkout>/workspaces` | `<worktree>/workspaces` |

The last digit of every allocated port is the slot, so slot 3's ports both end
in `3` — readable straight out of `docker ps` or `lsof`.

The compose project is the resource that carries the most: it namespaces the
containers, the `_default` network and the `harness-data` volume that holds
this worktree's entire SQLite store. Isolating it isolates all four at once,
which is why a worktree that skips `wt init` is not merely sharing ports.

Eight slots, because each one is a container, a named volume and a workspace
root. The ceiling is what the machine will carry, not what the port space
allows.

## What `init` writes

- **`wt-env.json`** at the worktree root — the machine-readable record of the
  allocation: identity, slot, every resource, and the list of things this tool
  does *not* isolate. Gitignored; per-worktree, per-machine state, never
  committed. Read it with `wt show` rather than parsing it.
- **`.env`**, in a block between the `# --- managed by wt` markers. A first
  run seeds the file from the main checkout's `.env`, so any model overrides
  carry over, then appends the block; a re-run replaces only that block,
  leaving the rest — hand edits included — alone.

Everything downstream flows from `.env` through the normal channels:
`docker-compose.yml`'s `${VAR:-default}` substitutions, `config.LoadDotEnv`
for `harness serve` run directly on the host, and `web/vite.config.ts`'s
`loadEnv` of the repository root.

Nothing in this repo needed a code change to become worktree-aware except
`web/vite.config.ts`, whose `/api` proxy target was hardcoded to
`127.0.0.1:8080`. It reads `HARNESS_HTTP_PORT` and `HARNESS_VITE_PORT` from
the root `.env`, falling back to 8080/5173 when neither is set. Everything
else was already relative to the working directory or namespaced by the
compose project, so it isolates for free once the project name does.

## The hooks

`wt init` runs the spec's hooks in order, so one command brings a worktree
all the way up:

| Hook | What it runs | Why |
| --- | --- | --- |
| `install` | `npm --prefix web ci` | The frontend's dependencies live in the tree. The container builds its own copy in the Dockerfile's `web` stage, so this is for `npm run dev` and for the frontend tests `scripts/build.sh` runs. |
| `build` | `docker compose -p <project> build` | |
| `start` | `docker compose -p <project> up -d --wait` | |
| `seed` | `bash scripts/wt-seed.sh` | The credentials — see below. |
| `health` | a probe inside the harness container | Namespace-independent on purpose: it runs against the container's own loopback, so it works whether `wt` runs on the host or inside a container, and needs no curl on the machine running `wt`. |

## Seeding a worktree's credentials

A worktree's SQLite store lives in its own compose volume, so a fresh worktree
starts with no DeepSeek API key and no GitHub App private key. It can neither
call a model nor clone anything private, and typing the same two secrets into
every new worktree is the kind of friction that gets worked around badly.

The `seed` hook runs `scripts/wt-seed.sh`, which pipes the main checkout's
stack into this one:

```sh
docker exec <source> harness seed-credentials -export \
  | docker compose -p <project> exec -T harness harness seed-credentials -import
```

**What travels.** Only the settings in the Credentials group, minus
`http.control_token` — `settings.SeedableCredentialKeys`, derived from the
registry so a credential added later is carried without anyone remembering
this file. The control token is left out deliberately: it is generated per
installation and guards that installation's own run-control endpoints, so a
copy would make one stack's bearer token work on another.

**What does not.** Everything else in the store stays where it is — the work
queue, sessions, events, workspace leases, the MCP server registry. A worktree
that inherited `work_queue` rows would claim and run work queued for another
stack, against a workspace root it does not own, and a copied lease would
point at a directory that is not its own. This is why seeding copies rows
rather than the database file.

**Both halves run inside a container**, so the host needs nothing but docker:
the source exports from the volume it already has mounted, and the target
imports through its own local HTTP API. That last part matters — the running
harness validates every write through the settings registry and re-syncs the
ones that need it, so a seeded App key is live without a restart. A write into
the SQLite file behind the process's back would skip both.

**Re-running it is safe.** A credential already set in the worktree is left
alone; `-overwrite` says otherwise. Values are never printed by either half.
Override the source with `WT_SEED_FROM=<container>`.

**The cost.** Each worktree holds a point-in-time copy, so rotating a key
means re-seeding the worktrees still alive. Worktrees are short-lived enough
that this beats the alternative, which is one shared settings store — and one
worktree's settings screen editing every other worktree's.

## Path parity

The workspace root is mounted at **the same absolute path inside the harness
container as it has on the host** — source and target of one bind mount. It
reads like a tautology and it is load-bearing.

The container shares the host's docker socket, so the `docker` a session runs
is a client of the *host's* daemon. That daemon resolves a bind mount's source
against the host's filesystem, never the client's. While the workspace root
was mounted at `/workspaces`, a session that ran `docker compose up` inside
its own clone asked the host to bind a path that exists nowhere on the host,
and compose's error named the path rather than the reason. With the two sides
equal, every path a session hands the daemon is already a host path, and
nested compose needs no translation at all.

This is why `workspaces` is a spec resource templated on `{worktree}` rather
than something under `{home}`: it has to be a real host path the daemon will
share, which on Docker Desktop means one under File Sharing.

[WORKTREES.md](WORKTREES.md) has the longer version, including the three
consequences worth knowing and the reasoning behind pointing the root
somewhere neutral.

## What stays shared

The descriptor's shared block restates this list inside the worktree itself,
so a session working there — human or agent — can see it without reading this
file.

- **The host docker socket.** Mounted into every harness container regardless
  of worktree. A session in `full` permission mode controls the one host
  daemon, and can reach every other worktree's containers through it.
  Isolating this would mean Docker-in-Docker per worktree — out of scope, and
  CLAUDE.md's docker socket rule is what governs it instead.
- **`deepseek-harness-prod`.** Fixed ports, its own project, volume and
  workspace root. Never allocated to a worktree and never torn down here.
- **`harness-unity-state`** and **`harness-unity:latest`.** The Unity CLI's
  sign-in state and download cache, and the image the `unity` shim forwards
  to, both named fixedly in `scripts/unity-shim.sh` rather than scoped to a
  compose project. Every worktree's `unity` shares one signed-in session, and
  a worktree that rebuilds the image changes what every other worktree's
  `unity` runs.
- **The DeepSeek account and the GitHub App installation.** Each worktree
  holds its own copy of the keys, but they name one account and one
  installation. Rate limits, spend and installation tokens are shared in
  reality however the keys are stored.
- **Go module cache, npm cache.** Content-addressed and read-mostly;
  isolating them would multiply setup time for no benefit.

## The band, on a new machine

The port bases are machine-local: they live in `wt`'s band ledger, not in this
repository. A clone on a fresh machine reserves them once:

```sh
wt bands reserve --base harness_http=8700 --base vite=5700
```

The production stack and the main checkout's own ports also want host-global
reservations on that machine, so nothing — this repo or another — allocates
over them:

```sh
wt bands reserve --host --port 8180 --name deepseek-harness-prod --note "the production harness stack"
wt bands reserve --host --port 8080 --port 5173 --name agent-harness --note "the main checkout, slot 0"
```

## Agent sessions inside the harness container

A `deepseek-flash-task` run clones the target repo into a directory of its own
inside the harness container. When that target is this repo, the collision
this tooling exists to prevent comes back in a different shape: the container
shares the host's docker socket, so `docker compose up` from inside an
agent's clone binds real host ports under a real host compose project name —
potentially the very stack that is running the agent.

This is the one part of the adoption not yet built. Until it is, that case is
still handled by `harness worktree init -standalone`, which allocates against
the old registry at `~/.deepseek-harness/worktrees.json`. See
[wt-decision-record.md](wt-decision-record.md), "The container-side client",
for the shape it will take and what has to be decided first.

# --- managed by wt; edits below are overwritten ---
# wt-field: app=agent-harness
# wt-field: band harness_http=8700
# wt-field: band vite=5700
# wt-field: descriptor=wt-env.json
# wt-field: resources=harness_http, vite, compose, workspaces
# wt-field: shared=the host docker socket, the deepseek-harness-prod stack, harness-unity-state (docker volume), harness-unity:latest (docker image), the DeepSeek account and the GitHub App installation, the Go module cache and the npm cache
# wt-field: worktrees=.claude/worktrees/{slug}
# --- end ---
