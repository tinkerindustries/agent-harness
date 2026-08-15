# Run control: starting, steering, and stopping a run

An interactive frontend that can start, steer, and stop a run, sitting on top
of the data API [DATA-API.md](DATA-API.md) specifies. `internal/httpapi`
imports neither `session` nor `worker` ([ARCHITECTURE.md](../ARCHITECTURE.md)):
run control reaches the loop through two narrow, typed seams out of `httpapi`,
not an incidental import.

Closing a run that never returns is the case that matters most: §4.10 names
the failure mode of a tool call blocked on something that ignores
cancellation — the delivery ceiling bounds the damage but does not end the
wedged attempt — and this design exists to close that, not just to add a stop
button.

This is the design. The build order, file by file, is
[RUN-CONTROL-PLAN.md](RUN-CONTROL-PLAN.md).

## Scope

- **Starting.** MCP (`deepseek_agent`), `harness publish`, and the web UI's
  start form each publish a work request.
- **Steering.** An operator appends a new user-authored message to a run in
  progress, from any of the three surfaces.
- **Stopping.** Ends a run on request — including one wedged inside a tool
  call that ignores its context, which a naive `cancel()`-only stop would not
  fix.
- Every one of these ships on **MCP, the CLI, and the web UI**, not one first.
  MCP callers are other agents, not just people at a keyboard, and an agent
  that can start a job but not steer or stop it is only half a capability.

### Explicitly out of scope

