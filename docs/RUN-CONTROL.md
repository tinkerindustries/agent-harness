# Run control: starting, steering, and stopping a run

DESIGN.md §4.2 calls this stage two: an interactive frontend that can start,
steer, and stop a run, sitting on top of the data API stage one is building now.
It has been named and deliberately not built since v1 — `internal/httpapi`
imports neither `session` nor `worker`, which ARCHITECTURE.md calls a
structural fact rather than a policy. This plan is the design that fact was
waiting on, and it retires it on purpose: a narrow, typed seam between
`httpapi` and `worker`, not an incidental one.

The prompt for writing this down now: `sess-23f440713ef783c1484eb3eb0be24969`
hung on production for good, wedged inside a `Bash` call that backgrounded a
process without redirecting its output. DESIGN.md §4.10 already names the
failure mode — "a tool call blocked on something that ignores cancellation
[is not reaped]... the delivery ceiling bounds the damage... but it does not
end the wedged attempt" — and this plan exists to close it, not just to add a
stop button.

## Scope

- **Starting.** The one gap here is the browser. MCP already starts runs —
  `deepseek_agent` publishes a work request — and so does `harness publish`.
  What's missing is a write path from the web UI, which today can only watch.
- **Steering.** Append a new user-authored message to a run in progress.
  Nothing today lets an operator add instructions mid-run from any surface.
- **Stopping.** End a run on request — including one wedged inside a tool call
  that ignores its context, which is the case that actually happened in
  production and the case a naive `cancel()`-only stop would not fix.
- Every one of these ships on **MCP and the web UI both**, not one first. MCP
  callers are other agents, not just people at a keyboard, and an agent that
  can start a job but not steer or stop it is only half a capability.

## Explicitly out of scope

- **Horizontal scaling of `harness serve`.** The seam below is in-process: one
  running session is one goroutine in one OS process, and the registry that
  makes it controllable lives in that same process. That is true today and
  nothing here changes it, but it is an assumption worth stating, because it
  is the thing that breaks first if `serve` is ever run as more than one
  replica behind the same queue.
- **Approval-gated tool calls.** §4.6 rejected a loop that blocks on a human
  for exactly this reason: it stalls when nobody is watching. Steering adds a
  channel *into* the loop; it does not make the loop wait on it.
- **Background shells with polling and kill tools.** TOOLS.md already names
  this as a separate follow-up for the `Bash` tool itself (letting a command
  run detached, with its own poll/kill affordances). This plan's "kill a
  wedged call" is about an operator ending a *foreground* call from outside;
  it is a prerequisite for that follow-up, not a substitute for it.
- **A revisited authentication story.** Addressed narrowly, below — this plan
  adds a control-token requirement, but the day this port is meant to be
  exposed off loopback is a bigger question than this doc answers.

## Design decisions

### The seam is in-process, not NATS

`harness serve` already runs the worker pool and the HTTP surface in one
process (ARCHITECTURE.md). CLAUDE.md's warning against giving `httpapi` a NATS
handle is not a ban on run control — it is a warning against *that specific
handle* getting smuggled in before the seam was chosen deliberately. NATS stays
what §4.10 designed it to be: durable, decoupled, at-least-once ingress for
work and results. Control is the opposite of that — real-time, addressed at
one specific in-flight goroutine, and actively wrong to make redeliverable
(a redelivered "stop" is fine; a redelivered "steer" is a duplicated
instruction).

So: a new in-process registry, `internal/worker` (or a small new
`internal/runs` if `worker` gets crowded), mapping a running session id to a
handle:

```go
type Handle struct {
    Cancel func(reason string)      // soft: cancels runCtx
    Steer  func(text string) error  // appends a steer_message event
    Kill   func() error             // hard: see "stopping a wedged run" below
}
```

