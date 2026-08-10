# Testing

Two suites: Go, run through `scripts/test.sh`, and the frontend's Vitest, run
through npm. There is no CI — the suites run where you run them, which makes the
smoke sequence at the bottom of this file the only gate there is.

## Test layers

| Layer | Purpose here | Touches | Runner | Lives in |
| --- | --- | --- | --- | --- |
| Go unit | Everything that is a pure function of its inputs: folds, diffs, SSE parsing, tool-call assembly, permission decisions, pricing | Nothing external; a temp dir and a local shell at most | `go test` | Beside the code, `internal/<pkg>/*_test.go` |
| Go integration | JetStream behaviour that only a real broker exhibits — stream convergence, ack discipline, redelivery, the MCP server over its wire transport | A NATS server from `docker-compose.test.yml` | `scripts/test.sh` | `internal/queue`, `internal/worker`, `internal/mcp` |
| Frontend unit | The browser-side fold and the display helpers around it | Nothing; no DOM harness | `vitest` | Beside the code, `web/src/**/*.test.ts` |

Nothing in either suite calls `api.deepseek.com`. Findings that needed the live
API were measured by hand and written down in
[`docs/OBSERVED.md`](docs/OBSERVED.md) rather than turned into tests.

There is no end-to-end layer. A full run costs minutes and real money, so the
equivalent is the manual smoke check below.

## Running tests

| What | Command |
| --- | --- |
| Everything, Go | `scripts/test.sh` |
| One package | `go test ./internal/session/...` |
| One test | `go test ./internal/session -run TestResumeContinuesSubTurnNumbering -v` |
| Go, with the race detector | `scripts/test.sh -race` |
| Everything, frontend | `npm --prefix web run test` |
| One frontend file | `npm --prefix web run test -- src/api/fold.test.ts` |
| Frontend watch mode | `npm --prefix web exec -- vitest` |

`scripts/test.sh` brings the test broker up, runs `go test ./...`, and takes the
broker down again however the run ends. Flags after the script name pass through
to `go test`.

To iterate on one package without paying the broker's start-up on every run,
start it yourself:

```sh
docker compose -f docker-compose.test.yml up -d --wait
go test ./internal/queue/...
docker compose -f docker-compose.test.yml down
```

### The broker the integration tests use is not the one the harness uses

This is the trap. Those tests delete the WORK and RESULTS streams in their
cleanup and share the `harness-workers` durable, so pointed at the stack in
`docker-compose.yml` they fight the running pool: it consumes their requests and
rejects them against its own workspace roots.

So they deliberately ignore `NATS_URL` and read `HARNESS_TEST_NATS_URL`, which
defaults to the loopback port `docker-compose.test.yml` publishes. Override
`HARNESS_TEST_NATS_PORT` and `HARNESS_TEST_NATS_URL` together or the suite and
the broker it starts disagree on the port. The test stack has its own compose
project name and keeps its JetStream store in tmpfs, so no run inherits messages
from the last.

### Prerequisites

- **Docker**, for the test broker. Nothing else in the Go suite needs it.
- **`git` on `PATH`** — `internal/workspace` clones for real, against local
  fixture repositories it creates.
- **A POSIX shell** — the `Bash` tool tests run commands.
- No API key, no `.env`, no network. The suite reads the repo's `.env` only to
  pick up a `HARNESS_TEST_NATS_*` override.

## What to test where

- **`internal/deepseek`** — the parsing and assembly edges that a generic
  OpenAI-compatible client gets wrong: oversized SSE lines, `:` comments,
  `[DONE]`, tool-call `arguments` fragmenting mid-token, retry classification.
  Don't mock the whole API to assert the harness sends what it sends; the
  request-shape rules are asserted in `internal/session`'s prefix tests where
  they matter.
- **`internal/session`** — the invariants: prefix stability, tool-result
  ordering, resume replaying an event log to the same messages. These are the
  tests that fail when someone perturbs the cached head, and the reason to write
  a new one is that a change touched the request path.
- **`internal/tools`** — argument validation, workspace confinement, and every
  permission decision in both modes. Policy tests also assert that the tool
  *definitions* are unchanged by mode, which is the cache invariant in test form.
- **`internal/fold`, `internal/store`** — pure transforms with obvious inputs and
  outputs; cheap, so cover the edges.
- **`internal/queue`, `internal/worker`, `internal/mcp`** — integration only.
  Assert against a real broker: nothing about JetStream's ack, redelivery, or
  convergence behaviour is worth a mock.
- **`web/src/api/fold.ts`** — the browser fold, which has to stay in shape
  agreement with the Go one. Test the event kinds, not the React tree.
- **Don't bother** with the React components. There is no DOM test harness and
  the frontend's risk is frame budget, not logic — `web/src/perf` is the
  instrument for that, driven by hand against a synthetic feed.

## Conventions

- Tests sit beside the code, `package foo` rather than `foo_test`, and reach
  into unexported identifiers freely.
- Table-driven where there is a table; a named `t.Run` per case.
- Standard library only. No assertion library, no mocking framework — collaborators
  are small interfaces satisfied by a struct declared in the test file.
- Comments on the non-obvious tests name the design section or measurement they
  are pinning. Keep that up: it is what tells the next person whether a failure
  means the code broke or the requirement changed.

## Known-awkward tests

**Integration tests fail, they do not skip, when the broker is missing.** With
no broker reachable the `queue`, `worker` and `mcp` suites call `t.Fatalf`
naming the URL they tried and pointing at `scripts/test.sh`, so `go test ./...`
never reads as a pass while the queue path went untested. The deliberate
opt-out is `HARNESS_TEST_NATS_OPTIONAL=1`, which restores the old skip for a
developer who genuinely has no Docker; `scripts/test.sh` never sets it, so its
own broker being unreachable is loud.

**`workspaces/` holds real clones from local runs.** Each is its own Go module,
so `./...` steps over them, but they are also why the directory is gitignored —
don't let a grep or a bulk edit wander in.

## Coverage

No enforced threshold, and no coverage job. `go test -cover ./...` reports the
number if you want it.

## Smoke test after a change

Cheapest first.

1. `gofmt -l cmd internal` — no output. No linter is configured; `gofmt` and
   `go vet` are the bar. (Scoped to the source directories, because
   `workspaces/` holds clones whose formatting is not ours.)
2. `go vet ./...` — clean.
3. `go build ./...` — compiles. (A binary built this way serves no UI; see
   [`ARCHITECTURE.md`](ARCHITECTURE.md) gotchas.)
4. `scripts/test.sh` — all pass. Seconds for the unit tests, under a minute
   including the broker's start-up.
5. Frontend, if you touched `web/`: `npm --prefix web run build` then
   `npm --prefix web run test`. The build runs `tsc -b`, so it is the typecheck
   too.

Then, for anything on the request path, the queue path, or the HTTP surface:

```sh
docker compose up -d --build          # --build, or you restart the old code
curl -sf localhost:8080/api/queue     # the harness is up and sees its streams
```

Open `http://localhost:8080` and confirm the session list renders. For a real
change to the agent loop, publish a run and watch it: `harness publish -repo
<url> -permission-mode readonly "..."`, then follow the transcript in the
browser. That costs
tokens, so it is the check for changes that could not fail any other way —
prompt wording, tool descriptions, the fold.
