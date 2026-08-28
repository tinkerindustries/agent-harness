# Path parity

This file is named for `harness worktree`, the per-worktree allocator this
repo used to carry. That tool is gone — [wt.md](wt.md) is the reference for
per-worktree environments now, and [wt-decision-record.md](wt-decision-record.md)
records why the replacement is shaped the way it is.

What survives here is the one thing that was never about the allocator: the
rule that the workspace root is mounted at the same absolute path on both
sides of its bind mount. That is a property of `docker-compose.yml` and it
governs every session that runs docker against the shared host socket, so it
would be true with no worktree tooling at all.

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
(CLAUDE.md). With the two paths equal, every path a session
hands the daemon is already a host path, and nested compose — this repo's or
any other's, relative bind mounts included — needs no translation at all.

Three consequences worth knowing:

- **`${PWD}` is the shell's directory, not the project's.** Compose resolves a
  *relative* bind source against the project directory but interpolates
  `${PWD}` from the environment, so running compose from a subdirectory would
  mount the wrong host path. `scripts/build.sh` exports `HARNESS_WORKSPACES`
  from the repository root, and `wt init` pins it absolutely in each
  worktree's `.env`, so only a hand-typed `docker compose` from a
  subdirectory can get it wrong. `build.sh` reads the value out of `.env`
  first: an exported variable beats `.env` in compose's interpolation, so
  without that read the export would silently replace a root pinned there on
  every build.
- **The path must be one the daemon will share.** On Docker Desktop that
  means under a directory in File Sharing — `/Users/...` by default, which is
  where a checkout normally lives. `scripts/build.sh` probes this before
  running compose when it detects it is inside a container, because compose's
  own error names a path rather than the reason.

Parity constrains the two sides to be equal; it does not constrain *which*
path they are. The root is `HARNESS_WORKSPACES`, and pointing it somewhere
neutral — `/Users/Shared/harness-workspaces` rather than the checkout's own
`workspaces/` — keeps the operator's home directory and the checkout's name
out of every path a session prints, records, and hands the model, at no cost
to any of the above. It has to stay a path the host daemon can resolve and
share, so this is a relocation rather than an alias: a container-only path
is what parity exists to rule out. Sessions already recorded under the old
root keep pointing at it, which is why the move is a change of root for new
runs rather than a migration of old ones.
