# Removing the CLI

`cmd/harness` dispatches fourteen subcommands today. Twelve of them are a
second process reaching into the same SQLite file that `serve` already owns,
duplicating a capability the browser and `/mcp` both already have. This plan
removes those twelve and leaves two.

It is a subtraction, not a migration. No `internal` package is deleted, no
schema changes, no API surface moves. Every capability being removed already
has a live replacement in the web UI, the HTTP API, or the MCP server —
except the three named under "Gaps accepted" below, which go away for good.

## Status

Steps 1 to 5 have landed on `remove-cli`.

| Step | State |
| --- | --- |
| 1 — Delete the command files | landed (`3703638`) |
| 2 — Trim `main.go` and the tests | landed (`f4edf83`) |
| 3 — Operator-facing docs and the frontend's own copy | landed (`716f411`) |
| 4 — Reference docs | landed (`7d0356f`) |
| 5 — CLI-era comments and error strings in Go source | landed (`78f1b8d`) |
| 6 — Optional: the seams that fall dead | landed |

Step 5 was not in this plan when it was written. Sweeping the Go source turned
up roughly eighty comments naming a removed command, and three of them were
not comments at all: `ErrNoAPIKey` in `internal/deepseek`, `internal/kimi` and
`internal/gemini` each told the operator to run `harness config set
<provider>.api_key`. An error message instructing somebody to run a command
that does not exist is a defect, not documentation rot, so it landed here
rather than as follow-up. The old step 5 became step 6.

## What survives

**`serve`.** It is the product: the web UI, `/api/...`, `/mcp`, and the worker
pool on one HTTP port. `Dockerfile` already ends `CMD ["serve"]`, so the
container is unaffected by any of this.

**`worktree`.** Repo development infrastructure, not an agent-facing tool: it
allocates the per-worktree ports and compose project names that let sibling
checkouts run `docker-compose.yml` concurrently
([WORKTREES.md](WORKTREES.md)). `web/vite.config.ts`, both installed worktree
skills, and `scripts/test.sh` depend on it.

## What goes

Every row's replacement is already shipped and in use.

| Command | File | Lines | Replaced by |
| --- | --- | --- | --- |
| `run` | `run.go` | 355 | `POST /api/runs`, the browser's start form, `deepseek_agent` |
| `publish` | `publish.go` | 345 | the same three — `publish` was only ever an HTTP client of `POST /api/runs` |
| `eval` | `eval.go` | 389 | `EvalListScreen` / `EvalStartForm`, `POST /api/evals` |
| `config` | `config.go` | 194 | the settings screen, `GET`/`PUT`/`DELETE /api/settings` |
| `resume`, `delete` | `session.go` | 185 | `POST /api/sessions/{id}/resume`, `DELETE /api/sessions/{id}` — but see the mirror note below |
| `steer` | `steer.go` | 100 | `POST /api/sessions/{id}/steer`, `deepseek_steer` |
| `stop` | `stop.go` | 94 | `POST /api/sessions/{id}/stop`, `deepseek_stop` |
| `export` | `export.go` | 47 | nothing — see below |
| `ask`, `models`, `balance` | `main.go` | ~300 | `GET /api/models`; balance survives only in serve's startup log |

Roughly 2,000 lines of `cmd/harness` go, plus about 300 more of helpers that
exist only to print for a terminal.

One architectural consequence is worth naming, because several documents
currently have to work around its absence: with these gone, `serve` is the
only process that opens the database. "serve stays the single writer"
([DATA-API.md](DATA-API.md), "attachments") stops being a convention the
operator can break by typing a command in another shell.

## Gaps accepted

Three capabilities have no replacement and are being given up deliberately.

1. **`harness run -workspace P`** — the only way to run the agent loop against
   a directory that already exists on disk, rather than a fresh workspace with
   repositories cloned into it. It is also the only caller that ever supplied
   `tools.Resolver`, the interactive "approve this call?" prompt.
2. **`harness export <session-id>`** — rebuilds a session's disk mirror from
   the database. The mirror is still *written* continuously by the session
   sinks; only the offline rebuild is lost. [DESIGN.md](DESIGN.md) §4.8 and
   [DATA-API.md](DATA-API.md) both cite it as the reason the mirror is
   disposable, and both need rewording.
3. **`harness ask "..."`** — a plain streaming completion with no tools, with
   a cost and cache-hit breakdown. Nothing else prints one.

A fourth was found while the work was underway, and it is the one worth
watching. `harness delete` removed the session's mirror directory
(`os.RemoveAll` after the store call) as well as its rows.
`DELETE /api/sessions/{id}` does not — it surfaces `store.DeleteSession`,
which is rows and event log only. Deleting a session from the browser
therefore leaves its directory under `<data dir>/sessions/` behind. Nothing
reads that directory back, so no behaviour breaks, but the disk no longer
tidies itself. Making the endpoint remove the directory is a change to the
HTTP surface's contract rather than a subtraction, so it is deliberately not
in this plan; [DATA-API.md](DATA-API.md) states the gap plainly instead.

