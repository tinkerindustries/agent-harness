# agent-harness

A harness for running coding agents against a model provider's own API.
DeepSeek is the default provider and the only one implemented today; Kimi
K3 is being added behind a narrow dialect seam. Prefer the option that
exercises a provider's real behaviour over a provider-agnostic abstraction.

Work arrives on a durable work queue (the `work_queue` table in serve's
SQLite store), runs as one of several concurrent agent sessions in a single
Go process, and records its result on the request's `work_requests` row.
[ARCHITECTURE.md](ARCHITECTURE.md) maps how the packages relate and the
invariants between them, and [`internal/CLAUDE.md`](internal/CLAUDE.md) is the
codemap — one entry per Go package, loaded automatically when you work in one;
[`docs/DESIGN.md`](docs/DESIGN.md) is the reference for why any of it is
shaped that way.

## Commands

| Task | Command |
| --- | --- |
| Build everything | `scripts/build.sh` — see the rule below |
| Logs | `docker compose logs -f harness` |
| Build the binary only | `scripts/build.sh --no-docker` |
| Test, Go | `scripts/test.sh` |
| Test, frontend | `npm --prefix web run test` |
| Format and vet | `gofmt -l cmd internal && go vet ./cmd/... ./internal/...` |
| Frontend dev server | `npm --prefix web run dev`, against `harness serve -dev-frontend http://127.0.0.1:5173` |
| Production stack | `scripts/prod.sh promote && scripts/prod.sh deploy` — see [RELEASE.md](RELEASE.md) and the rule below |

Subcommands: `serve`, `gemini-session`, `worktree`, `help`. `harness help`
lists them with their arguments.

[TESTING.md](TESTING.md) covers running a subset, the broker the integration
tests need, and the smoke sequence to finish on. [RELEASE.md](RELEASE.md) covers
cutting a version and deploying it to the production stack.

- **Build with `scripts/build.sh`.** It runs gofmt, vet, `npm ci` when
  `web/node_modules` is missing or older than the lockfile, the frontend build,
  the frontend tests, the Go binary and the container, stopping at the first
  failure, and finishes by checking that the running container serves the
  bundle it just produced. `--no-docker` stops after the binary, and a build
  that finds no docker daemon skips the container stages with a notice rather
  than failing — the last line then says the container was not built, because
  that stage is the only thing proving what a running harness serves. Pass
  `--require-docker` where the container is the point, such as a release.
  A plain `docker compose up -d --build` still works and is what the script
  runs; the reason to prefer the script is the last check. `npm run build` is
  `tsc -b && vite build`, so a typecheck failure means vite never runs — and
  until the output directory was emptied first, the previous bundle stayed on
  disk, the image baked it, and the container served an app that no longer
  matched the source, with nothing reporting an error.
- **`internal/webassets/dist` is not in git.** A plain `go build` compiles and
  serves no UI; run `npm --prefix web run build` first if you need one outside
  compose.
- **The host's docker socket is mounted into the harness container**, so a
  session in `full` permission mode has control of the host daemon. Weigh that
  before changing what a mode allows. Because that daemon resolves bind mounts
  on the host, the workspace root is mounted at the same absolute path on both
  sides — [`docs/WORKTREES.md`](docs/WORKTREES.md), "Path parity". Keep the two
  sides of that mount equal: unequal, a session cannot run `docker compose` in
  its own clone, which is most of what `scripts/build.sh` is for.
- **`docs/GITHUB-APP.md`** is the reference for reaching more than one GitHub
  account. `github.token` is one account's personal access token; a GitHub
  App (`github.app_id` and `github.app_private_key`) is installed separately
  on a personal account and an organisation and covers both, which no
  fine-grained token can. When an App is configured it wins: git is pointed
  at the `harness github-credential` helper, which mints an installation
  token per repository owner, and `gh` is given one per Bash call chosen from
  the session's own clones. Read it before touching `internal/githubauth`,
  `internal/githubapp`, or anything that spawns git.
