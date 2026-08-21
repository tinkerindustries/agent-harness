# Run control: starting, steering, continuing, and stopping a run

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
- **Continuing.** A person sends the next message to a session whose run has
  already ended, continuing it in place rather than starting a new run
  ("Continuing" below). The browser and the CLI have this; the MCP surface
  deliberately does not — an agent that wants more work done starts a run.
- **Stopping.** Ends a run on request — including one wedged inside a tool
  call that ignores its context, which a naive `cancel()`-only stop would not
  fix.
- Starting, steering and stopping ship on **MCP, the CLI, and the web UI**,
  not one first. MCP callers are other agents, not just people at a keyboard,
  and an agent that can start a job but not steer or stop it is only half a
  capability. Continuing is the exception, and the reason is what the verb is
  for: it exists so a person can keep talking to a session, which is a thing
  people do and agents do not.

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

## Continuing

Steering is for a run that is still going. Continuing is the other half: a
person's next message after the loop already stopped.

The browser needed it because the chat page is a chat. It calls itself the
interactive session page, it renders sent messages as messages, and its
composer sits in the footer whatever the run is doing — and then the moment
the model called `Complete` the next message was refused, because a steer for
a finished run would sit in the log forever unapplied. The way on was to start
a fresh run against a fresh clone, discarding the workspace, the model's
context and the conversation, which is the wrong unit of work for anything
iterative: look, change something, look again.

`internal/session`'s `Resume` already did all of it. It reads the frozen
session row back — workspace, model, effort, permission mode, deny patterns,
the stored `tool_schema` — appends the continuation as a second
`session_started`, promotes the row to `running` and re-enters the same loop.
None of that is new. What was missing was a way to ask for it from anywhere
but the CLI.

### Continuing is a publish, for the same reason starting is

The resume endpoint publishes a work request naming a session, rather than
reaching into a loop the way stop does. `queue.Request` gains one field,
`resume_session_id`, and the worker branches on it: no session id to mint, no
workspace to build, no row to create — just `Runner.Resume` on the session the
request names.

That choice buys the whole of the queue's machinery for free, and every part
of it turns out to matter:

- **The stop registry.** The pool registers its in-flight record under the
  resumed session's own id, so a continued run is stoppable exactly like a
  fresh one, with no second registry and no special case in the escalation.
- **Redelivery and the delivery ceiling.** A worker that dies mid-continuation
  is a redelivery, not a session stuck in `running`.
- **The `work_requests` row.** Each continuation records its own result, so a
  session that ran four times has four rows, each answerable by
  `deepseek_result` like any other request.
- **No affinity.** The process that ran the session originally does not have
  to be the one that continues it — which the in-process stop seam *does*
  require, and is the assumption that would break first under more than one
  replica of `serve`.

The alternative — a third seam out of `internal/httpapi`, a `RunResumer`
implemented by the pool — was rejected for costing all four of those and
adding a second code path that starts a loop.

`Validate` relaxes exactly one rule for a resume: repos are required unless
`resume_session_id` is set, and must be *empty* when it is, because the
workspace already exists with the original clones in it. Everything else still
applies, which is why the handler copies the session's own `permission_mode`
and `model` onto the request rather than leaving them blank — the request
satisfies the queue's rules instead of the queue weakening them for one
producer. Both values are read back off the row by `Resume` regardless; the
copies exist for validation and accounting.

### What the browser does with it

The composer stops being a steer control and becomes a message box: the screen
decides at send time which verb a typed message is — a steer while
`canSteer(status)`, a resume while `canResume(status)` — and the two cover
every status but `creating` and `compacted`. Text typed while a run is
finishing is not thrown away; it becomes the resume.

Two consequences in the display, both of which were latent assumptions that
resume falsified:

- **A terminal event no longer ends the client's stream.** The browser used to
  close its own `EventSource` when a `run_finished` or `error` event landed,
  on the reasoning that nothing could follow. A continued session appends
  after its terminal event, and every later replay of its history carries that
  event in the middle of the log — which would tear down a stream following a
  run in progress. Ending a stream is now the server's call, announced with a
  `closed` marker frame, the same pattern and for the same reason as
  `replayed`: the client cannot work it out for itself.
- **A finished session's page has to notice it came back.** The page learns it
  from the session-list feed, an app-lifetime stream it is already connected
  to, and reopens the transcript stream when the row goes live again. Not a
  poll: a resume is accepted before it begins, so there is nothing to look at
  until a worker claims it, and the rule that "a started run appears when the
  pool claims it" applies here unchanged.

On the page itself a run ending is a turn boundary rather than an event: a
clean finish renders its closing text as the model's reply, without the
banner, outcome label and cost line that make a conversation read as a series
of jobs. Any other outcome keeps the full card, because that is when the
reason matters. The finished band — the composer's replacement — is now
reached only by a session that can be continued by nothing at all.

## The HTTP surface

Four endpoints, all `POST`, all actions rather than row edits.

```
POST /api/sessions/{id}/stop     {"reason": "..."}          → 202
POST /api/sessions/{id}/steer    {"text": "...", attachments?} → 202
POST /api/sessions/{id}/resume   {"text": "...", attachments?} → 202
POST /api/runs                   <work request body>        → 202
```

