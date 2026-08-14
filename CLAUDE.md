# agent-harness

A harness for running coding agents against a model provider's own API.
DeepSeek is the default provider and the only one implemented today; Kimi
K3 is being added behind a narrow dialect seam. Prefer the option that
exercises a provider's real behaviour over a provider-agnostic abstraction.

Work arrives on a NATS JetStream queue, runs as one of several concurrent agent
sessions in a single Go process, and returns a result to a results stream.
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
| Measure a rendered screen | `scripts/layout-metrics.sh <route>` — boxes, type, contrast, spacing; grades nothing |
| Format and vet | `gofmt -l cmd internal && go vet ./cmd/... ./internal/...` |
| Frontend dev server | `npm --prefix web run dev`, against `harness serve -dev-frontend http://127.0.0.1:5173` |
| Production stack | `scripts/prod.sh promote && scripts/prod.sh deploy` — see [RELEASE.md](RELEASE.md) and the rule below |

Subcommands: `ask`, `run`, `serve`, `publish`, `resume`, `delete`,
`export`, `models`, `balance`, `worktree`. `harness help` lists them with
their arguments.

[TESTING.md](TESTING.md) covers running a subset, the broker the integration
tests need, and the smoke sequence to finish on. [RELEASE.md](RELEASE.md) covers
cutting a version and deploying it to the production stack.

- **Build with `scripts/build.sh`.** It runs gofmt, vet, the frontend build,
  the frontend tests, the Go binary and the container, stopping at the first
  failure, and finishes by checking that the running container serves the
  bundle it just produced. `--no-docker` stops after the binary.
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

`third_party/kimi-docs/` mirrors <https://platform.kimi.ai/docs> the same way,
for Moonshot AI's Kimi models. Start at its `README.md`.

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
- The Responses API supports both models.

Pricing, rate limits, and context/output limits change; read
`third_party/deepseek-docs/quick_start/pricing.md` rather than quoting numbers from
memory.

Findings measured against the live API are in
[`docs/OBSERVED.md`](docs/OBSERVED.md) and override the vendored docs where they
disagree. [`docs/VALIDATION.md`](docs/VALIDATION.md) records which design
decisions the mirror confirms and which are assumptions.
