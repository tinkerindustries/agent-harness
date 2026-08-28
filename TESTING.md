# Testing

Two suites: Go, run through `scripts/test.sh`, and the frontend's Vitest, run
through npm. There is no CI — the suites run where you run them, which makes the
smoke sequence at the bottom of this file the only gate there is.

## Test layers

| Layer | Purpose here | Touches | Runner | Lives in |
| --- | --- | --- | --- | --- |
| Go unit | Everything that is a pure function of its inputs: folds, diffs, SSE parsing, tool-call assembly, permission decisions, pricing | Nothing external; a temp dir and a local shell at most | `go test` | Beside the code, `internal/<pkg>/*_test.go` |
| Go integration | The queue's edges against a real SQLite file — lease expiry, concurrent claim, the delivery ceiling — and the MCP server over its wire transport | A fresh SQLite file in a temp dir; `git` for the clone tests | `scripts/test.sh` | `internal/queue`, `internal/worker`, `internal/mcp` |
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

`scripts/test.sh` runs the whole Go suite — no broker, no other service, every
test on its own SQLite file in a temp dir. Flags after the script name pass
through to `go test`.

It runs `go list ./...` minus anything under a `workspaces` path rather than a
bare `./...`, because agent workspaces live inside this checkout and their Go
files are part of this module — see the smoke sequence below. Filtering the
listed packages rather than hardcoding `./cmd/... ./internal/...` is deliberate:
a hardcoded pair of roots would silently stop covering a new top-level package,
and tests that quietly do not run are worse than a build error.

### Prerequisites

- **`git` on `PATH`** — `internal/workspace` clones for real, against local
  fixture repositories it creates.
- **A POSIX shell** — the `Bash` tool tests run commands.
- No API key, no `.env`, no network.

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
- **`internal/queue`, `internal/worker`, `internal/mcp`** — integration over a
  real SQLite file in a temp dir. The queue is a table now; its edges — lease
  expiry, concurrent claim, the delivery ceiling, the crash-then-redeliver
  shape — are the things to cover, alongside the MCP server's wire transport.
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

**The queue tests are deterministic, not timed.** Lease expiry and Nak delays
are driven by an explicit `now` argument into the store, never by sleeping, so
the queue and worker suites run in seconds and do not flake under load.

**`workspaces/` holds real clones from local runs.** Each is its own Go module,
so `./...` steps over them, but they are also why the directory is gitignored —
don't let a grep or a bulk edit wander in.

## Coverage

No enforced threshold, and no coverage job. `go test -cover ./...` reports the
number if you want it.

## Smoke test after a change

`scripts/build.sh` runs steps 1 to 5 below and the container, stopping at the
first failure. Run the individual commands when you want one of them on its
own; the list is what the script does and why.

Two checks worth knowing about because nothing else catches them:

- `web/src/styles.test.ts` asserts every `var(--x)` in `styles.css` is
  defined. An undefined custom property renders as nothing — no build error,
  no console warning — and produced a run-together layout that only a
  screenshot found.
- `scripts/layout-check.sh` loads every route at 1280 and 390 in the
  container's Chromium and fails on any element whose right edge is past the
  viewport, skipping anything inside a deliberate `overflow-x: auto` scroller.
  `document.scrollWidth` is not the measure: an ancestor that clips reports no
  document scroll while a table runs 179px off the side of a phone.

Cheapest first.

1. `gofmt -l cmd internal` — no output. No linter is configured; `gofmt` and
   `go vet` are the bar. (Scoped to the source directories, because
   `workspaces/` holds clones whose formatting is not ours.)
2. `go vet ./cmd/... ./internal/...` — clean.
3. `go build ./cmd/... ./internal/...` — compiles. (A binary built this way
   serves no UI; see [`ARCHITECTURE.md`](ARCHITECTURE.md) gotchas.)

   Steps 2 and 3 name the source roots for the same reason step 1 does, and
   it is not cosmetic: any Go a run leaves in a workspace that sits inside
   this checkout joins this module. `./...` then tries to build it and fails
   on code that was never ours — today, a prod session's `scratch/` with two
   `main` declarations in it. Both roots now default outside the checkout, so
   new runs cannot add to the problem, but the workspaces written before that
   are still in `workspaces/` and `workspaces-prod/` and still shadow the
   module — keep naming the roots.
4. `scripts/test.sh` — all pass. Seconds for the unit tests, under a minute
   for the whole suite.
5. Frontend, if you touched `web/`: `npm --prefix web run build` then
   `npm --prefix web run test`. The build runs `tsc -b`, so it is the typecheck
   too.

Then, for anything on the request path, the queue path, or the HTTP surface:

```sh
docker compose up -d --build          # --build, or you restart the old code
curl -sf localhost:8080/api/queue     # the harness is up and sees its queue
```

