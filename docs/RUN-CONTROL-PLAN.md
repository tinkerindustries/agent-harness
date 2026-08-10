# Run control: implementation plan

The staged build order for [RUN-CONTROL.md](RUN-CONTROL.md). That document is
the design and the reference for *why*; this one is the sequence, the files,
and what has to be true before each step is done.

Six steps. Each one ships and leaves the harness working; nothing here needs a
big-bang cutover. Steps 1 and 2 have no external surface at all and are worth
doing whatever happens to the rest. Steps 4 to 6 each land backend, MCP, CLI
and web together, so a step is "this capability works end to end", never "the
backend half of it".

Verification after every step, per [CLAUDE.md](../CLAUDE.md):

```sh
gofmt -l cmd internal && go vet ./...
scripts/test.sh                 # brings the test broker up and down
npm --prefix web run test       # from step 4 on
docker compose up -d --build    # --build, or it restarts the old code
```

---

## Step 1 — `Bash` survives a wedged child

**Goal.** `tools.bash_timeout` actually terminates a command that backgrounds
a process without redirecting its output, and the orphan dies with it. No API
surface; a correctness fix that stands alone.

**Files.**

| File | Change |
| --- | --- |
| `internal/tools/bash.go` | `WaitDelay`, mutex-guarded output buffer, `ErrWaitDelay` result |
| `internal/tools/bash_unix.go` (new, `//go:build unix`) | `Setpgid`, group-kill `Cancel` |
| `internal/tools/bash_other.go` (new, `//go:build !unix`) | no-op group hooks |
| `internal/tools/registry.go` | resolve `tools.bash_wait_delay` in `Executor` |
| `internal/settings/registry.go` | the new setting, `GroupToolLimits` |
| `internal/tools/bash_test.go` | the regression test |
| `internal/settings/registry_test.go` | pin the default against the package constant |
| `docs/TOOLS.md` | what a wedged command now does and what the result says |

**Changes.**

1. `cmd.WaitDelay = e.bashWaitDelay(ctx)` — new setting
   `tools.bash_wait_delay`, duration, default `2s`, bounds `100ms`–`5m`. It
   bounds two waits, not one: after `ctx` is cancelled, and after the process
   exits with its pipes still held by a grandchild. The second is the common
   shape of the production hang and the reason this is not merely a
   timeout-enforcement fix.
2. Build the child in its own process group and override `cmd.Cancel` to
   signal the group (`-pgid`): SIGTERM, then SIGKILL after the wait delay.
   Split into `bash_unix.go` / `bash_other.go` so `go vet ./...` still passes
   on a platform without `Setpgid`; the harness runs Linux in the container
   and macOS on the desk, and both are `unix`.
3. Kill the group again on the `ErrWaitDelay` path, which has no cancellation
   behind it. Without this the orphan survives the tool call, keeps whatever
   port it bound, and breaks the next run.
4. Wrap the captured `bytes.Buffer` in a small mutex-guarded writer. `Wait`
   can now return while a copy goroutine is still writing; the post-`Wait`
   read of that buffer is otherwise a race the detector will find.
5. `errors.Is(runErr, exec.ErrWaitDelay)` gets its own `Result`: an error
   result naming the mistake and the fix (redirect and detach), per
   [PROMPTING.md](PROMPTING.md) — the model is the one who has to route around
   it.

**Tests.**

- `TestBashBackgroundedProcessDoesNotHang` — reproduce
  `sess-23f440713ef783c1484eb3eb0be24969`'s exact shape: a command that
  backgrounds a child holding stdout, e.g.
  `sh -c 'sleep 300 & echo started'`. Assert the call returns within
  `WaitDelay + slack` (not the tool timeout), and that the result names the
  cause.
- `TestBashKillsProcessGroup` — the backgrounded pid is gone after the call.
  Use a marker the test can poll (a pidfile the child writes, then
  `syscall.Kill(pid, 0)`), not a port, so the test needs nothing bindable.
- `TestBashOrdinaryCommandUnchanged` — exit codes, truncation, and streamed
  stdout behave exactly as before. `WaitDelay` must not change the ordinary
  path.
- Run the package under `-race`; points 4 and 1 interact.

**Exit.** The regression test passes, the orphan is confirmed dead, and
`scripts/test.sh -race ./internal/tools/...` is clean.