- **`harness gemini-session` is the second entry point, and it is not a
  server.** It hosts one coding session for a parent application over stdin
  and stdout — no queue, no worker pool, no HTTP listener, no web UI — running
  `internal/session` unchanged in a directory the parent owns. The protocol is
  Google's own Interactions vocabulary rather than one of ours: the methods
  are the REST methods on `POST /v1beta/interactions` and the notifications
  are that surface's server-sent events, so `internal/gemini` speaks the
  vocabulary to Google and `internal/geministdio` speaks it to the parent.
  [`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
  is what a client is built from; read it before changing anything under
  `internal/geministdio`, because every field on it is a contract with a
  process this repo does not contain. It has a private SQLite file and that is
  deliberate — the agent loop's state machine is its event log — but nothing
  serves a queue from it.
- **`docs/MCP.md`** is the reference for the harness's own MCP *client*
  support — an operator registers an external MCP server and its tools join
  every session's array (`internal/mcpclient`). Do not confuse this with
  `internal/mcp`, the MCP *server* this harness itself exposes at `/mcp` so
  another agent harness can launch runs here — same three letters, opposite
  direction, and the two packages never meet.
- **`unity` in a session is a shim, not a binary.** The Unity CLI needs glibc
  2.34+ and refuses musl, so it cannot live on this Alpine image — it fails
  there with `no such file or directory` naming a file that exists, because
  the missing piece is the loader. It lives in its own image
  (`Dockerfile.unity`, built by `scripts/build.sh`) and
  `scripts/unity-shim.sh` forwards to it over the mounted docker socket.
  [`docs/UNITY.md`](docs/UNITY.md) is the reference, including why no Editor
  is reachable from a container and why `unity mcp` is deliberately not
  registered as an MCP server.
- **This repo uses per-worktree environments.** Every linked git worktree gets
  its own harness HTTP port, Vite port, compose project and workspace root,
  allocated by `wt init` from the spec in [`wt.yaml`](wt.yaml).
  - **Never hardcode a port or a state path.** Read them with `wt show`
    (`--json` for the whole descriptor). They differ per worktree, and a value
    that is right in one is wrong in the next.
  - **Something else creates the worktree; `wt init` attaches to it.** On this
    machine that something is Claude Code's `WorktreeCreate` hook, which runs
    `wt init` for any repository with a `wt.yaml`; by hand it is
    `git worktree add "$(wt spec path --slug <slug>)" -b <slug>` followed by
    `wt init` inside it. `init` runs the `hooks:` block in `wt.yaml` — install,
    build, start, seed, health — so it also seeds the credentials a new
    worktree needs before it can call a model or clone anything private.
    Tear one down with `wt rm --slug <slug>` from the repo root.
  - The main checkout is slot 0 and is never managed: it keeps
    `docker-compose.yml`'s and `web/vite.config.ts`'s committed defaults, so
    nothing changes for anyone who never makes a worktree.
  - [`docs/wt.md`](docs/wt.md) is the reference — the slot model, the hooks,
    the seeding, what stays shared, and the band bases to reserve on a new
    machine. [`docs/wt-decision-record.md`](docs/wt-decision-record.md) is why
    each of those is the way it is.
  - **An agent session inside the harness container has no allocator.**
    `harness worktree` and its `-standalone` flag are gone, and the container
    is not a `wt` client, so a session that brings a stack up in its own clone
    must set `COMPOSE_PROJECT_NAME`, its ports and `HARNESS_WORKSPACES`
    itself. Setting nothing makes compose fall back to the clone's directory
    basename — this repo's own dev project name — and recreate the containers
    of the stack running the session. `assets/skills/deepseek-flash-task`
    carries the instructions; `docs/wt.md` explains why nothing does it
    automatically.

## Skill packs

`assets/skill-packs/<name>` is a bundle of Agent Skills a run gets **only when
its request asked for it** — the browser's start form has a checkbox per pack,
and the MCP `deepseek_agent` tool a `skill_packs` argument; off is the default
on both. Two exist: `unity`
(sixteen of Unity Technologies' own skills, vendored, plus our note on what
the CLI cannot do without an Editor) and `blender` (ours alone — the Blender
MCP server already sends the conceptual material at initialize).

The default is off because a pack's cost is not the disk it takes: every
skill's one-line description rides in the opening message of **every request
of the run** carrying it, and the catalogue is capped at 50 entries, so an
always-on pack taxes runs that have nothing to do with it and crowds out the
repositories' own skills. `assets/agent-skills/README.md` argues that at
length and is the thing to read before making anything always-on.

Adding a pack means a directory here, a name in `internal/skills.PackNames`,
and a line in `web/src/api/operations.ts`'s `SKILL_PACKS`; a test in `assets`
fails if the first two disagree. Do not hand-edit a vendored `SKILL.md` — the
next refresh overwrites it. Ship the correction as a skill of ours in the same
pack, the way `unity-harness-notes` does.

## Vendored documentation

`third_party/deepseek-docs/` mirrors <https://api-docs.deepseek.com/> as Markdown.
Refresh it with the `deepseek-docs-refresh` skill rather than by hand. The
converter reproduces unchanged pages byte for byte, so every page the refresh
reports as changed is an upstream edit; a page that differs for no visible
upstream reason is a converter bug, and committing that churn destroys the
property for everyone after you.

`third_party/kimi-docs/` mirrors <https://platform.kimi.ai/docs> the same way,
for Moonshot AI's Kimi models. Start at its `README.md`.

`third_party/gemini-docs/` mirrors <https://ai.google.dev/gemini-api/docs/>
the same way, for Google's Gemini models — scoped to the **Interactions API**
(`POST /v1beta/interactions`), the surface [GEMINI-INTEGRATION.md](docs/GEMINI-INTEGRATION.md)
targets, not the legacy `generateContent` tree. Start at its `README.md`.

## Generated skills

`.claude/skills/playwright-cli/` is emitted by the tool it documents, not
authored here. Refresh it with `playwright-cli install --skills` and copy the
result in wholesale; the version in the image is pinned by
`PLAYWRIGHT_CLI_VERSION` in the Dockerfile.

Do not hand-edit any file under it, including the frontmatter. `playwright-cli`
compares the skill against its own copy byte for byte and prints a "does not
match the tool version" banner on *every* invocation when they differ — so a
one-line tweak costs a banner on each of the dozens of calls a browser session
makes. A stale copy is how the CLI's real commands and this skill's description
of them drifted apart before.

## API facts

- Base URL, OpenAI format: `https://api.deepseek.com`
- Base URL, Anthropic format: `https://api.deepseek.com/anthropic`
- Models: `deepseek-v4-flash`, `deepseek-v4-pro`, and
  `deepseek-v4-flash-vision-exp`. All three default to thinking mode and
  support non-thinking mode. `deepseek-v4-flash-vision-exp` is the one that
  reads images; see [`docs/DEEPSEEK-VISION.md`](docs/DEEPSEEK-VISION.md) for
  how, and for what remains unverified against the live API.
- The Responses API supports all three models. The harness posts to
  `/chat/completions` and speaks no other surface.

Pricing, rate limits, and context/output limits change; read
`third_party/deepseek-docs/quick_start/pricing.md` rather than quoting numbers from
memory.

Findings measured against the live API are in
[`docs/OBSERVED.md`](docs/OBSERVED.md) and override the vendored docs where they
disagree. [`docs/VALIDATION.md`](docs/VALIDATION.md) records which design
decisions the mirror confirms and which are assumptions.