## Verification

After every step, per [CLAUDE.md](../CLAUDE.md):

```sh
gofmt -l cmd internal && go vet ./cmd/... ./internal/...
scripts/test.sh
npm --prefix web run test
scripts/build.sh                # frontend, binary, container, bundle check
```

Finish on the smoke sequence in [TESTING.md](../TESTING.md), which step 3
rewrites: start a run from the browser's start form, and start a second one
through `deepseek_agent`. Both surviving ingresses have to work end to end
before this is done.

---

## Step 1 — Delete the command files

**Goal.** Eight files gone, the dispatch switch down to three arms. Nothing
else in the tree changes yet, so the build breaks only where `main.go` still
names what was deleted.

**Files.**

| File | Change |
| --- | --- |
| `cmd/harness/run.go` | delete |
| `cmd/harness/publish.go` | delete |
| `cmd/harness/session.go` | delete |
| `cmd/harness/steer.go` | delete |
| `cmd/harness/stop.go` | delete |
| `cmd/harness/export.go` | delete |
| `cmd/harness/config.go` | delete |
| `cmd/harness/eval.go` | delete |

`run.go` also carries `stringList`, `cacheHitRate`, `printResult`,
`printProgress`, `resolveMaxSubTurns` and `newInteractiveResolver`; every
caller of each is in this list except `cacheHitRate`, which `runAsk` uses and
step 2 removes. `config.go` carries `openConfigResolver` and `splitFlags`,
used only by `config`, `steer` and `stop`.

**Done when.** The eight files are gone and `go build ./cmd/...` fails only
with undefined-symbol errors pointing into `main.go`.

## Step 2 — Trim `main.go` and the tests

**Goal.** `cmd/harness` compiles again, as `serve`, `worktree` and `help`.

**Files.**

| File | Change |
| --- | --- |
| `cmd/harness/main.go` | delete `runAsk`, `runModels`, `runBalance`; delete `reasoningClaim`, `rateTierNote`, `peakWindowLine`, `explainError`, `cacheHitRate`'s last caller; cut the dispatch switch to `serve`, `worktree`, `help`; rewrite `usage` |
| `cmd/harness/claim_test.go` | delete — tests `reasoningClaim`, which only `ask` and `run` called |
| `cmd/harness/pricingline_test.go` | delete — tests `peakWindowLine` and `rateTierNote`, both `ask`-only |

**Keep, explicitly.** Everything `serve.go` composes with stays: `loadConfig`,
`openStore`, `newHTTPLogRecorder`, `closeHTTPLog`, `withHTTPLog`,
`withKimiHTTPLog`, `withGeminiHTTPLog`, `clientForModel`, `providerFor`, and
the four key/model providers. `clientformodel_test.go`,
`pricing_coverage_test.go` and `judge_test.go` all cover live code and stay
where they are — `pricing_coverage_test.go`'s own comment explains why it
lives in `cmd/harness` rather than in `internal/pricing`, and that reason is
unchanged.

The package doc on `main.go` currently reads "Command harness is a CLI for
talking to DeepSeek's native API directly." It is now a server; say so.

**Done when.** `gofmt -l cmd internal` is silent, `go vet ./cmd/...` passes,
`scripts/test.sh` is green, and `harness help` lists exactly `serve`,
`worktree` and `help`.

## Step 3 — Operator-facing docs and the frontend's own copy

**Goal.** Nothing a person is told to type still names a command that does not
exist. This step includes two strings rendered in the UI, which matter more
than the prose: they are instructions the app gives while running.

**Files.**

| File | Change |
| --- | --- |
| `web/src/components/SettingsScreen.tsx:333` | tells the reader to run `harness config get -reveal` to see a masked secret. There is no replacement — the value is write-only from here on. Reword to say the stored value is not readable back. |
| `web/src/components/EvalListScreen.tsx:95` | empty state suggests `harness eval run -suite search -variants base,search-first`. Point at the start form on this screen instead. |
| `README.md:42-60` | the settings section is built around `harness config set`/`list`/`get`. Rewrite around the settings screen and `curl` against `/api/settings` (those endpoints are not token-gated, unlike the run-control ones). |
| `README.md:89` | `docker compose exec harness harness balance` in "Check it worked" — replace with the `GET /api/queue` check already beside it, and note serve logs the balance at startup. |
| `README.md:100-126` | "Sending work" is a `harness publish` walkthrough. Rewrite around the start form and `POST /api/runs`; the paragraph at 121 already says publish is "not the only ingress", which becomes the whole story. |
| `README.md:142-152` | delete "From the terminal, without the queue" entirely — this is gap 1 and gap 3. |
| `README.md:162-163` | drop the `harness export` and `harness resume` rows; resume is the composer on a finished session ([RUN-CONTROL.md](RUN-CONTROL.md), "Continuing"). |
| `TESTING.md:154`, `:233` | the smoke sequence launches with `harness publish -repo ...`. Rewrite as the start form plus `deepseek_agent`. |
| `TESTING.md:166`, `:175` | `harness config set deepseek.api_key <key>` — becomes the settings screen, or `curl -X PUT localhost:8080/api/settings/deepseek.api_key`. |
| `RELEASE.md:109-110` | the post-deploy check runs `harness config list` and `harness balance` through `scripts/prod.sh exec`. Replace with `curl -s localhost:8180/api/settings` (secrets come back masked, same as `config list` did) — and either drop the balance check or read it out of the container logs. |
| `.env.example:4`, `:9-10`, `:42`, `:77`, `:85` | five mentions of `harness config set`/`list` in the header comments. |
| `docker-compose.yml:8` | one comment naming `harness config set deepseek.api_key`. |
| `CLAUDE.md` | the subcommand list under "Commands"; the "Skill packs" paragraph at 86-87 naming `-skill-pack` on `harness run` and `harness publish` — the browser checkbox and `deepseek_agent`'s `skill_packs` are what remain. |