**Risk.** Low, and scoped to one function. The one thing to watch is a
legitimate long-running foreground command whose output pauses — `WaitDelay`
does not touch it, because the process has not exited and `ctx` is not done.

---

## Step 2 — Store, event, and settings foundations

**Goal.** Everything the later steps write against exists and is tested, with
no external surface. Independently testable inside `internal/store` and
`internal/fold`.

**Files.**

| File | Change |
| --- | --- |
| `internal/store/store.go` | `ErrSessionCancelled`, append fence, terminal-status fence, `CancelRunningSession` |
| `internal/store/events.go` | `KindSteerMessage`, `KindSteerApplied`, both payloads, both added to `EventKinds` |
| `internal/store/store.go` | `SteerMessagesAfter(ctx, sessionID, afterSeq, limit)`, `LastAppliedSteerSeq(ctx, sessionID)` |
| `internal/fold/fold.go` | one case: `steer_applied` → user message; `steer_message` → nothing |
| `internal/queue/result.go` | `StatusCancelled` |
| `internal/settings/registry.go` | `run.stop_grace_period`, `http.control_token` |
| tests beside each | below |

**Changes.**

1. **Fences.** `AppendEvents` refuses a session whose status is `cancelled`
   with `ErrSessionCancelled`. `FinishSession` and `UpdateSessionStatus`
   refuse to move a row out of `cancelled`. Both inside the store's write
   transaction, like every other precondition (DATA-API.md, "Preconditions").
   This is what makes a wedged goroutine that later wakes unable to dirty a
   stopped session's log — a second, independent stop.
2. **`CancelRunningSession(ctx, id, now)`** — sets `cancelled`, sets
   `finished_at`, bumps `version`, and does **not** take the idle
   precondition. `CloseSession`'s idle check exists to stop an operator
   closing a live row; this writer *is* the run's owner and the row being live
   is the point. A distinct method rather than `CloseSession(..., minIdle: 0)`
   so the intent is readable at the call site.
3. **Two event kinds**, with the payloads in RUN-CONTROL.md. Adding them to
   `EventKinds` extends the events endpoint's `?kind=` filter and its 400
   message for free — that list is derived, never hand-written.
4. **The fold.** `steer_applied` appends a user message; `steer_message` joins
   `tool_stdout` in the "carries no messages-array content" case. The
   asymmetry is the design's core, and the comment at that case should say so
   in one line rather than leave it looking like an oversight.