`attachments` on the two message endpoints is the same array `POST /api/runs`
takes, and is what "Images in the composer" below is about. Because it can
carry several multi-megabyte images, both bodies are read under the 64 MB
ceiling `POST /api/runs` already reads under rather than the 1 MB one they
read text under. `text` may be empty when `attachments` is not: a pasted
screenshot with no words is a complete message, and only a body carrying
neither is a 400.

`methodGate` learns `POST` on exactly these four path shapes, extending the
`writeAllowed`/`allowedMethods` switch the way the phase-3 resources did, so a
`POST` anywhere else stays a 405 with a correct `Allow` header. The three
session subresources share one path-shape predicate
(`isSessionActionPath`), because they share one rule: `POST` passes on an
action path and nowhere else, and `isSessionPath` — which keeps `PATCH` and
`DELETE` scoped to the row — must not be widened to cover any of them.

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
| resume | the session exists | 404 |
| resume | it is not live — a live session wants steer, and the 409 says so | 409 |
| resume | it was not retired by compaction | 409, naming its child as the thing to continue |
| resume | the session's own stored configuration still validates | 409, naming the session (not the message) as the cause |
| runs | the body passes `queue.Request.Validate` | 400, the validator's message |

`202 Accepted` on all four, with a body naming what was accepted:
`{"session_id": "...", "stopping": true}`, `{"session_id": "...", "seq": 412}`,
`{"session_id": "...", "request_id": "..."}`, `{"request_id": "..."}`. Nothing waits for the outcome — §5's existing rule
that "a request that starts a run is not answered by the response to it"
applies to ending one too. The terminal state arrives over the session's own
SSE stream and, for a queue caller, on the request's `work_requests` row.

Stop is idempotent: a second stop for a session already stopping is another
202, not a 409. Steer is not — two steers are two instructions, which is why
the response carries the `seq` the caller's text landed at. Neither is resume:
each one starts a run.

Resume's last precondition is the odd one, and it is a 409 rather than a 400
on purpose. Every field of the request it publishes except the message comes
off the session row, so a validation failure there is a statement about the
session, not about what the caller sent — a permission mode retired since that
session was created, say. Answering 400 would tell somebody who just typed a
sentence that their sentence was malformed.

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

### Images in the composer

A person mid-conversation can paste images into the composer and they ride the
next message, whichever verb that message turns out to be. Pasting is the only
way in on this surface — no file input beside the box — because the thing
people actually do is screenshot something and hit paste, and the start form
already covers choosing a file from disk.

The bytes take the route a work request's attachments already take: the
endpoint validates each image against the same allowlist and caps
(`internal/attachment`), writes it to the `attachments` table, and carries
only the ids onward. Nothing multi-megabyte goes into the event log or onto a
`work_queue` row — both are read whole, repeatedly, and one screenshot in
either would be paid for on every fold.

**Where the files get written is the part worth stating.** A run's attachments
are materialised by `internal/workspace.Prepare`, before there is a loop at
all. Neither of these messages has a Prepare: a steer goes to a session whose
workspace exists and is in use, and a resume keeps the workspace it already
has. So the loop writes them, through
`internal/workspace.WriteAttachments` — the same confinement check, so a
name that could climb out of `scratch/attachments/` is refused by the same
code — and it writes them at the moment it is about to read the message:

- **A steer**: `pickUpSteers` materialises the images at the sub-turn boundary
  it applies the message at, then folds the attachment block and the
  operator's words as one user message. The order is the guarantee — the file
  exists before the model is told the path. A write that fails is *not* fatal:
  the message still reaches the model, saying that the images could not be
  written, because a full disk should not end a run that was going fine, and a
  message naming files the model cannot open is worse than one that never
  claimed they were there.
- **A resume**: `Runner.Resume` materialises them before appending the
  continuation's `session_started`. Here a failure *is* fatal — nothing has
  happened yet, so refusing leaves the session exactly as it was, and the
  browser shows the refusal with the message still in the box.

The transcript follows the same fact. A continuation's images render as a
gallery as soon as the block exists, because the files were written before it
was. A steer's do not: its `steer_message` records the paths at acceptance so
a *pending* message can name what it carries, but the gallery only appears
once the message is delivered, because until then there is nothing on disk to
fetch and every tile would report the image as gone.

One more consequence of the clipboard: **the browser renames every pasted
image**. Chrome hands each screenshot over as `image.png`, so honouring the
name would have the second paste into a session overwrite the first while the
model had been told about both. Each becomes `pasted-<stamp>-<n>.<ext>`
(`web/src/api/attachments.ts`).

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
it. **`harness resume <session-id> ["..."]`** predates all of this and is the
exception: it runs the loop in its own process against its own data directory
rather than posting to a running `serve`. That is what it is for — continuing
a session on a machine with no server up — and the browser's resume does not
replace it.

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

**Steer** is what the transcript screen's composer sends while the session is
running; once the run is over the same box sends a resume instead
("Continuing" above), so the control is visible for both and it is the screen
that decides which verb a message is. Sent steer text appears immediately as its own block, styled distinctly
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
