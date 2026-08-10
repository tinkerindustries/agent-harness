# deepseek-harness

A coding harness for DeepSeek. An agent loop that clones repositories, reads and
writes files, runs commands, and iterates until a task is done — targeting
DeepSeek's own API rather than a provider-agnostic abstraction.

Work arrives on a NATS JetStream queue, runs as one of several concurrent agent
sessions in a single Go process, and returns a result to a results stream. A
web UI shows what the sessions are doing and lets an operator set the
harness's API keys. An MCP server lets another
agent harness — Claude Code, Cursor — launch runs here and collect them later.

## What you need

- **Docker** with Compose. That is the whole story for running it; the image
  carries the Go binary, the frontend, and the language toolchains agent
  sessions use.
- **A DeepSeek API key** from <https://platform.deepseek.com/api_keys>. The
  account is prepaid and needs a balance — the harness halts the pool rather
  than failing every run when it runs out.
- **A GitHub token**, only if runs need to clone private repositories or push
  branches.

To build outside Docker you also need Go and Node; see
[Running without Docker](#running-without-docker).

## Setup

```sh
git clone https://github.com/mrgeoffrich/deepseek-harness.git
cd deepseek-harness
cp .env.example .env
```

Edit `.env` for the optional overrides — model and effort defaults, pool size,
deadlines, ports — all documented inline in `.env.example`. The DeepSeek API
key is **not** an environment variable anymore: it lives in the harness's
SQLite settings table, and is set after the stack is up:

```sh
docker compose exec harness harness config set deepseek.api_key sk-...
```

`harness config list` shows the stored keys with their values masked; `harness
config get deepseek.api_key -reveal` prints one in full.

Set `GITHUB_TOKEN` too if you want private clones. The container's entrypoint
turns it into a git credential inside the container, and `gh` picks it up from
the environment. Nothing is written to your host's git config.

Then:

```sh
docker compose up -d --build
```

Three services come up: NATS with JetStream, the harness itself, and the MCP
launch server. The harness declares its streams and consumer at startup, so
there is no setup script and an empty broker converges on its own.

### Check it worked

```sh
curl -sf localhost:8080/api/queue     # {"available":true,"halted":false}
docker compose exec harness harness balance
```

Then open <http://localhost:8080> for the session list. It will be empty until
you send some work.

Both ports bind to loopback only. Transcripts carry workspace paths, file
contents, and command output, so treat them as sensitive — and note that the
MCP port starts sessions that run commands as root inside the workspace mount.

## Sending work

```sh
docker compose exec harness harness publish \
  -repo https://github.com/org/app.git \
  -permission-mode readonly \
  "Summarise how this project handles configuration"
```

Watch it in the browser, or block until it finishes with `-wait`. Add `-repo`
more than once to clone several repositories into the same workspace, and
`URL#branch` to check out something other than `main`.

`-permission-mode` is required and is either `readonly` or `full`. Under `full`
a session can run any command, including `docker`, against the host's daemon —
the socket is mounted in. Narrow it with repeatable `-deny` patterns if you want
`full` minus something specific.

Every run gets its own directory under `workspaces/`, named for its session id,
holding that run's clones. Nothing is shared between runs.

`harness publish` is an operator's tool, not the only ingress: a NATS client in
any language can publish the same JSON body. The request and result shapes are
in [`docs/DESIGN.md`](docs/DESIGN.md) §4.10.

### From another agent harness

The MCP server at `http://127.0.0.1:8090/mcp` speaks streamable HTTP and exposes
`deepseek_agent` to launch a run, `deepseek_result` to collect it, and
`deepseek_runs` to list. Launching returns immediately; collection is safe to
retry. In Claude Code:

```sh
claude mcp add --transport http deepseek-harness http://127.0.0.1:8090/mcp
```

Set `DEEPSEEK_MCP_PERMISSION_CEILING=readonly` in `.env` to refuse `full` runs
from this server outright — they error rather than being quietly downgraded.

### From the terminal, without the queue

The same loop runs interactively against a directory you already have, and this
is the one caller that can prompt you to approve a call the policy would refuse:

```sh
docker compose exec harness harness run -workspace /workspaces/scratch "..."
```

`harness ask "..."` is a plain streaming completion with no tools, useful for
checking the key works.

## Day-to-day

| Task | Command |
| --- | --- |
| Rebuild after a source change | `docker compose up -d --build` |
| Logs | `docker compose logs -f harness` |
| Stop | `docker compose down` |
| List sessions | browse <http://localhost:8080> |
| Rebuild a session's disk mirror | `harness export <session-id>` |
| Continue a finished or timed-out session | `harness resume <session-id> ["..."]` |

`--build` matters: the image bakes the frontend and the binary, so a plain
`up -d` restarts the old code.

Shutdown drains in-flight runs rather than cutting them off, so `down` can take
up to a minute. A run that dies with its process leaves its queue message
unacked and is redelivered.

### Where state lives

- `workspaces/` — one directory per run, bind-mounted into the container. Local
  only; gitignored.
- `harness-data` volume — SQLite database and the human-readable session mirror
  under `sessions/<date>/<session-id>/`.
- `nats-data` volume — JetStream's store, so a queue backlog survives a restart.

`docker compose down -v` removes both volumes and every session with them.

## A production stack beside the dev one

`docker-compose.prod.yml` is a second, independent deployment for a machine that
does real work with the harness while the same checkout is being developed on.
Its isolation is the compose project name `deepseek-harness-prod`: separate
containers, network and volumes, on separate ports.

| | dev | prod |
| --- | --- | --- |
| Compose project | `deepseek-harness` | `deepseek-harness-prod` |
| Web UI | <http://localhost:8080> | <http://localhost:8180> |
| MCP | `http://127.0.0.1:8090/mcp` | `http://127.0.0.1:8190/mcp` |
| NATS client / monitor | 4322 / 8322 | 4522 / 8522 |
| Workspaces | `workspaces/` | `workspaces-prod/` |
| Environment | `.env` | `.env.prod` |

The difference that matters is that **prod never builds**. Both its services run
the `deepseek-harness:prod` tag, and only `promote` moves that tag, so no dev
rebuild can change what production is running:

```sh
scripts/prod.sh promote     # build this checkout, move the prod tag onto it
scripts/prod.sh deploy      # restart the stack onto the new tag
```

[RELEASE.md](RELEASE.md) is the full sequence — versioning, the checks to run
first, and how to roll back.

`promote` refuses a dirty tree unless given `-f`, and also writes an immutable
`deepseek-harness:prod-<sha>` tag, which is what `rollback` selects between:

```sh
scripts/prod.sh rollback <sha> && scripts/prod.sh deploy
```

The rest is lifecycle: `up`, `down`, `logs [service]`, `status`, and anything
else passed straight through to `docker compose`. Use the script rather than
`docker compose` directly — without `-f docker-compose.prod.yml` the command
lands on the dev stack instead.

Both stacks mount the same host docker socket and share the same DeepSeek
account, so per-model concurrency ceilings are spent between them.

## Running without Docker

You need Go and Node; the versions are in `go.mod` and the `Dockerfile`. The
frontend has to be built first, because the binary embeds it:

```sh
npm --prefix web run build
go build -o bin/harness ./cmd/harness
```

Skip that first step and the binary compiles fine and serves no UI at all.

Then point it at a broker and give it somewhere to work:

```sh
docker compose up -d nats                 # or your own NATS with JetStream
export DEEPSEEK_WORKSPACE_ROOT=$PWD/workspaces
bin/harness serve
```

Without `DEEPSEEK_WORKSPACE_ROOT` every request fails before its session starts.
For frontend work, run Vite separately and proxy to it rather than rebuilding
the embedded assets each time:

```sh
npm --prefix web run dev
bin/harness serve -dev-frontend http://127.0.0.1:5173
```

## Troubleshooting

**A port is already in use.** NATS on 4222 is a normal thing for a machine to
have. Set `NATS_CLIENT_PORT`, `NATS_MONITOR_PORT`, `HARNESS_HTTP_PORT`, or
`HARNESS_MCP_PORT` in `.env` and bring the stack back up.

**`{"halted":true}` from `/api/queue`.** The account balance ran out. The pool
stops taking work rather than burning through redeliveries; top up and restart.

**A run failed with `workspace_setup`.** The clone was refused or the branch does
not exist. Private repositories need `GITHUB_TOKEN` set before the container
started.

**A run reports `status: "ok"` but did nothing useful.** Check
`complete_status`: `gave_up` means the model finished cleanly and said it could
not do the task. Then read the transcript — a denied tool call renders as its own
block, and that is usually the answer.

**Changes do not seem to take effect.** You ran `docker compose up -d` without
`--build`.

## Documentation

- [CLAUDE.md](CLAUDE.md) — the rules an agent working in this repo has to know
- [ARCHITECTURE.md](ARCHITECTURE.md) — the packages, how they relate, and the
  invariants between them
- [TESTING.md](TESTING.md) — the suites, the broker the integration tests need,
  and the smoke sequence
- [`docs/DESIGN.md`](docs/DESIGN.md) — the technical design and why each choice
  was made; the reference for anything on the request path
- [`docs/TOOLS.md`](docs/TOOLS.md), [`docs/MODELS.md`](docs/MODELS.md),
  [`docs/PROMPTING.md`](docs/PROMPTING.md), [`docs/CACHE.md`](docs/CACHE.md) —
  the tool set, model routing, prompt wording, and the prompt cache
- [`docs/OBSERVED.md`](docs/OBSERVED.md) — findings measured against the live
  API, which override the vendored docs where they disagree
- [`third_party/deepseek-docs/`](third_party/deepseek-docs/README.md) — a
  Markdown mirror of DeepSeek's own API documentation