5. **Queue status.** `StatusCancelled = "cancelled"`, with the comment above
   the block ("Nothing produces a cancelled status because nothing can cancel
   a run") rewritten rather than left contradicting the constant beneath it.
6. **Settings.** `run.stop_grace_period` (duration, `30s`, 1s–1h,
   `GroupRunBudget`, not restart-flagged) and `http.control_token` (string,
   secret, `GroupCredentials`, empty default — generated at startup, step 4).

**Tests.**

- `TestAppendEventsRefusesCancelledSession`, and that every *other* terminal
  status still accepts appends (compaction and resume depend on it).
- `TestFinishSessionCannotLeaveCancelled`.
- `TestCancelRunningSessionIgnoresIdleness` — the mirror image of the existing
  `CloseSession` idle test.
- `TestSteerMessagesAfter` — ordering by `seq`, the limit, and that it reads
  only the two kinds (the pattern `RequestStatus` already follows: never pull
  reasoning or content payloads for a cheap query).
- **`TestAppendOnly` in `internal/fold` is the one that matters.** Extend its
  corpus with a log where a `steer_message` lands between an assistant's
  `tool_call` events and its `tool_result` events, and a `steer_applied` at
  the following boundary. It must still hold for every prefix. If it fails
  here, the two-kind split has been implemented as one kind somewhere.
- `internal/settings/registry_test.go` pins both new defaults against their
  package constants, as it does for the existing ones.

**Exit.** `scripts/test.sh` clean; `TestAppendOnly` green with the new corpus.
Nothing user-visible has changed yet.

**Risk.** The append fence is the sharp edge: a fence that is too broad breaks
compaction (which creates a *new* session) or resume. The tests above are
specifically the guard against that.

---

## Step 3 — The control seam

**Goal.** The in-process registry and the `Kill` escalation exist and are
tested, with no external surface. Unblocks steps 4 and 6.

**Files.**

| File | Change |
| --- | --- |
| `internal/worker/control.go` (new) | `Controller`, `inflight`, `Pool.Stop`, `Pool.Running` |
| `internal/worker/pool.go` | register/deregister in `run`; idempotent slot release; `disposed` guard in `finish`; `cancelled` classification |
| `internal/worker/control_test.go` (new) | below |

**Changes.**

1. **Register** in `Pool.run`, immediately after `sessionID` is generated and
   `runCtx` exists — before workspace preparation, so a run that wedges in
   `git clone` is stoppable too. **Deregister** where `finish` disposes of the
   message, so the entry outlives `Runner.Run` returning by exactly as long as
   the disposal takes.
2. **The semaphore release becomes a closure.** `Pool.Run`'s
   `defer func() { <-sem }()` becomes `defer release()`, where `release` is a
   `sync.Once` stored on the `inflight` record. The force-finish path calls
   the same closure. Without this the pool slot is held by a defer inside the
   goroutine that by construction never runs.
3. **`Pool.Stop(sessionID, reason)`** — the four steps in RUN-CONTROL.md:
   look up (404 as `ErrRunNotFound`), record and cancel, return immediately,
   escalate in a goroutine after `run.stop_grace_period`.
4. **Force-finish** publishes `queue.Result{Status: StatusCancelled, Error:
   {Code: "cancelled", Message: reason}}`, calls `CancelRunningSession`, runs
   the ordinary `finish` sequence (row, publish, ack) under the `disposed`
   guard, releases the slot, and logs a leaked-goroutine warning naming the
   session and its last tool call.
5. **`finish` becomes disposal-guarded** so the two paths cannot both publish.
   A wedged goroutine that wakes logs and returns.
6. **`classify`** grows a `cancelled` arm: a run whose `runCtx` was cancelled
   by a stop is `StatusCancelled`, distinct from `context.DeadlineExceeded`'s
   `StatusTimeout`. The signal is the registry's `stopping` flag, not the
   context error — both produce `context.Canceled`.

**Tests.** All in `internal/worker`, which already has a real-broker harness
and `TestMain`'s stream isolation.

- `TestStopHealthyRunCancels` — a run whose fake `PrepareWorkspace` and runner
  block on `ctx`; `Stop` returns immediately, the run ends, the published
  result is `cancelled`, the message is acked.
- **`TestStopWedgedRunFreesSlotAndCancels`** — the direct regression test for
  the production incident. Drive a runner that blocks on a channel the test
  never closes (the same shape as step 1's wedged `Bash`, *without* relying on
  step 1's fix, so the backstop is proven independent of it). Assert: the
  session row reaches `cancelled` within the grace period, a `cancelled`
  result is published, the message is acked, and the pool accepts a new
  message afterwards.
- `TestWedgedRunCannotPublishTwice` — release the blocked runner after the
  force-finish and assert no second result and no second ack.
- `TestStopUnknownSession` → `ErrRunNotFound`.
- `TestStopIsIdempotent` — two stops, one cancellation, one result.
- Use a short `run.stop_grace_period` in tests; the field is resolved per call
  so a test resolver can set it without touching the default.

**Exit.** The wedged-run test passes against a real broker, and the pool
survives it — a second request runs to completion on the same pool afterwards.

**Risk.** The grace-period default. Size it against the slowest ordinary thing
a healthy run does between context checks, not against typical sub-turn
latency; too short and a healthy cancel gets force-finished and logged as a
leak it never was.

---

## Step 4 — Stop, end to end

**Goal.** An operator or another agent can stop a run from the browser, MCP,
or the CLI — including a wedged one.

**Files.**

| File | Change |
| --- | --- |
| `internal/httpapi/server.go` | `RunController` interface, `POST /api/sessions/{id}/stop`, `GET /api/control-token`, bearer guard, `methodGate` POST rules |
| `internal/httpapi/server_test.go` | below |
| `cmd/harness/serve.go` | generate `http.control_token` when unset; wire `Pool` as `RunController` |
| `cmd/harness/stop.go` (new), `main.go` | `harness stop <session-id> ["reason"]` |
| `internal/mcp/httpclient.go` | `postJSON`, bearer header |
| `internal/mcp/control.go` (new), `service.go` | `deepseek_stop` |
| `internal/config` | `MCPConfig.ControlToken` from `DEEPSEEK_CONTROL_TOKEN` |
| `web/src/api/operations.ts`, `TranscriptScreen.tsx`, `SessionListScreen.tsx` | the control and its confirm |
| `docs/DESIGN.md`, `ARCHITECTURE.md`, `CLAUDE.md`, `web/CLAUDE.md`, `docs/DATA-API.md` | the invariant edits below |

**Changes.**

1. **The endpoint.** `POST /api/sessions/{id}/stop`, body
   `{"reason": "..."}`. `writeGuards` (415/403) unchanged, then the bearer
   check, then: 404 unknown session, 409 when this process is not running it
   (message naming the session's actual status), 202 otherwise. No `If-Match`
   — RUN-CONTROL.md, "The HTTP surface", says why, and the handler comment
   should carry the one-sentence version so nobody restores it for consistency
   with the row endpoints.
2. **`methodGate`.** `writeAllowed` and `allowedMethods` learn POST on the
   control sub-paths; a POST anywhere else stays a 405 with a correct `Allow`.
   Add the predicate beside `isSessionPath` and friends — note it must not
   widen `isSessionPath` itself, which requires no further segments and must
   keep the PATCH/DELETE rules to the row.
3. **Token generation.** In `runServe`, after the resolver exists: read
   `http.control_token`, and if empty generate 32 bytes of `crypto/rand` as
   base64url and store it. Log that one was generated (never the value).
4. **`GET /api/control-token`** — loopback `RemoteAddr` only, else 403. This
   is the frontend's and MCP's way to get it; a remote caller must be given it
   out of band, which is exactly the property that makes the token worth
   having.
5. **MCP.** `postJSON` in `httpclient.go` — the package's first non-GET
   helper, still no database handle. `deepseek_stop` takes `session_id` or
   `request_id` (resolve via the existing `/api/requests/{id}` lookup
   `deepseek_result` uses) and an optional `reason`, and its description says
   it never blocks and that `deepseek_status` is how to see the outcome.
6. **CLI.** `harness stop` posts to the same endpoint, reading the token from
   the settings table. One file per subcommand; add it to `main.go`'s dispatch
   and to `harness help`.
7. **Frontend.** Stop on the in-flight session card and the transcript header,
   behind a confirm; *stopping…* between the 202 and the terminal event; then
   the `CANCELLED` badge, which `statusBadge.ts` already renders. The screen
   re-fetches after the write rather than guessing — the settings screen's
   pattern, per `web/CLAUDE.md`.

**Tests.**

- `internal/httpapi`: 401 without a token and with a wrong one; 415 and 403
  from the existing guards; 404 unknown; 409 not-running; 202 accepted, with a
  fake `RunController`. Extend the existing method-gate test so POST is
  rejected everywhere it is not explicitly allowed, and the
  events-resource-has-no-mutating-route test still passes.
- `GET /api/control-token` from a non-loopback `RemoteAddr` is 403.
- `internal/mcp`: the existing HTTP integration harness, driving
  `deepseek_stop` against a stub endpoint.
- Manual, end to end, per the smoke sequence in [TESTING.md](TESTING.md): stop
  a healthy run and a deliberately wedged one (`Bash` with `sleep 600 &`
  before step 1's fix is in the image, or a `sleep 600` foreground call after
  it), from both the browser and MCP.

**Docs in this step.** ARCHITECTURE.md's "serves `GET` and `HEAD` and nothing
else" invariant and its "no endpoint starts, steers, or stops a run" clause;
the `httpapi` codemap entry, restated as "imports neither `session` nor
`worker` — control goes through the declared `RunController` seam"; §4.10's
"There is no `cancelled`"; CLAUDE.md's NATS-handle rule (narrowed, not
deleted — see step 6); web/CLAUDE.md's "no cancel control".

**Exit.** Stopping a healthy run and stopping a deliberately wedged one both
reach `cancelled` within the grace period, from the browser and from MCP, and
the pool keeps working afterwards.

---

## Step 5 — Steer, end to end

**Goal.** An operator or another agent can add instructions to a run in
progress, from any surface, without the loop ever blocking on it.

**Files.**

| File | Change |
| --- | --- |
| `internal/session/turn.go` | pick up pending steers at the top of `runSubTurn` |
| `internal/session/runner.go` | derive the applied high-water mark on entry to `runLoop` |
| `internal/httpapi/server.go` | `POST /api/sessions/{id}/steer` |
| `cmd/harness/steer.go` (new), `main.go` | `harness steer <session-id> "text"` |
| `internal/mcp/control.go` | `deepseek_steer` |
| `web/src/api/fold.ts`, `types.ts`, `components/blocks/`, `TranscriptScreen.tsx` | the steer block and the input |
| `docs/DESIGN.md` §4.1, `docs/OBSERVED.md` | the new kinds; the wire-shape confirmation |

**Changes.**

1. **The loop.** At the top of `runSubTurn`, before `fold.Fold`: read
   `steer_message` events past the applied high-water mark, append one
   `steer_applied` per steer (batched into a single `AppendEvents` call, so
   the mirror and hub see one batch), push them onto `*allEvents`, and fold.
   Order within the batch is `seq` order — two steers arrive as two user
   messages in the order they were sent.
2. **The high-water mark** is derived from the log in `runLoop`, not carried
   in memory, so `Resume` and a compacted successor recompute it. A compacted
   session is a *new* session id with a fresh log; steers applied before
   compaction live in the summary that carried them forward, and unapplied
   ones stay attached to the retired session — call that out in the code, it
   is the one place the two-kind design has a rough edge.
3. **The endpoint.** `POST /api/sessions/{id}/steer`, body `{"text": "..."}`,
   same guards and bearer as stop. 404 unknown, 409 when the session is not
   `running`, 400 on empty text, 202 with the `seq` the event landed at. Not
   idempotent by design: two steers are two instructions.
   Note this handler needs no `RunController` at all — it is a store write.
4. **Frontend fold.** One new `Block` variant, `steer`, emitted on
   `steer_message` in the *pending* state and flipped to *delivered* when the
   matching `steer_applied` (by `source_seq`) arrives. This is the one place
   the browser fold completes a block it has already emitted; the Go fold's
   append-only rule does not apply to display blocks, and a comment should say
   so where it happens.
5. **The input** sits on the transcript screen while the session is running,
   styled distinctly from the model's turns, showing pending vs delivered. A
   steer that sits pending for minutes is the operator's signal that the run
   is wedged — that is a feature of the display, not an accident of it.

**Tests.**

- `internal/session`: `TestSteerAppearsAsNextUserMessage` — append a
  `steer_message` mid-run against a fake API client, assert the next request's
  messages array ends with the steer text verbatim as a user message, and that
  every message before it is byte-identical to the previous request's
  (`prefix_test.go` has the shape to copy).
- `TestSteerDoesNotInterruptToolCalls` — a steer during a tool round changes
  nothing about that round.
- `TestSteerHighWaterSurvivesResume` — resume a session with an applied and an
  unapplied steer; only the unapplied one is delivered, and only once.
- `internal/fold`: two steers in one batch fold to two user messages in order.
- Frontend: `fold.test.ts` for the pending→delivered transition.
- **A cache-churn check**, per [CACHE.md](CACHE.md)'s diagnostic: a run with a
  steer mid-way shows no churn point at any sub-turn before it. This is the
  assertion that the two-kind split was worth making, so it should exist
  rather than be argued.
- **One live-API confirmation**, by hand: a `tool` message followed by a
  `user` message ahead of the next assistant turn is accepted. Record the
  answer in OBSERVED.md — the vendored docs do not promise it.

**Exit.** A steer sent mid-tool-call does not affect that call, appears in the
transcript immediately as pending, becomes delivered at the next sub-turn, and
the model's next request carries it verbatim as the newest message with an
unperturbed prefix.

---

## Step 6 — Start from the browser

**Goal.** Close the one gap in starting a run. MCP and the CLI already have
this; the browser gets a write path that is indistinguishable downstream.

**Files.**

| File | Change |
| --- | --- |
| `internal/httpapi/server.go` | `RunPublisher` interface, `POST /api/runs` |
| `cmd/harness/serve.go` | implement `RunPublisher` over the JetStream handle and pass it in |
| `internal/queue/request.go` | `PublishRequest(ctx, js, req)` — one marshal-and-publish shared by all three producers |
| `web/src/components/` | the start form |
| `CLAUDE.md`, `docs/DESIGN.md` §4.2, `docs/DATA-API.md` | the rule the seam replaces |

**Changes.**

1. **The interface** (`PublishRequest(ctx, queue.Request) error`) is declared
   in `httpapi` and implemented in `cmd/harness` — composition happens in
   `cmd/` and nowhere else, per ARCHITECTURE.md. `httpapi` gains an import of
   `internal/queue` for the request type and `Validate`, and no JetStream
   handle. There is no shared publish helper today: `harness publish` and
   `deepseek_agent` each marshal and call `js.Publish(queue.RequestSubject(id),
   data)` themselves. Add `queue.PublishRequest` and move all three onto it,
   rather than writing a third copy in the adapter.
2. **The endpoint.** `POST /api/runs`, body identical to the work-request
   shape, `request_id` optional and generated when absent. Validation is
   `req.Validate()` — the queue's own, so browser and queue can never
   disagree. 400 with the validator's message, 202 `{"request_id": "..."}`.
3. **The form**: prompt, repos (URL#branch, repeatable), permission mode, and
   the optional fields, mirroring `publish`'s flags. On submit, follow the new
   session from the existing `GET /api/stream` list feed as soon as one
   appears. Permission mode is a deliberate choice, not a default — a `full`
   run has the host docker socket (CLAUDE.md).
4. **CLAUDE.md's rule** becomes: the HTTP server holds no JetStream handle;
   it holds one narrow publisher interface, and the run-control seam is
   `RunPublisher` + `RunController`. Retiring the rule silently is the failure
   mode worth avoiding — it was there to stop the seam being chosen by
   accident, and it should record that the seam was chosen deliberately.

**Tests.**

- `internal/httpapi`: validation rejection mirrors `queue`'s own errors; a
  generated `request_id`; 202 with the id; the guards and bearer as elsewhere.
  Use a fake publisher.
- End to end against the dev stack: a run started from the browser and one
  started from `harness publish` produce the same event kinds and the same
  row shape, and a duplicate `request_id` deduplicates identically.

**Exit.** A browser-started run is byte-identical in the store to an
MCP-started or CLI-started one — same event kinds, same validation, same
idempotency behaviour.

---

## Cross-cutting: the documentation sweep

Each step carries its own doc edits, listed above. The ones that must not be
missed, because they are written as facts and would otherwise silently rot:

| Document | Statement | Step |
| --- | --- | --- |
| `ARCHITECTURE.md` | "The HTTP API serves `GET` and `HEAD` and nothing else. No endpoint starts, steers, or stops a run." | 4 |
| `ARCHITECTURE.md` | the `httpapi` codemap entry: "it cannot reach a running loop" | 4 |
| `ARCHITECTURE.md` | the overview: "nothing it does can start, steer, or stop a run" | 4 |
| `ARCHITECTURE.md` | the two folds "must agree in shape" — two new kinds, both folds | 5 |
| `docs/DESIGN.md` §4.10 | "There is no `cancelled`." | 4 |
| `docs/DESIGN.md` §4.2 | run control as "the next stage, not built" | 4, 6 |
| `docs/DESIGN.md` §4.1 | the event-kind list | 5 |
| `docs/DATA-API.md` | the data/run-control bracket, and the `If-Match` departure | 4 |
| `CLAUDE.md` | "do not give the HTTP server a NATS handle" | 6 |
| `web/CLAUDE.md` | "no prompt box, no approve button, no cancel control" | 4, 5, 6 |
| `docs/TOOLS.md` | what a wedged `Bash` command now does | 1 |
| `docs/OBSERVED.md` | the `tool` → `user` → `assistant` wire shape | 5 |

The approve button stays out of `web/CLAUDE.md`'s prohibition. §4.6 has not
changed and a loop that waits on a person is still a loop that stalls when
nobody is watching.

## Sequencing notes

- **1 and 2 are independent of each other** and of everything else; either can
  land first, and step 1 is worth landing regardless of whether the rest of
  this plan ever happens.
- **3 depends on 2** (the store fences and `CancelRunningSession`).
- **4 depends on 3.** 
- **5 depends on 2 only** — it needs the event kinds and the fold, not the
  registry — so it can land before or beside 4 if steering turns out to be the
  more urgent capability.
- **6 depends on nothing but 4's bearer-token plumbing**, and could be lifted
  earlier if starting from the browser is what is actually wanted first.
- Deliberately proving step 3's backstop **without** step 1's fix in the same
  test binary is the one ordering subtlety: the wedged-run test fakes the
  wedge at the runner level rather than through `Bash`, so the two fixes are
  proven independent rather than one masking the other.
