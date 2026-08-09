# deepseek-harness

A harness for running DeepSeek specifically. Target DeepSeek's own API rather
than a provider-agnostic abstraction; where a choice arises, prefer the option
that exercises DeepSeek's behaviour directly.

## Shape

Work arrives on a NATS JetStream queue, runs as one of several concurrent agent
sessions in a single Go process, and returns a result to a results stream. A
request names the repositories to clone; each run gets its own directory under
`DEEPSEEK_WORKSPACE_ROOT`, named for its session id. Private clones need
`GITHUB_TOKEN`, which the container's entrypoint turns into a git credential. The
web UI is read-only: `GET` and `HEAD` only, and no endpoint starts or steers a
run. `docs/DESIGN.md` is the reference; read it before changing the request
path, the tool array, or the system prompt, all of which are cache-critical.
`docs/PROMPTING.md` covers how to word the system prompt and tool descriptions.
Thinking mode ignores the sampling parameters and rejects the coercive
`tool_choice` values, so wording is the main loop's only lever.

Skills come from the cloned repositories: `internal/skills` scans
`.claude/skills/` and `.deepcode/skills/` and lists what it finds in the opening
user message, never the system prompt (`docs/DESIGN.md` §4.11). There is no
`Skill` tool; the model opens a `SKILL.md` with `Read`.

`harness mcp` (`internal/mcp`) is a separate process, its own port, that lets
an external agent harness launch and collect runs over the same NATS streams;
it never touches the system prompt or tool array.

## Running and testing

Compose is the dev environment, running the harness, `harness mcp`, and NATS.
Rebuild with `docker compose up -d --build` after any source change: the image
bakes the frontend and the binary, so a plain `up -d` restarts the old code.
The runtime image carries the language toolchains, Playwright wired to
Alpine's own Chromium (no Firefox or WebKit on musl), and the docker CLI
against the host's socket. That socket gives a `full` session control of the
host daemon.

`internal/webassets/dist` is not in git. A plain `go build` compiles and serves
no UI; run `npm --prefix web run build` first if you need one outside compose.

Run the Go suite with `scripts/test.sh`, which starts the separate broker in
`docker-compose.test.yml`. The integration tests delete the WORK and RESULTS
streams, so they read `HARNESS_TEST_NATS_URL` and ignore `NATS_URL` — pointed
at the deployment's broker they fight its running pool.

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
