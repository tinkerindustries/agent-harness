# Git worktrees

Two sibling git worktrees of this repo compete for the same host ports and
the same docker compose project name the moment both run `docker compose up`
— the second one either fails to bind or, worse, quietly attaches to the
first one's containers because `docker-compose.test.yml` pins a fixed project
name. `harness worktree` gives each worktree its own slot, and everything
else in this document is that slot turned into ports and names.

There is only one production stack — `deepseek-harness-prod`, fixed on ports
8180/4522/8522 (CLAUDE.md). This tooling never allocates a worktree
onto those ports and never touches that stack.

## The workflow

Normal use goes through the two installed skills, which get the ordering
right so you don't have to remember it:

- `/worktree-create <slug>` — creates the git worktree, then runs
  `harness worktree init` inside it.
- `/worktree-remove <slug>` — runs `harness worktree rm <slug>` first, then
  `git worktree remove`.

The raw commands, if you're not going through the skills:

```
git worktree add .claude/worktrees/<slug> -b <branch>
cd .claude/worktrees/<slug>
harness worktree init
docker compose up -d --build      # reads the .env init just wrote
```

```
harness worktree rm <slug>        # from the repo root, before removing the tree
git worktree remove .claude/worktrees/<slug>
```

`harness worktree list` shows every registered worktree and its ports.
`harness worktree doctor` reports drift: a registry entry whose directory is
gone, a worktree directory with no registry entry, a descriptor that didn't
survive.

## The slot model

The main checkout is slot 0 and is never touched by this tool — it keeps
`docker-compose.yml`'s and `docker-compose.test.yml`'s own defaults, so
nothing changes for anyone who never creates a worktree. `harness worktree
init`, run from inside a linked worktree, allocates the lowest free slot in
1..32 and derives every port from it:

| Resource | Slot 0 (main, unmanaged) | Worktree slot N |
| --- | --- | --- |
| NATS client port | 4222 | 4600 + N |
| NATS monitor port | 8222 | 8600 + N |
| Harness HTTP port (web UI, /api/..., and /mcp) | 8080 | 8700 + N |
| Test broker port | 4422 | 4700 + N |
| Vite dev server port | 5173 | 5700 + N |
| Dev compose project | `deepseek-harness` (directory basename) | `deepseek-harness-<slug>` |
| Test compose project | `deepseek-harness-test` (fixed in the file) | `deepseek-harness-<slug>-test` |

The last two digits of every allocated port equal the slot, so slot 7's ports
all end in `07` — readable straight out of `docker ps` or `lsof`.

## What `init` writes

- **`.worktree-env.xml`** at the worktree root — the machine-readable record
  of the allocation: identity, compose project names, every port, and the
  list of resources this tool does *not* isolate (below). Gitignored;
  per-worktree, per-machine state, never committed.
- **`.env`**, in a block between `# --- harness worktree env: managed by
  ... ---` markers. A first run seeds the file from the main checkout's
  `.env` — so `GITHUB_TOKEN` and any DeepSeek credentials carry over — then
  appends the block; a re-run replaces only that block, leaving the rest
  (including hand edits) alone. Everything below it flows from `.env`
  through the normal channels: `docker-compose.yml`'s and
  `docker-compose.test.yml`'s `${VAR:-default}` substitutions for the
  compose path, and `config.LoadDotEnv` for `harness serve` (which also
  mounts /mcp on that same address) run directly on the host.

Nothing else needed a code change to become worktree-aware **except**:

- `web/vite.config.ts` — its `/api` proxy target was hardcoded to
  `127.0.0.1:8080`. It now reads `HARNESS_HTTP_PORT` and `HARNESS_VITE_PORT`
  from the repo root `.env` via Vite's `loadEnv`, falling back to today's
  8080/5173 when neither is set.
- `scripts/test.sh` — `docker-compose.test.yml` pins `name:
  deepseek-harness-test`, which a bare `${COMPOSE_PROJECT_NAME}` override
  would collide with `docker-compose.yml`'s own project if reused directly
  (they'd become the *same* compose project fighting over one `nats`
  service). The script instead derives `${COMPOSE_PROJECT_NAME}-test` and
  passes it explicitly via `-p`, which always wins over the file's `name:`.

