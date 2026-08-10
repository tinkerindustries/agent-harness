# deepseek-harness

A harness for running DeepSeek specifically. Target DeepSeek's own API rather
than a provider-agnostic abstraction; where a choice arises, prefer the option
that exercises DeepSeek's behaviour directly.

Work arrives on a NATS JetStream queue, runs as one of several concurrent agent
sessions in a single Go process, and returns a result to a results stream.
[ARCHITECTURE.md](ARCHITECTURE.md) maps the packages and the invariants between
them; [`docs/DESIGN.md`](docs/DESIGN.md) is the reference for why any of it is
shaped that way.

## Commands

| Task | Command |
| --- | --- |
| Dev loop | `docker compose up -d --build` |
| Logs | `docker compose logs -f harness` |
| Build the binary | `npm --prefix web run build && go build -o bin/harness ./cmd/harness` |
| Test, Go | `scripts/test.sh` |
| Test, frontend | `npm --prefix web run test` |
| Format and vet | `gofmt -l cmd internal && go vet ./...` |
| Frontend dev server | `npm --prefix web run dev`, against `harness serve -dev-frontend http://127.0.0.1:5173` |
| Production stack | `scripts/prod.sh promote && scripts/prod.sh deploy` — see [RELEASE.md](RELEASE.md) and the rule below |

Subcommands: `ask`, `run`, `serve`, `mcp`, `publish`, `resume`, `delete`,
`export`, `models`, `balance`, `worktree`. `harness help` lists them with
their arguments.

[TESTING.md](TESTING.md) covers running a subset, the broker the integration
tests need, and the smoke sequence to finish on. [RELEASE.md](RELEASE.md) covers
cutting a version and deploying it to the production stack.

## Rules

- **The system prompt, the tool array, and the request path are cache-critical.**
  Read `docs/DESIGN.md` §3.2 and [`docs/CACHE.md`](docs/CACHE.md) before touching
  any of them: the head of every request is frozen and shared, and the prompt
  cache is worth 50–120× on input tokens.
  [`docs/PROMPTING.md`](docs/PROMPTING.md) covers how to word the system prompt
  and tool descriptions. Thinking mode ignores the sampling parameters and
  rejects the coercive `tool_choice` values, so wording is the main loop's only
  lever.
- **The HTTP API is becoming the harness's real interface, in stages.** The
  read-only rule is retired. Stage one is the data the harness manages —
  sessions, events, work requests, settings — and it is specified in
  docs/DATA-API.md. Stage two is run control from the browser, built in stages
  (docs/RUN-CONTROL.md): stopping (`POST /api/sessions/{id}/stop`, through the
  declared `RunController` seam), steering (`POST /api/sessions/{id}/steer`,
  a store write the loop reads), and starting (`POST /api/runs`, through the
  declared `RunPublisher` seam) are built.
  **The HTTP server holds no JetStream handle; it holds two narrow run-control
  seams, `RunPublisher` and `RunController`.** `RunPublisher` (declared in
  `internal/httpapi`, implemented by `cmd/harness` over the queue's own
  handle) publishes one validated `queue.Request` to the WORK stream — the
  browser is one more producer, not a second way a session starts, and the
  claim/heartbeat/redelivery machinery stays the only one. The seam was
  chosen deliberately, not by an import appearing, which is what the old rule
  ("do not give the HTTP server a NATS handle") was protecting: this is its
  record. Build stage one so stage two is an addition rather than a rewrite.
  Every write carries the guards the settings endpoints already use — same
  origin, `application/json`, loopback. Authentication is the open question
  stage two forces: the port serves transcripts carrying workspace paths, file
  contents, and command output, and loopback stops being sufficient the moment
  the surface is something a person leaves open.
- **A production stack runs on this machine and must not be disturbed.** It is
  the `deepseek-harness-prod` compose project from `docker-compose.prod.yml`,
  on ports 8180 / 8190 / 4522, and it is very likely mid-run. A bare
  `docker compose ...` in this directory only ever touches the dev project, so
  keep it that way: never pass `-f docker-compose.prod.yml`, never
  `docker rmi`/`docker tag` `deepseek-harness:prod`, and leave `.env.prod` and
  `workspaces-prod/` alone. Promotion is a deliberate act by the operator —
  see the README's "A production stack beside the dev one".
- **Rebuild with `docker compose up -d --build`** after any source change. The
  image bakes the frontend and the binary, so a plain `up -d` restarts the old
  code.
- **`internal/webassets/dist` is not in git.** A plain `go build` compiles and
  serves no UI; run `npm --prefix web run build` first if you need one outside
  compose.
- **The host's docker socket is mounted into the harness container**, so a
  session in `full` permission mode has control of the host daemon. Weigh that
  before changing what a mode allows.
- **Sibling git worktrees each get their own ports and compose project**,
  allocated by `harness worktree init` and torn down by `harness worktree rm`.
  Create one with `/worktree-create <slug>`, remove one with
  `/worktree-remove <slug>` — the two installed skills get the ordering right.
  [`docs/WORKTREES.md`](docs/WORKTREES.md) is the reference: the slot model,
  the port bands, what's still shared, and why `web/vite.config.ts` and
  `scripts/test.sh` are the only two files that needed a code change to
  become worktree-aware.

## Vendored documentation

`third_party/deepseek-docs/` mirrors <https://api-docs.deepseek.com/> as Markdown.
Start at `third_party/deepseek-docs/README.md` for the index. Consult it before
answering questions about DeepSeek's API surface.

The mirror is generated, not authored — do not hand-edit the files. To refresh,
re-scrape the site; response schemas under `api/` render client-side and need a
headless browser to capture.

Files with an `.en.` in the name are our English translations rather than
upstream content. DeepSeek's prompt library is published in Chinese only;
`_data/prompts.en.json` mirrors its structure and works as a drop-in substitute.

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
- Models: `deepseek-v4-flash` and `deepseek-v4-pro`. Both default to thinking
  mode and support non-thinking mode.
- The Responses API supports `deepseek-v4-flash` only.

Pricing, rate limits, and context/output limits change; read
`third_party/deepseek-docs/quick_start/pricing.md` rather than quoting numbers from
memory.

Findings measured against the live API are in
[`docs/OBSERVED.md`](docs/OBSERVED.md) and override the vendored docs where they
disagree. [`docs/VALIDATION.md`](docs/VALIDATION.md) records which design
decisions the mirror confirms and which are assumptions.