`Pool.run` registers a handle when it starts `Runner.Run` and deregisters it
in the same place `finish` already runs. `httpapi` is given a small interface
— `RunController` — implemented by `*worker.Pool`, not the whole pool. That
keeps the new dependency typed and reviewable instead of `httpapi` reaching
into `worker`'s internals wholesale, which is the actual thing ARCHITECTURE.md
was protecting against.

`harness mcp` stays a separate process with no database handle and no pool
reference, exactly as designed. Its new `deepseek_stop` and `deepseek_steer`
tools are thin wrappers over the HTTP endpoints below — the same pattern
`deepseek_result` already uses to read the store without touching it directly.

### Stopping a healthy run vs. a wedged one

A healthy run answers a cancelled `runCtx` at its next check point (top of a
sub-turn, between tool calls) the same way a `deadline_ms` timeout already
does — no new mechanism needed there.

A wedged run is the case that matters and the case sess-23f440713ef783c1484eb3eb0be24969
actually hit. `internal/tools/bash.go` runs the child with
`exec.CommandContext`, whose cancellation kills only the direct `/bin/sh`
child. A command that backgrounds a process without redirecting its output
(`node ... &` inheriting the captured stdout/stderr pipe) leaves that pipe
open after `sh` exits, and `cmd.Run()` blocks on it forever — past the tool's
own timeout, past a cancelled `runCtx`, past anything the session loop can do
from inside Go. Go cannot forcibly kill a goroutine blocked in a syscall; only
ending whatever holds the pipe open unblocks it. **A stop control that only
cancels a context will look identical to today's timeout on this exact
failure — it will not return.** Phase 1 below fixes the tool layer so this
class of hang stops happening; the registry's `Kill` path (phase 2) is the
backstop for the ones that still get through.

That backstop has to be honest about what it can and cannot promise: it can
mark the run stopped, publish a terminal result, and free the pool slot
without waiting on the goroutine; it cannot guarantee the goroutine exits. Log
a leaked-goroutine warning when `Kill` fires and the run never actually
returns, so an abandoned goroutine is visible rather than a silent leak that
looks identical to a clean stop.

### Steering: augment, don't gate

A steer call appends a new event kind, `steer_message` (`internal/fold`'s
switch on event kind gets one more case), monotonic `seq` like everything
else in the log. The session loop checks for pending steer events at the top
of its next sub-turn — after the current tool round finishes, never
mid-call — and folds each one in as a new user message appended to the tail
of the conversation. That is the same shape §3.2 already requires of
everything else: no prefix rewrite, tail-only growth, cache-safe. A steer sent
while a long tool call is running shows up in the transcript immediately (the
event is visible the moment it is accepted), but it reaches the model only at
the next natural boundary — the same non-blocking shape the permission policy
already uses, and the reason §4.6's stalling problem does not reopen.

### Starting from the browser

`POST /api/runs` takes exactly the request body §4.10 already specifies
(`prompt`, `repos`, `permission_mode`, and the rest) and publishes it to the
WORK stream, the same way `harness publish` and `deepseek_agent` already do.
Recommend against a shortcut that calls into the pool directly in-process:
publishing keeps the browser as one more producer among several, so the
claim/heartbeat/redelivery machinery in §4.10 stays the *only* way a session
ever starts, with no second code path to keep in sync.

### Authentication

Loopback plus the existing same-origin/`Content-Type` guards (§4.2) stay in
place and are not enough on their own for a surface that can now spend money
and run arbitrary commands under `full` permission mode. Add one new setting,
`http.control_token` (secret-flagged like the API keys, generated on first
`serve` startup if unset, in the same registry as everything else in
`internal/settings`), required as a bearer token on the three write endpoints
this plan adds. Stage-one's existing endpoints (settings, session repair) are
unaffected — their blast radius is a database row; this surface's blast
radius is a running command. This is a floor, not an answer to §4.2's larger
question about exposing the port at all.

## New result status

