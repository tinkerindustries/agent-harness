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
`export`, `models`, `balance`. `harness help` lists them with their arguments.

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
- **The web surface is read-only with respect to runs** — no endpoint starts,
  steers, or stops a run, and nothing a browser does can reach the loop. The
  settings endpoints (`GET /api/settings`, `PUT`/`DELETE /api/settings/{key}`,
  see docs/DESIGN.md §4.2) are the one write, and they stop at the settings
  table. Don't add another one, and don't give the HTTP server a NATS handle.
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