**Done when.** `grep -rn 'harness \(ask\|run\|publish\|resume\|stop\|steer\|delete\|export\|models\|balance\|config\|eval\)' README.md TESTING.md RELEASE.md CLAUDE.md .env.example docker-compose.yml web/src` returns nothing, and `npm --prefix web run test` is green.

## Step 4 — Reference docs

**Goal.** The `docs/` tree and the codemaps stop describing a CLI. Prose only;
no code changes.

Live documents, all of which need edits:

| File | What to fix |
| --- | --- |
| `docs/EVALS.md` | lines 14-17 are a CLI usage block, and 112, 180-182 explain the architecture in terms of `harness eval run` posting to the endpoint. The orchestrator still lives in `serve`; only the client changes. |
| `docs/RUN-CONTROL.md` | 20 (`harness publish` as an ingress), 634, 646, 757, 785-789 — the last is a whole subsection documenting the `stop`/`steer`/`resume` commands. |
| `docs/DATA-API.md` | 68, 161-162 (`harness export` rebuilds the mirror, `harness delete` removes it), 341. |
| `docs/DESIGN.md` | 299, 310 (settings masking described via `config list`), 544, 552 (`export` as the mirror's justification), 588 (`ask`'s peak-hours line), 665-668, 691-692 (`parent_is_user` defaults differ between `run` and `publish` — with both gone, only `deepseek_agent` and the browser set it), 906. |
| `docs/TOOLS.md` | 134, 819 (`harness config set tools.bash_timeout`), 864 (`harness run` as a producer). |
| `docs/MODELS.md` | 333 — a section about what `harness models` does not do. |
| `docs/UNITY.md` | 112-113 — `-skill-pack` on `run` and `publish`. |
| `internal/CLAUDE.md` | the `cmd/harness` entry ("one file per subcommand" is no longer the shape); 137 (`session` reached by `harness resume`); 208 (`export`); 378-379 (`-skill-pack`); 460 (`harness config`). |
| `ARCHITECTURE.md` | 162 — `harness config set`. |
| `.claude/skills/prod-diagnostics/SKILL.md` | 40 and 45 name `harness config get -reveal` and `config list` among the things that hold live credentials; 251 says the mirror is "not derived — `harness export` does not produce it". Both need the new spelling. |
| `.claude/skills/prod-diagnostics/references/queries.md` | 294 — same. |

Leave alone, as historical records of what was true when written:
`docs/QUEUE-MIGRATION-PLAN.md`, `docs/RUN-CONTROL-PLAN.md`,
`docs/KIMI-INTEGRATION.md`, `docs/GEMINI-INTEGRATION.md`,
`docs/OBSERVED.md`, `docs/DSH-COMPARISON.md`, `docs/reviews/`. Their CLI
mentions are inside verification steps and measurements already taken; editing
them rewrites history rather than documentation.

**Done when.** The live documents no longer instruct anyone to run a removed
command, and `internal/CLAUDE.md`'s `cmd/harness` entry describes what the
package now is.

## Step 5 — Optional: the seams that fall dead

Separate from everything above, and worth doing separately so steps 1 and 2
stay a pure subtraction with no behaviour to reason about.

With `run` and `resume` gone, two seams have no production caller left:

- **`tools.Resolver`** (`internal/tools/policy.go:26`) — consulted for a call
  the policy would otherwise deny. Its own comment names the CLI as the
  caller. `Policy.Resolver` is nil everywhere else, so `policy.go:131` is
  dead.
- **`session.RunOptions.Progress` and `Runner.Progress`**
  (`internal/session/runner.go:146`, `:358`, and `progressFunc` at `:453`) —
  the per-sub-turn callback. Neither `internal/worker` nor `internal/evals`
  sets it; only `printProgress` did.

Both are still exercised by tests. Removing them means deleting or rewriting
those tests too, which is why this is its own step and not a tidy-up at the
end of step 2. Neither is costing anything by staying, so this can also simply
not happen.