DESIGN.md §4.10 currently states flatly: "There is no `cancelled`." This plan
adds one. `internal/queue` already has `StatusOK`, `StatusFailed`,
`StatusDenied` (unused since a per-run directory made the case it named
impossible), and `StatusTimeout` — add `StatusCancelled = "cancelled"`
alongside them, distinct from `StatusTimeout` (deadline-driven, no operator
involved).

The frontend already expects this. `web/src/components/statusBadge.ts`
handles `status: "cancelled"` today, ahead of any backend code that can
produce it — `outcome({ status: "cancelled" })` already renders `CANCELLED`
with the `stopped` badge variant. No frontend relabelling needed; wiring the
backend to actually emit it is the whole gap. (Careful not to collide with
the existing `STOPPED` label, which today means `run_finished.reason ==
"no_tool_calls"` — the model stopping on its own, nothing to do with an
operator. `CANCELLED` is the new, distinct label for this plan's stop.)

## Phases

Each phase ships and leaves the harness working; nothing here needs a
big-bang cutover. Phase 1 stands alone and is worth doing regardless of the
rest of this plan. Phases 3–5 each land backend, MCP, and web UI together, so
a phase is "start/steer/stop works end to end for this one capability," not
"the backend half of it."

### Phase 1 — Bash: survive a wedged child

**Goal.** The existing `tools.bash_timeout` actually terminates a command that
backgrounds a process without redirecting its output, instead of hanging past
it indefinitely. No API surface; this is a correctness fix to
`internal/tools/bash.go` that stands on its own.

**Changes.** Set `cmd.WaitDelay` (Go 1.20+, built for exactly this) alongside
the existing `exec.CommandContext` cancellation, so Go force-closes the
child's stdout/stderr pipes a bounded time after `ctx` is done even if a
backgrounded grandchild still holds them open. Additionally run the child in
its own process group (`SysProcAttr{Setpgid: true}`) and have cancellation
signal the whole group (`-pid`) rather than the single child pid, so an
orphaned background process actually dies instead of leaking on the host —
today it would keep running (and keep the port it bound) after the tool call
"times out."

**Exit.** A regression test that reproduces sess-23f440713ef783c1484eb3eb0be24969's
exact shape — a command that backgrounds a process holding the output pipe
open — returns a timed-out `Result` within the configured timeout instead of
hanging, and the backgrounded process is confirmed dead afterward (check its
pid, or that its listening port is free).

**Risk.** Low; scoped to one function, and every other `Bash` caller is
unaffected — `WaitDelay` only changes behaviour once `ctx` is already done.

### Phase 2 — The control seam

**Goal.** The in-process registry and the `Kill` escalation path exist and are
tested, with no external surface yet. This unblocks phases 3 and 4 and is
independently testable inside `internal/worker`.

**Changes.** The `Handle` registry described above, wired into `Pool.run`.
`Cancel` is the existing per-run `context.CancelFunc`. `Kill` adds a grace
period — new setting `run.stop_grace_period` (restart-required, alongside
`worker.max_delivery_attempts` in the run-budget settings group): call
`Cancel`, wait up to the grace period for `Runner.Run` to return, and if it
has not, mark the session `StatusCancelled` and free the pool slot anyway,
without waiting further. Log when this abandons a goroutine that never
returns.

**Exit.** A worker-package test that starts a run whose `Bash` call is
deliberately wedged (same shape as phase 1's regression test, but *without*
phase 1's fix, to prove the registry's backstop works independently of it),
calls `Kill`, and asserts the pool slot frees and the session reaches
`cancelled` within the grace period — this is the direct regression test for
the production incident that started this plan.

**Risk.** The grace-period default needs to be long enough that a healthy
`Cancel` (the common case) isn't ever mistaken for wedged — size it against
the slowest ordinary tool-timeout, not against typical sub-turn latency.

### Phase 3 — Stop, end to end

**Goal.** An operator or another agent can stop a run from MCP or the browser,
including a wedged one.

**Backend.** `POST /api/sessions/{id}/stop`, body `{"reason": "..."}`, bearer
auth via `http.control_token`. Calls `Handle.Cancel`, then `Handle.Kill` after
the grace period if the run hasn't finished. Returns `202` immediately — this
mirrors §5's existing rule that "a request that starts a run is not answered
by the response to it"; the terminal state arrives over the session's own SSE
stream, not the stop response.

**MCP.** `deepseek_stop`, taking `session_id` (or `request_id`, resolved the
same way `deepseek_result` already does) and an optional reason, described the
same way the existing tools are: never blocks, reports what happened via
`deepseek_status` afterward.

**CLI.** `harness stop <session-id> ["reason"]`, `cmd/harness/stop.go`,
following the one-file-per-subcommand convention.

**Frontend.** A Stop control on the in-flight session card (§5.8) and the
transcript header, behind a confirmation (this codebase treats hard-to-reverse
actions as worth a confirm step generally, and ending a run an operator can't
get back is one). Shows a "stopping…" state between the request and the
terminal event, then the new `CANCELLED` badge `statusBadge.ts` already knows
how to render.

**Exit.** Stopping a healthy run and stopping a deliberately wedged run
(phase 2's test, driven end to end through the HTTP endpoint this time) both
reach `cancelled` within the grace period, from both MCP and the web UI.

### Phase 4 — Steer, end to end

**Goal.** An operator or another agent can add instructions to a run in
progress, from MCP or the browser, without the loop ever blocking on it.

**Backend.** New `steer_message` event kind in `internal/fold` and its
frontend counterpart. `POST /api/sessions/{id}/steer`, body `{"text": "..."}`,
same auth as stop. The session loop checks for unconsumed `steer_message`
events at the top of each sub-turn and folds each into a new tail-appended
user message, in order.

**MCP.** `deepseek_steer`, taking `session_id` and `text`.

**CLI.** `harness steer <session-id> "..."`.

**Frontend.** A steer input on the transcript screen for an in-flight
session, visible only while the session is running. Sent text appears in the
transcript immediately as its own block (so the operator sees it was
received) and is styled distinctly from the model's own turns, since it may
sit a sub-turn or two before the model actually reads it.

**Exit.** A steer sent mid-tool-call does not affect that call, appears in the
transcript immediately, and the model's next sub-turn includes it verbatim as
the newest message. A cache-churn test (per CACHE.md's diagnostic) confirms
this doesn't perturb the prefix of turns before it.

### Phase 5 — Start from the browser

**Goal.** Close the one gap in starting a run: the browser gets a write path.
MCP (`deepseek_agent`) and the CLI (`harness publish`) already have this.

**Backend.** `POST /api/runs`, body identical to §4.10's work-request shape,
same auth as stop/steer, publishes to the WORK stream exactly as the existing
producers do. No new code path in `internal/worker` — a browser-started run is
indistinguishable from a queue-started one to everything downstream.

**Frontend.** A start form: prompt, repos, permission mode, and the optional
fields, mirroring what the CLI's `publish` flags already accept. Submitting
returns a `request_id` and the screen can immediately start following its
session once one exists (the same `GET /api/stream` the session list already
subscribes to will show it arrive).

**Exit.** A run started from the browser is byte-identical in the store to
one started from MCP or the CLI — same event kinds, same request-body
validation, same idempotency behaviour on a duplicate `request_id`.

## Open questions this plan does not close

- **Multi-instance `serve`.** If this ever needs to run as more than one
  replica, the in-process registry stops being sufficient — control needs to
  find the right instance. Not a problem today; worth a pointer here for
  whoever hits it.
- **Rate-limiting stop/steer.** A control token is authorization, not a
  throttle. A misbehaving caller (or a compromised token) could steer a
  session in a tight loop; not addressed here.
- **Fine-grained kill.** This plan kills at the run level (the whole session).
  Killing one wedged tool call while letting the rest of the run continue is
  a smaller, harder feature and is deliberately not this plan's phase 2 —
  the whole-run `Kill` is the one that matches what actually happened in
  production and is enough to unblock an operator.
