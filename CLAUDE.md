# agent-harness

A harness for running one coding session against a model provider's own API,
hosted for a parent process over stdin and stdout. DeepSeek is the default
provider; Gemini models are routed through the same loop, and Kimi K3 is
being added behind a narrow dialect seam. Prefer the option that exercises a
provider's real behaviour over a provider-agnostic abstraction.

There is one entry point. `harness stdio-session` hosts a single coding
session in a directory the parent owns: no queue, no worker pool, no HTTP
listener, no web UI. The parent supplies the credentials, names the working
directory on each create call, and reads the session's events off the pipe.
[ARCHITECTURE.md](ARCHITECTURE.md) maps how the packages relate and the
invariants between them, and [`internal/CLAUDE.md`](internal/CLAUDE.md) is the
codemap — one entry per Go package, loaded automatically when you work in one;
[`docs/DESIGN.md`](docs/DESIGN.md) is the reference for why any of it is
shaped that way.

## Commands

| Task | Command |
| --- | --- |
| Build | `scripts/build.sh` — gofmt, vet, the suite, then the binary |
| Test | `scripts/test.sh` |
| Format and vet | `gofmt -l cmd internal && go vet ./cmd/... ./internal/...` |
| Run one session by hand | `go run ./cmd/harness stdio-session -env .env` |

`harness help` lists the subcommand and its arguments. `gemini-session` is
the name this command was called when the only models it hosted were
Google's, and it still works: a parent that spawns the old name gets the same
process, and the handshake reports back the name it was actually spawned as.

[TESTING.md](TESTING.md) covers running a subset and what each layer of the
suite is for.

- **The protocol is the contract with a process this repo does not contain.**
  It uses the OpenAI Responses API's vocabulary rather than one of ours: the
  methods are its REST methods on `POST /responses` and the notifications are
  that surface's semantic server-sent events. `internal/stdiosession`
  speaks it to the parent and `internal/deepseek` speaks it to DeepSeek, so
  one vocabulary runs the length of the process; `internal/gemini` translates
  for the Gemini models.
  [`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
  is what a client is built from. Read it before changing anything under
  `internal/stdiosession`, because every field on it is a contract.
- **The process has a private SQLite file and that is deliberate** — the
  agent loop's state machine is its event log. It holds one session's rows and
  nothing else; nothing serves a queue from it, and nothing else reads it.
  `-state-dir` names the directory; without one the process makes a
  per-pid directory under the user cache dir and removes it on exit.
- **Credentials are the parent's to supply**, in `GEMINI_API_KEY` /
  `GOOGLE_API_KEY` and `DEEPSEEK_API_KEY`. Neither is required and neither
  implies the other: a host with one key runs that provider's models and is
  told which variable is missing if it asks for the other's. `-env` names a
  KEY=VALUE file to fall back to, for driving the process by hand. No key is
  ever written to the settings table, because a `-state-dir` the parent keeps
  for resuming must not become a file holding a plaintext key.
- **Nothing but protocol frames may reach stdout.** A stray line there is an
  unparseable frame to the parent and there is no recovering from it. The
  process logs to stderr, and it reads no `.env` of its own — the parent owns
  the working directory, and a `.env` sitting in a repository the session is
  about to work in must not feed this process.
- **`harness stdio-session` hosts every Gemini model the harness routes and
  one DeepSeek model,** `deepseek-v4-flash-vision-exp`. It refuses DeepSeek's
  other two because a model that cannot see is given vision tools that reach
  Google — so hosting one would need a second provider's key.
- **`docs/MCP.md`** is the reference for MCP client support: an operator
  registers an external MCP server and its tools join the session's array
  (`internal/mcpclient`).

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
result in wholesale.

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
- The Responses API supports all three models, and this harness speaks it:
  `harness stdio-session` posts to `/responses`.
  [`docs/DEEPSEEK-RESPONSES.md`](docs/DEEPSEEK-RESPONSES.md) is the
  reference, including what remains unverified. A session speaks one surface
  for its whole life, because the frozen prefix is what the prompt cache is
  built on.

Pricing, rate limits, and context/output limits change; read
`third_party/deepseek-docs/quick_start/pricing.md` rather than quoting numbers from
memory.

Findings measured against the live API are in
[`docs/OBSERVED.md`](docs/OBSERVED.md) and override the vendored docs where they
disagree. [`docs/VALIDATION.md`](docs/VALIDATION.md) records which design
decisions the mirror confirms and which are assumptions.