Open `http://localhost:8080` and confirm the session list renders. For a real
change to the agent loop, start a run from the browser's start form —
permission mode `readonly` — and follow the transcript there; then start a
second one through `deepseek_agent` and collect it with `deepseek_result`, so
both surviving ingresses run at least once. That costs tokens, so it is the
check for changes that could not fail any other way — prompt wording, tool
descriptions, the fold.

## The browser pass over the session pages

This pass drives the session pages against a **live run** — a real session,
streaming over SSE, that you type into and watch respond — and it is the only
check that exercises the steer/stop flows end to end. It costs real tokens and
needs a real DeepSeek key in the harness's settings table (the settings
screen, or `curl -X PUT .../api/settings/deepseek.api_key`), so it is a
manual, occasional pass rather than part of the suites.

### Standing the stack up

1. Isolate: make a worktree and `wt init` it (`/worktree-create <slug>` does
   both), which allocates this stack its own port and compose project and
   brings it up with `--build`. The image bakes the frontend and the binary,
   so a plain `up -d` would restart the old code.
2. Set the key into the isolated stack's settings table:
   `curl -X PUT localhost:<port>/api/settings/deepseek.api_key -H
   'Content-Type: application/json' -d '{"value":"<key>"}'`, or the same
   field on that stack's settings screen. The resolver reads it per request,
   so no restart is needed.
3. The harness container is reachable at `http://<container-ip>:8080` from
   the machine driving the browser (or at the worktree's published port on
   the host). `docker inspect` for the IP, or `docker compose port harness
   8080` for the host side.

### The two traps

- **Vite's dev server does not stream SSE.** `web/vite.config.ts`'s `/api`
  proxy buffers the response, so the transcript never loads past the initial
  fetch on a page served by `npm run dev`. Drive the browser against the
  **built image** (step 1's `--build`), never the dev server.
- **A container that cannot fork.** The image runs `tini` as PID 1 so
  orphaned processes get reaped. A session's Bash calls run under a shell in
  its own process group, and any of that shell's children outliving it are
  reparented to PID 1; `harness serve` is a Go program and reaps only what it
  started, so without `tini` those orphans accumulate as zombies until the
  container runs out of PIDs. The symptom is not an obvious crash: `serve`
  keeps running while its healthcheck fails, because the check `exec`s a
  `wget` there is no room to fork, and the HTTP listener shuts down with
  `context deadline exceeded`. Count them with
  `docker exec <container> sh -c "ps -o stat | grep -c '^Z'"` — a healthy
  container sits at zero.

### The sequence

The interactive page, against a run you start from the browser:

1. Start a run from the session list's **start form** (never the CLI or MCP —
   the fork decision is `parent_is_user`, producer-stamped). Give it a task
   long enough to steer, and a permission mode you are comfortable with.
2. The form opens the session page. Confirm it is the **chat** page: plan
   rail on the right, composer along the bottom, no provenance strip.
3. Watch the first turns stream in — reasoning appearing, a tool call
   opening, output arriving — and confirm the stream stays pinned to the
   tail. Note: sub-turns commit atomically, so turns appear at batch
   granularity; only a running Bash call's stdout streams token by token.
4. Type a message into the composer and send it with Enter.
5. Confirm it appears immediately as a sent message in the **pending** state,
   and that the queued line above the composer says so.
6. Keep watching until the matching `steer_applied` arrives, and confirm the
   message flips to **delivered** and names a sub-turn.
7. Confirm the sub-turn it names is the one the run actually applied it in —
   through the API, not just the screen:
   `GET /api/sessions/{id}/events` — the `steer_applied` event (matched to
   your `steer_message` by `source_seq`) must sit after the previous
   sub-turn's tool results and before the named sub-turn's `turn_started`.
8. Scroll up mid-run: following stops and the jump pill appears; click it
   and confirm following resumes (the view glides to the tail and stays).
9. Stop the run from the page: the confirmation strip, then *stopping…*,
   then the terminal badge (CANCELLED) and the finished band. Check the
   session row too: `GET /api/sessions/{id}` must read `"status":
   "cancelled"` with a `finished_at` — a stop that leaves the row running is
   a bug.

The watch page, against a run another agent started:

1. Launch a run the way an agent would — through `deepseek_agent`, with
   `permission_mode: "readonly"`, `parent_agent_type: "claude-code"`, and
   `parent_agent_id: <id>` — so `parent_is_user` is false.
2. Open it while it is running. Confirm the watch page renders: a top nav
   whose only navigation is the back link to the session list, the
   provenance strip naming the launcher, the navigator rail on the left with
   the plan-as-phases and one tick per sub-turn, the footer saying what the
   run is doing, and **no way to send it a message** (not even a disabled
   composer).
3. Confirm the launching agent's instruction renders as its own message at
   the top of the stream: `.msg-user` reading "from claude-code · delivered
   · sub-turn 1".
4. Stop it from the page and confirm the CANCELLED terminal state.

Tear the stack down afterwards: stop any run still going, then
`wt rm --slug <slug>`.
