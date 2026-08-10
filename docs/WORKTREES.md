# Git worktrees

Two sibling git worktrees of this repo compete for the same host ports and
the same docker compose project name the moment both run `docker compose up`
— the second one either fails to bind or, worse, quietly attaches to the
first one's containers because `docker-compose.test.yml` pins a fixed project
name. `harness worktree` gives each worktree its own slot, and everything
else in this document is that slot turned into ports and names.

There is only one production stack — `deepseek-harness-prod`, fixed on ports
8180/8190/4522/8522 (CLAUDE.md). This tooling never allocates a worktree
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
| Harness HTTP port | 8080 | 8700 + N |
| Harness MCP port | 8090 | 8800 + N |
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
  compose path, and `config.LoadDotEnv` for `harness serve`/`harness mcp`
  run directly on the host.

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