- **Horizontal scaling of `harness serve`.** The stop seam below is
  in-process: one running session is one goroutine in one OS process, and the
  registry that makes it stoppable lives in that same process. That is true
  today and nothing here changes it, but it is an assumption worth stating,
  because it is the thing that breaks first if `serve` is ever run as more
  than one replica behind the same queue. (Steering, as it turns out, does not
  share the assumption — see [Steering](#steering-augment-dont-gate).)
- **Approval-gated tool calls.** §4.6 rejected a loop that blocks on a human
  for exactly this reason: it stalls when nobody is watching. Steering adds a
  channel *into* the loop; it does not make the loop wait on it.
- **Background shells with polling and kill tools.** [TOOLS.md](TOOLS.md)
  names this separately as not built, for the `Bash` tool itself (letting a
  command run detached, with its own poll/kill affordances). This design's
  "kill a wedged call" is about an operator ending a *foreground* call from
  outside; it is a prerequisite for that, not a substitute for it.
- **Killing one tool call and letting the run continue.** Stop is whole-run.
  See [What this does not promise](#what-this-does-not-promise).
- **A revisited authentication story.** Addressed narrowly in
  [Authentication](#authentication) — this design adds a control-token
  requirement — but the day this port is meant to be exposed off loopback is a
  bigger question than one document answers.

## The two seams

`harness serve` runs the worker pool and the HTTP surface in one process
(ARCHITECTURE.md). CLAUDE.md's warning against giving `httpapi` a queue
handle is not a ban on run control — it is a warning against *that specific
handle* getting smuggled in rather than a declared seam. Run control uses two
seams, different shapes because starting and stopping are different problems.

### Starting is a publish, so the seam is a publisher

The queue stays what §4.10 designed it to be: durable, decoupled,
at-least-once ingress for work. A browser-started run is one more producer
among several — the claim/heartbeat/redelivery machinery in §4.10 stays the
*only* way a session ever starts, with no second code path to keep in sync.
The seam is therefore the narrowest thing that can enqueue:

```go
// RunPublisher is the subset of the queue a start endpoint needs: enqueue
// one validated work request. Declared here and implemented in cmd/harness,
// so this package never holds a queue handle — only the ability to enqueue
// one request.
type RunPublisher interface {
    PublishRequest(ctx context.Context, req queue.Request) error
}
```

Recommended against: an in-process shortcut that calls `Pool.run` directly.
It would skip the idempotency row, the heartbeat, and the redelivery ceiling,
and it is a second way for a session to begin.

`httpapi` imports `internal/queue` for `queue.Request` and its `Validate`
rather than a copy of the rules, because a copy is a validator that eventually
disagrees with the queue's. It holds no queue handle — the request type and
its validator are already the whole of its `internal/queue` import, so the
import graph barely moves; the change is that one narrow interface can now
write.

### Stopping is addressed at one goroutine, so the seam is a registry

Control is the opposite of durable ingress — real-time, addressed at one
specific in-flight goroutine, and actively wrong to make redeliverable. So
stop does not go through the queue. A new in-process registry in
`internal/worker` maps a running session id to the handle for the goroutine
running it:

```go
// Controller is the pool's registry of in-flight runs, keyed by session id.
// One entry exists from the moment Pool.run generates a session id until
// that run's message is disposed of, whether by the run finishing or by a
// stop taking it over.
type Controller struct {
    mu   sync.Mutex
    runs map[string]*inflight
}

type inflight struct {
    requestID string
    sessionID string
    msg       queue.Msg          // so the escalation can run the same finish
    cancel    context.CancelFunc // cancels runCtx: the soft stop
    done      chan struct{}      // closed when Runner.Run returns
    releaseSlot   func()         // idempotent; frees the pool semaphore
    stopHeartbeat func()         // idempotent; stops the message heartbeat
    started   time.Time
    stopping  atomic.Bool        // a stop has been accepted
    disposed  atomic.Bool        // the queue message has been answered
    reason    atomic.Pointer[string]
}
```

Two fields carry reasons worth stating up front: `msg`, because the
escalation has to run the *same* finish sequence an ordinary run would rather
than a parallel one, and `stopHeartbeat`, for the reason in the guards below.

`httpapi` is given a small interface — `RunController` — implemented by
`*worker.Pool`, not the whole pool, exactly as `QueuePool` already is for
`/api/queue`:

```go
// RunController is the subset of the worker pool that run control needs.
type RunController interface {
    // Stop begins ending sessionID and returns immediately. It reports
    // ErrRunNotFound when no run in this process owns that session, and nil
    // both for a stop it accepted and for one already in progress.
    Stop(sessionID, reason string) error
    // Running reports whether this process is running sessionID.
    Running(sessionID string) bool
}
```

That keeps the new dependency typed and reviewable instead of `httpapi`
reaching into `worker`'s internals wholesale, which is the actual thing
ARCHITECTURE.md was protecting against. The import graph gains
`httpapi → queue` and nothing else; `worker` and `session` stay out of it.

The embedded MCP service stays as designed — no database handle and no pool
reference. Its `deepseek_stop` and `deepseek_steer`
tools are thin wrappers over the HTTP endpoints below — the same pattern
`deepseek_result` already uses to read the store without touching it directly.

## Stopping

### A healthy run and a wedged one are different problems

A healthy run answers a cancelled `runCtx` at its next check point — inside
`Client.StreamChatCompletion`, or between tool calls — the same way a
`deadline_ms` timeout already does. No new mechanism is needed there, and the
common case is over in seconds.

A wedged run is the case that matters. `internal/tools/bash.go` runs the child
with `exec.CommandContext`, whose cancellation kills only the direct `/bin/sh`
child. A command that backgrounds a process without redirecting its output
(`node server.js &`, inheriting the captured stdout/stderr pipe) leaves that
pipe open after `sh` exits, and `cmd.Run()` blocks on the copy goroutines
forever — past the tool's own timeout, past a cancelled `runCtx`, past
anything the session loop can do from inside Go. Go cannot forcibly kill a
goroutine blocked in a read. Only ending whatever holds the pipe open unblocks
it.

**A stop control that only cancels a context looks identical to today's
timeout on this exact failure — it does not return.** So the design has two
halves, and the first one is not a stop button at all.

### Half one: the tool layer stops producing wedged calls

Three changes to `execBash`, all of them correctness fixes that stand alone
whether or not anything below them ever ships:

1. **`cmd.WaitDelay`.** Go's `os/exec` was given this for exactly this
   failure. With a non-zero `WaitDelay`, `Wait` stops waiting indefinitely on
   I/O the process itself no longer owns: it closes the pipes and returns
   `exec.ErrWaitDelay`. Note what this fixes and when — **not only after `ctx`
   is done**. The commonest shape of the bug is a command that *succeeds*:
   `sh` exits 0 in a millisecond and a backgrounded grandchild holds the pipe
   for hours. `WaitDelay` bounds that case too, so a wedged `Bash` call now
   ends a couple of seconds after the shell exits rather than at the tool
   timeout, or never.
2. **A process group.** `SysProcAttr{Setpgid: true}` makes the child a group
   leader, and `cmd.Cancel` is overridden to signal the whole group
   (`syscall.Kill(-pgid, …)`, SIGTERM then SIGKILL) rather than the single
   `sh` pid. The same kill runs after a `WaitDelay` expiry, because that path
   has no cancellation behind it — without it, the orphan survives, keeps the
   port it bound, and the next run's `npm run dev` fails on an address already
   in use. Today it survives; that is a host-level leak the timeout hides.
3. **A mutex around the output buffer.** Once `WaitDelay` can make `Wait`
   return while a copy goroutine is still writing, the plain `bytes.Buffer`
   shared by `cmd.Stdout`, `cmd.Stderr` and the post-`Wait` read is a data
   race. `liveStdoutWriter` already takes a mutex for its own state; the
   captured buffer needs one for the same reason.

`WaitDelay` is a new tool limit and belongs with the others: **`tools.bash_wait_delay`**,
`GroupToolLimits`, default `2s`. Small on purpose — by the time it fires, the
command's own process is gone or has been killed, and what is left is a
grandchild holding a pipe the harness has no further use for.

`errors.Is(runErr, exec.ErrWaitDelay)` gets its own `Result`: not "command
timed out" and not a bare `run command: …`, but a message naming the actual
mistake, because the model is the one who has to route around it —
"the command exited but left a process holding its output open; the harness
stopped waiting and killed the process group. Redirect output and detach
(`cmd >/tmp/x.log 2>&1 &`) if you meant to leave something running."
[PROMPTING.md](PROMPTING.md)'s rule applies: a tool result is an instruction to
the model, so say what to do differently.

### Half two: the escalation, for the ones that still get through

`Pool.Stop(sessionID, reason)`:

1. Look the session up in the `Controller`. Unknown → `ErrRunNotFound`.
   Already stopping → return nil (idempotent; a second stop is not an error).
2. Record the reason, set `stopping`, call `cancel()`. Return to the caller
   here — the whole path is non-blocking, which is why the endpoint answers
   202.
3. In a goroutine: wait on `done` for **`run.stop_grace_period`**. If the run
   returns inside it, nothing else happens — the ordinary finish path
   publishes a `cancelled` result and acks, exactly as it publishes any other
   terminal result.
4. If the grace period passes, force-finish: mark the session row cancelled,
   record the terminal result, dispose of the queue message, stop the
   heartbeat, release the pool slot, and log a leaked-goroutine warning
   naming the session and request ids.

A run wedged in workspace preparation is why the session row exists before
the workspace does: the pool creates it as `creating` before the clone starts
(docs/DESIGN.md §4.10), so a stop during preparation marks that row cancelled
— `CancelRunningSession` accepts `creating` — instead of finding no row to
mark.

Six things have to be true for step 4 to be honest rather than cosmetic, and
each is a real failure rather than a tidiness point.

**The pool slot must actually free.** Today the semaphore token is released by
`defer func() { <-sem }()` in `Pool.Run`'s consume callback — a defer inside
the wedged goroutine, which by construction never runs. The release becomes an
idempotent closure (`sync.Once` over `<-sem`) held on the `inflight` record, so
the control path can release it and the wedged goroutine's own deferred call
becomes a no-op if it ever wakes.

**The queue message must be disposed of by whoever gets there first.**
Freeing the local semaphore alone would not help: `Size` equals the number of
rows the pool holds leased at once, so an undisposed row counts against the
pool's slots whatever the local count says. The force-finish path therefore
runs the same `finish` sequence the run would have — record the result on
`work_requests`, ack the queue row — guarded by the `disposed` flag, so a
wedged goroutine that wakes up an hour later logs and returns instead of
recording a second, contradictory result over the top of the first.

The flag's home is the `inflight` record rather than the registry, and that
placement is load-bearing: `finish` deregisters the run, so a guard that lived
in the map would stop guarding at exactly the moment the wedged goroutine is
still out there holding a reference.

**A cancelled session's log must stop growing.** The wedged goroutine holds a
`store.Store` handle and will, if it ever unblocks, append tool results, a
second `run_finished`, and a `FinishSession` that would move the row out of
`cancelled`. Two store-level fences close that:

- `AppendEvents` refuses a session whose status is `cancelled`, returning
  `store.ErrSessionCancelled`.
- `FinishSession` and `UpdateSessionStatus` refuse to move a row *out of*
  `cancelled`. Cancelled is terminal and final; every other terminal status
  is reachable only from a live session (`running`, or `creating` while the
  workspace is still being prepared — a stop or a setup failure can end the
  preparation window directly).

This is worth more than tidiness: it is a second, independent stop. A wedged
goroutine that wakes finds its next append refused, fails the run, and unwinds
— so the leak is bounded by the wedged syscall, not by the run's remaining
sub-turn budget.

**The heartbeat closer must be shared, or the stop panics the process.**
`Pool.run` stops its heartbeat with `defer close(hbDone)`. If the force-finish
path closed that channel too, the wedged goroutine's own deferred close would
close an already-closed channel and take the whole harness down with it — the
one bug in this design that costs more than the run it was stopping. Both
paths go through a single `sync.Once`-guarded closer. Leaving the heartbeat
running instead is not an option either: it calls `msg.InProgress()` on an
acked message every twenty seconds, forever.

**A stop must not relabel a run that finished first.** The grace period
creates a race with a real cost: the run reaches a terminal status of its own
in the moment between the timer firing and the cancel landing. Overwriting
`ok` with `cancelled` there would destroy the exact distinction the new status
exists to carry, on the strength of a timer. `store.CancelRunningSession`
refuses any row that is already terminal with a typed `SessionFinishedError`
naming what it finished as; the escalation logs it, publishes nothing, and
leaves the run's own result alone. A second cancel of an already-cancelled row
is a no-op rather than a version bump, so a retried stop cannot move
`finished_at` off the moment the stop actually landed.

**A cancelled parent must take its subagents with it.** A `Task` subagent runs
`Runner.Run` under the parent's `ctx`, so `cancel()` propagates without extra
work; the child fails normally and its own row records that. Nothing here
cancels a child independently, and nothing needs to.

### `run.stop_grace_period`

New setting, `GroupRunBudget`, type duration, default **`30s`**, bounds 1s to
1h, resolved per call rather than at startup (so it is not restart-flagged).

Sizing it against the wrong thing is the one real risk in this half. It is not
"how long does a sub-turn take" — a healthy `Cancel` lands inside a streaming
read almost immediately. It is "how long can a *healthy* run legitimately take
to notice", and the answer is bounded by the longest uninterruptible thing a
tool does between context checks. The tools all take `ctx` and the timeouts
are the ceiling; 30s is comfortably past a healthy cancel and comfortably
short of an operator concluding the button did nothing.

## Steering: augment, don't gate

A steer is a new user message appended to the tail of the conversation, folded
in at the top of the next sub-turn — after the current tool round finishes,
never mid-call. That is the same shape §3.2 already requires of everything
else: no prefix rewrite, tail-only growth, cache-safe. A steer sent while a
long tool call is running is visible in the transcript the moment it is
accepted but reaches the model only at the next natural boundary — the same
non-blocking shape the permission policy already uses, and the reason §4.6's
stalling problem does not reopen.

### Two event kinds, not one

The obvious design — one `steer_message` event that the fold turns into a user
message — breaks the fold's append-only property, and it is worth being
explicit about how, because the bug would be a cache regression nobody could
see.

An operator steers while a tool call is outstanding. The event lands in the
log at a `seq` **between** the assistant message's `tool_call` events and its
`tool_result` events. `Fold(events[:n])` for an `n` inside that window would
emit the user message at index *i*; `Fold(events[:n+k])`, once the tool
results land, must emit those tool messages before it — so the same message
moves to index *i+k*. Folding more events would have rewritten an earlier
one. `TestAppendOnly` in `fold_test.go` asserts precisely that this never
happens, and the prompt cache is what the assertion is protecting
([CACHE.md](CACHE.md)).

One event kind *can* be made to work. If the fold tracks how many tool calls are
outstanding — the count it would need anyway to know the message array is at
rest — it can buffer a `steer_message` and emit it only once every outstanding
call has its result. The position is then stable for every prefix, and
`TestAppendOnly` holds. So the choice below is not forced by the append-only
property; it is made on three other grounds, and a later reader deciding to
collapse the two kinds should weigh these rather than re-derive a prohibition
that does not exist:

- **The fold stays a dumb switch.** Buffering moves state into the one
  function whose simplicity the prompt cache rests on. The append-only proof
  would newly depend on the outstanding-call bookkeeping being right in every
  case — parallel calls, a denial among the results, a crash mid-round.
- **Both folds would need it.** `web/src/api/fold.ts` walks the same log and
  must agree in shape (ARCHITECTURE.md). With two kinds the browser matches
  `source_seq` and is done; with one it has to re-derive the same tracking to
  know a steer has actually been delivered.
- **A wedged run stays legible.** A run that never reaches another boundary
  is the case that matters most. With two kinds, a steer sits in the log
  as sent-and-never-applied, which is exactly what the transcript should show.
  With one, "queued" and "delivered" are the same event and the operator
  cannot tell the difference — on precisely the run where the difference is
  the diagnosis.

The delivery point is identical either way: a steer reaches the model at the
next sub-turn boundary, never mid-call. So the split is made in the log:

| Kind | Appended by | In the fold? | Meaning |
| --- | --- | --- | --- |
| `steer_message` | the HTTP handler, immediately | no | an operator sent this text at this instant |
| `steer_applied` | the session loop, at a sub-turn boundary | yes — a user message | the model was shown this text here |

`steer_applied` is only ever appended where the message array is at rest, so
its fold position is final the moment it exists, and the append-only property
holds unchanged. `steer_message` contributes nothing to the messages array, the
way `tool_stdout` already contributes nothing.

```go
// SteerMessagePayload is an operator instruction accepted for a running
// session. It carries no messages-array content: the loop decides where the
// model sees it, and records that with a steer_applied event.
type SteerMessagePayload struct {
    Text   string `json:"text"`
    Source string `json:"source,omitempty"` // "web", "mcp", "cli"
}

// SteerAppliedPayload is the point in the log where a steer_message became a
// user message. SourceSeq links it to the steer_message it applies, which is
// what lets a resumed run recompute which steers are outstanding from the log
// alone, with no in-memory high-water mark to lose.
type SteerAppliedPayload struct {
    SourceSeq int64  `json:"source_seq"`
    Text      string `json:"text"`
    SubTurn   int    `json:"sub_turn"`
}
```

Both go in `store.EventKinds`, which automatically extends the events
endpoint's `?kind=` filter and its 400 message — the reason that list is
derived rather than hand-written.

### How the loop picks one up

At the top of `runSubTurn`, before folding: one indexed query for
`steer_message` events with `seq >` the highest `source_seq` this session has
already applied, capped at a small batch. For each, append a `steer_applied`
event, and push it onto the loop's local `allEvents` so the fold that follows
includes it. The applied high-water mark is derived from the log on entry to
`runLoop`, so `Resume` and a compacted successor both recompute it rather than
inheriting anything.

This is a store read per sub-turn against an indexed `(session_id, kind, seq)`
predicate, on a loop whose other step is a multi-second API call. It is not a
cost worth designing around.

Two consequences worth naming:

- **Steering needs no in-process handle.** It is a store write by the handler
  and a store read by the loop. So the multi-instance caveat that applies to
  stop does not apply here — the seam is the database, which every replica
  shares. That is a happy accident of the event log, not a design goal, but it
  is the reason steering and stopping are specified differently rather than
  forced through one mechanism.
- **A steer on a wedged run is not lost, only unapplied.** It sits in the log
  as a `steer_message` with no `steer_applied`, which is exactly what the UI
  should show: *sent, not yet delivered*.

### The message array shape it produces

`… assistant(tool_calls) → tool → tool → user(steer) → assistant …`

A `user` message following `tool` messages, ahead of the next assistant turn.
`Resume` already produces the same shape by appending a second
`session_started` for its continuation instruction, so this is not new ground
on the wire — but it is worth confirming against the live API once and writing
the answer into [OBSERVED.md](OBSERVED.md) rather than assuming, since nothing
in the vendored docs promises it.

Rejected: reusing `session_started` for the applied steer, which would need no
new fold case at all. It costs less code and lies about what the event is — a
kind named "session started" appearing four times mid-run — and it gives the
`?kind=` filter no way to find steers. The fold's switch gains one case either
way.

## The HTTP surface

Three new endpoints, all `POST`, all actions rather than row edits.

```
POST /api/sessions/{id}/stop     {"reason": "..."}          → 202
POST /api/sessions/{id}/steer    {"text": "..."}            → 202
POST /api/runs                   <work request body>        → 202
```

`methodGate` learns `POST` on exactly these three path shapes, extending the
`writeAllowed`/`allowedMethods` switch the way the phase-3 resources did, so a
`POST` anywhere else stays a 405 with a correct `Allow` header.

**The guards every write carries apply unchanged**: `Content-Type:
application/json` or 415, same-origin or 403 (DATA-API.md, "The guards every
write carries"). `writeGuards` is called by these handlers exactly as the
settings and session handlers call it. On top of that, and only on these
three, a bearer token — see [Authentication](#authentication).

**`If-Match` is not required here**, a departure from DATA-API.md's rule
worth stating rather than leaving to be discovered. That
rule governs *mutations of a row*: it exists so an operator's write cannot
land on a row that changed since they read it. These three are not row
mutations. A running session's `version` changes continuously underneath the
operator — the runner bumps it on every terminal write and the session's live
state moves constantly — so requiring a version echo would make a correct stop
racy by construction, and an operator would learn to fetch-then-immediately-post,
which is the check without the protection. The preconditions that matter here
are about the *run*, not the row, and they are:

| Endpoint | Precondition | Failure |
| --- | --- | --- |
| stop | the session exists | 404 |
| stop | this process is running it | 409, naming the session's status |
| steer | the session exists and is `running` | 404 / 409 (a `creating` session's 409 says its workspace is still being prepared, not that the run is over) |
| runs | the body passes `queue.Request.Validate` | 400, the validator's message |

`202 Accepted` on all three, with a body naming what was accepted:
`{"session_id": "...", "stopping": true}`, `{"session_id": "...", "seq": 412}`,
`{"request_id": "..."}`. Nothing waits for the outcome — §5's existing rule
that "a request that starts a run is not answered by the response to it"
applies to ending one too. The terminal state arrives over the session's own
SSE stream and, for a queue caller, on the request's `work_requests` row.

Stop is idempotent: a second stop for a session already stopping is another
202, not a 409. Steer is not — two steers are two instructions, which is why
the response carries the `seq` the caller's text landed at.

### `POST /api/runs`

Body is `queue.Request` verbatim minus the provenance fields: `repos`,
`permission_mode`, the optional `prompt` — a browser start may omit it and
create the run empty, with the operator's first message typed into the session
once it appears ("Start" below) — and the optional `model`, `effort`, `deny`,
`result_schema`, `max_sub_turns`, `deadline_ms`, `job_type` — the same fields
`harness publish` sets from flags and `deepseek_agent` sets from tool
arguments. `parent_is_user`, `parent_agent_type`, and `parent_agent_id` are
**not** accepted from the body: the handler stamps them itself — `true`,
empty, and the `identity.operator` name — before validation, and ignores
anything the body sent (an overwrite, not a 400, so the browser never has to
know the fields exist). `request_id` is optional on this surface and generated
when absent, because a browser form has no idempotency key to offer; supplying
one gets the same deduplication every other producer gets.

The operator name comes from the **`identity.operator`** setting
(`GroupIdentity`, string, not secret): the name recorded as the parent of a
run started from the web UI. There is no login system, so this is a label the
operator configures once — `harness config set identity.operator geoff` — read
server-side on every start. An unset or malformed name (one that fails the
parent-agent-id validation) degrades to an unnamed person rather than failing
the start; on a bare-metal `serve` that has never set it, the login user's
name is stored automatically when it is usable.

Validation is `req.Validate()` — the queue's own, not a copy. A browser-started
run is byte-identical in the store to one started from MCP or the CLI: same
event kinds, same validation, same idempotency behaviour on a duplicate
`request_id`.

The body may also carry an `attachments` array — images (a mockup, say)
stored before the publish so the request carries only `attachment_ids`, never
the bytes (docs/DATA-API.md, "attachments"). The caps come from
`tools.attachments_max_count` and `tools.attachments_max_bytes`, and the
materialised files are named in the run's opening message.

### Authentication

Loopback plus the same-origin and content-type guards stay, and are not enough
on their own for a surface that can now spend money and run arbitrary commands
under `full` permission mode. One new setting:

**`http.control_token`** — `GroupCredentials`, string, secret-flagged like the
API keys, generated (32 bytes, base64url) on `serve` startup when unset and
stored through the ordinary settings path. Required as
`Authorization: Bearer <token>` on the three endpoints above; a missing or
wrong token is 401 with the standard `{"error": …}` body. Stage-one's existing
endpoints are unaffected — their blast radius is a database row; this
surface's is a running command.

Beside it sits the second run-control-adjacent setting: **`identity.operator`**
(`GroupIdentity`, string, not secret) — the name stamped into
`parent_agent_id` on a browser start. To be plain about what it is and is not:
it is a *label*, not a credential. It names who the operator says they are on
a surface that has no login; it authenticates nothing, grants nothing, and is
not required by any endpoint. An unset or malformed value degrades to an
unnamed person rather than failing the start.

The honest accounting of what that buys, because a token in a
world-readable-to-your-own-uid SQLite file is easy to over-sell:

- Against a **cross-origin page in the operator's browser**: nothing the
  origin and content-type guards did not already do.
- Against **another process running as the same user**: nothing. It can read
  the settings table.
- Against **the port being exposed off loopback** — on purpose, or by a
  `-addr 0.0.0.0`, or by a container port publish: everything. This is the
  case it is for, and it is the authentication question §4.2 names as open.

Distribution follows from that. `GET /api/control-token` returns the token
**only to a local `RemoteAddr`** — loopback, or a private (RFC 1918 / RFC
4193) address — so the frontend fetches it at startup and a remote caller must
be given it out of band. The private-range allowance exists because
`docker-compose.prod.yml` publishes the port as `127.0.0.1:8180`, and a
browser on the host hitting that address arrives inside the container NAT'd
through the compose network's gateway rather than as `127.0.0.1`; a
loopback-only check rejected that genuinely local traffic with a 403 the
frontend rendered as "run control not configured". The real boundary is still
the published port being loopback-only on the host, so trusting the private
range on top of it does not admit a caller that could not already reach here.
`harness stop` and `harness steer` read it from the settings table directly,
the way every other CLI subcommand reads configuration. The embedded MCP
service gets the token handed to it directly at startup by `harness serve` —
the same value the HTTP server itself holds — so `deepseek_stop` and
`deepseek_steer` authenticate without a round-trip; the `GET
/api/control-token` loopback fetch in `internal/mcp/control.go` stays as
defensive code for a `Service` built without that step, but is no longer the
primary path.

This is a floor, not an answer to §4.2's larger question about exposing the
port at all. It does mean that the day someone does expose it, the control
surface fails closed instead of open.

## MCP and CLI

**`deepseek_stop`** — `session_id` or `request_id` (resolved the same way
`deepseek_result` already resolves one), optional `reason`. Described the way
the existing tools are: it never blocks, and `deepseek_status` afterwards is
how the caller learns what happened.

**`deepseek_steer`** — `session_id` or `request_id`, and `text`. The
description has to carry the one thing an agent will otherwise get wrong: the
run does not stop to read this, and it lands at the next sub-turn boundary.

Both are HTTP calls to the endpoints above, which means `internal/mcp` needs
its first non-GET helper — `postJSON` alongside `getJSON`/`getRaw`, carrying
the bearer token. It still opens no database.

**`harness stop <session-id> ["reason"]`** and **`harness steer <session-id>
"text"`**, one file per subcommand (`cmd/harness/stop.go`, `steer.go`) as the
dispatch convention requires, both talking to the same HTTP endpoints so there
is one implementation of each verb rather than a CLI path that reaches around
it.

## The frontend

**Stop** sits on the in-flight session card (§5.8) and in the transcript
header, behind a confirmation — this codebase treats hard-to-reverse actions
as worth a confirm step, and a run an operator cannot get back is one. Between
the 202 and the terminal event the control shows *stopping…*; then the
`CANCELLED` badge `statusBadge.ts` already knows how to render.

That badge is the one piece of this already built. `outcome({status:
"cancelled"})` returns `CANCELLED` on the `stopped` variant today, ahead of
any backend that can produce it. Careful not to collide with the existing
`STOPPED` label, which means `run_finished.reason == "no_tool_calls"` — the
model stopping on its own, nothing to do with an operator.

**Steer** is an input on the transcript screen, visible only while the session
is running. Sent text appears immediately as its own block, styled distinctly
from the model's turns, in one of two states: *pending* while only the
`steer_message` event exists, *delivered* once the matching `steer_applied`
arrives (matched by `source_seq`). That two-state rendering is the whole
reason the log carries two kinds, and it is what makes a wedged run legible —
a steer that sits pending for minutes is telling the operator something.

`web/src/api/fold.ts` gains one `Block` variant, `steer`, produced by
`steer_message` and mutated to delivered by `steer_applied` — the one place
the browser fold takes a second event to complete a block it has already
emitted, which is fine because display blocks carry no cache invariant. The Go
fold and this one still agree in shape: both know both kinds, and each does
with them what its own consumer needs.

**Start** is a form: repos, a model, a thinking effort, a permission mode
defaulting to full, and the optional fields, mirroring what `publish`'s flags
accept — with no prompt field. The run is created empty; submitting returns a `request_id`, the screen
follows the session from the existing `GET /api/stream` list feed as soon as
one exists, and once it does the form opens that session's transcript, where
the operator types the first message into the steer input.

## The result status

`internal/queue` has `StatusCancelled = "cancelled"` alongside `StatusOK`,
`StatusFailed`, `StatusDenied` and `StatusTimeout` — distinct from
`StatusTimeout`, which is deadline-driven with no operator involved. The
result carries `error.code` `"cancelled"` and the operator's reason as its
message, so a queue consumer can tell an operator's decision from a budget
being exhausted without parsing prose.

`store.StatusCancelled` and `httpapi`'s `terminalSessionStatuses` both accept
it, covering two distinct causes with one vocabulary: a session marked
`cancelled` by an operator's PATCH-based repair (DATA-API.md), and one marked
`cancelled` by an operator's stop.

## What this does not promise

- **That the goroutine exits.** The force-finish path marks the run stopped,
  publishes a terminal result, and frees the slot without waiting. If the
  wedged syscall never returns, the goroutine leaks for the life of the
  process. It is logged, loudly, with the session id and the tool call it was
  last in — an abandoned goroutine that looks identical to a clean stop is the
  outcome worth refusing, not the leak itself.
- **That the workspace is cleaned up.** A stopped run's workspace directory
  and its lease outlive it, exactly as an abandoned run's do today; the lease
  endpoints in DATA-API.md are how they get released.
- **That one wedged tool call can be killed on its own.** Stop is whole-run.
  Killing a single call and letting the run continue is a smaller, harder
  feature — the run loop would have to be told to synthesise a tool result for
  a call that never returned — and the whole-run stop is the one that matches
  what happened in production.
- **That a run stops on another `serve` instance.** The registry is
  in-process. Today there is exactly one instance; see the open questions.

## Open questions

- **Multi-instance `serve`.** If this ever runs as more than one replica, the
  in-process registry stops being sufficient for stop: control has to find the
  right instance. Steering already works in that world, and start does too.
  The likely answer is a control channel — a shared subject or a store table
  — with instances subscribing for their own session ids. Not built, because
  it is a mechanism for a problem nobody has.
- **Rate-limiting stop and steer.** A control token is authorization, not a
  throttle. A misbehaving caller, or a leaked token, could steer a session in
  a tight loop and drive its cost up; nothing here bounds that.
- **A steer that should have been a stop.** An operator who steers "stop what
  you are doing" gets a message the model may or may not act on. That is the
  correct behaviour for a steer and a bad outcome for the operator; whether
  the UI should notice and suggest the stop control is a product question.
- **Whether `full` mode should be startable from a browser form at all.** The
  docker socket is mounted into the harness container (CLAUDE.md), so a `full`
  run has the host daemon. `POST /api/runs` accepts the same modes the queue
  does, and the argument for restricting the browser specifically is not
  obviously stronger than the argument that it would just push the operator
  back to the CLI.