Everything else — `HARNESS_WORKSPACES`'s bind mount, the `harness-data` and
`nats-data` named volumes, the on-host `DEEPSEEK_DATA_DIR` default — was
already relative to the working directory or namespaced by the compose
project, so it isolates for free once the project name does.

## Path parity

The workspace root is mounted at **the same absolute path inside the harness
container as it has on the host** — source and target of one bind mount, both
`${HARNESS_WORKSPACES:-${PWD}/workspaces}`. It reads like a tautology and it
is load-bearing.

The container shares the host's docker socket, so the `docker` a session runs
is a client of the *host's* daemon. That daemon resolves a bind mount's source
against the host's filesystem, never the client's. While the workspace root
was mounted at `/workspaces`, a session that ran `docker compose up` inside
its own clone asked the host to bind
`/workspaces/sess-…/<repo>/workspaces` — a path that exists nowhere on the
host:

```
mounts denied: the path /workspaces/sess-…/deepseek-harness/workspaces
is not shared from the host and is not known to Docker.
```

That is what stopped a delegated run from completing this repo's own
`scripts/build.sh`, whose last and most valuable stage is a container check
(CLAUDE.md). It was recorded twice as an environment limitation before it was
recognised as a fixable one. With the two paths equal, every path a session
hands the daemon is already a host path, and nested compose — this repo's or
any other's, relative bind mounts included — needs no translation at all.

Three consequences worth knowing:

- **`${PWD}` is the shell's directory, not the project's.** Compose resolves a
  *relative* bind source against the project directory but interpolates
  `${PWD}` from the environment, so running compose from a subdirectory would
  mount the wrong host path. `scripts/build.sh` exports `HARNESS_WORKSPACES`
  from the repository root, and `harness worktree init` pins it absolutely in
  each worktree's `.env`, so only a hand-typed `docker compose` from a
  subdirectory can get it wrong.
- **The registry is passed by path, not inherited.** `${HOME}` inside the
  container is `/root`, so a nested compose interpolating it would ask the
  host for `/root/.deepseek-harness`. `HARNESS_REGISTRY_DIR` carries the
  host's own path into the container's environment for the nested mount to
  name.
- **The path must be one the daemon will share.** On Docker Desktop that
  means under a directory in File Sharing — `/Users/...` by default, which is
  where a checkout normally lives. `scripts/build.sh` probes this before
  running compose when it detects it is inside a container, because compose's
  own error names a path rather than the reason.

## What stays shared

`.worktree-env.xml`'s `<shared>` block restates this list inside the
worktree itself, so a session working there — human or agent — can see it
without reading this file:

- **The host docker socket.** Mounted into every harness container
  regardless of worktree; a session in `full` permission mode controls the
  one host daemon (CLAUDE.md's docker socket rule). Isolating this is out of
  scope — it would mean a Docker-in-Docker setup for every worktree.
- **`deepseek-harness-prod`.** Fixed ports, never allocated to a worktree,
  never touched by `harness worktree`.
- **`GITHUB_TOKEN` and the DeepSeek API key.** Copied into a new worktree's
  `.env` from the main checkout at `init` time. Same account either way —
  safe to use concurrently.
- **Go module cache, npm cache.** Content-addressed and read-mostly;
  isolating them would multiply setup time for no benefit.

## The registry

`harness worktree` keeps one host-global file, `~/.deepseek-harness/
worktrees.json`, tracking every worktree that has run `init` — keyed by
slug (the worktree directory's name, not the branch, which gets renamed and
deleted). It's host-global rather than repo-local because its whole job is
coordinating *across* worktrees, which a file living inside one of them
can't do. Reads and writes go through an exclusive-create lockfile with a
15s stale-lock timeout, and every write is a temp-file-plus-rename, so two
`init` calls launched at once cannot both claim the same slot and a crash
mid-write cannot truncate the file.

`harness worktree rm <slug>` works from the registry alone — it tears down
by docker compose project label (`com.docker.compose.project`), never by
reading a compose file, so it still works after `git worktree remove` has
already deleted the directory. Run it before that removal, not after; the
installed `/worktree-remove` skill does both in the right order.

## Agent workspaces (`-standalone`)

A `deepseek-flash-task` run clones the target repo into a directory of its own
inside the harness container. When that target is this repo, the same
collision `harness worktree` was built to prevent is back in a different
shape: the container shares the host's docker socket
(docker-compose.yml's `harness` service), so `docker compose up` from inside
an agent's clone binds real host ports under a real host compose project name
— potentially the dev (or prod) deployment's own, since that's the stack
currently running the agent.

Plain `harness worktree init` refuses to help here: a fresh `git clone` has no
linked-worktree relationship to anything, so `--git-dir` and
`--git-common-dir` are always equal and it looks exactly like the main
checkout (slot 0). `-standalone` is the escape hatch — it skips that check and
allocates a slot for the clone as if it were a linked worktree:

```
harness worktree init -slug <unique-slug> -standalone
docker compose up -d --build      # or scripts/test.sh — both read the .env just written
harness worktree rm <unique-slug> # unconditionally, before finishing
```

Two things had to be true before that sequence actually worked, and both are
worth knowing because neither is visible from inside a session:

**The slug has to reach compose.** `docker-compose.yml` loads `.env` wholesale
into the container, so a worktree's own `COMPOSE_PROJECT_NAME` and ports
became environment variables of every session running there — and compose
prefers the environment to a `.env` file. A session's `init -standalone` wrote
itself a correct `.env` that its own `docker compose up` then ignored,
inheriting the parent's project name and recreating the parent's containers:
the harness that was running the session, restarted by the session.
`scripts/docker-entrypoint.sh` now unsets those keys before the harness
process starts, so every session inherits an environment without them and its
own `.env` is read. Unsetting is the only thing that works — a variable set to
the empty string still shadows `.env`, and merely falls back to the compose
file's directory name instead, which is how the same bug came back pointed at
a different stack. `HARNESS_REGISTRY_DIR` is the one key a session inherits on
purpose.

The unset happens in the entrypoint rather than the compose file, which means
a shell from `docker compose exec` — a human debugging, not a session — still
carries them and should pass `-p` to compose, the way `scripts/test.sh` does.

**The bind mounts have to resolve**, which they do because the session's
workspace has the same path inside the container as it does on the host — see
[Path parity](#path-parity) above. It is also why the registry this `init`
writes to is the host's own: the file is shared into the container, and
`HARNESS_REGISTRY_DIR` carries its host path for the nested compose to mount.
Both stacks share it, so a session cannot be handed a slot a host worktree is
already holding.

Never pass `-standalone` in your own primary checkout of this repo — git
cannot distinguish "the real main checkout" from "a disposable clone" on its
own, so the flag is the only thing making that call, and it trusts you to mean
it.

Two things make this work without touching `init`'s allocation logic at all:

- **The registry is genuinely shared.** `docker-compose.yml` mounts the host's
  `$HOME/.deepseek-harness` into the container at `/root/.deepseek-harness`
  (the container runs as root, so that's where `os.UserHomeDir()` resolves).
  `harness worktree init/list/rm/doctor` inside a session read and write the
  exact same file the host's own worktrees do — so a slot an agent allocates
  is genuinely unavailable to a concurrent host worktree, and vice versa.
- **Nothing downstream needed a standalone-specific code path.** `MainRoot()`
  for a clone with no linked-worktree sibling simply resolves to the clone's
  own root, so `UpsertEnv`'s "seed from the main checkout's `.env`" step finds
  no file there and seeds nothing — correct, since an agent's `GITHUB_TOKEN`
  and DeepSeek credentials arrive some other way, not through a worktree
  `.env`. `harness worktree rm` was already registry-and-docker-label-only
  with no git dependency, so it needs no changes either.

`AllocateSlot`'s live port probe (`ports.probeFree()`) binds on the
container's own loopback, which is a different network namespace than the
host ports `docker compose up` actually publishes to via the shared daemon —
so from inside a session that probe cannot catch a collision the way it can
on the host. The registry check is what actually prevents collisions here;
the probe is a bonus that happens not to fire in this environment.
