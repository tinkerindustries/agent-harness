# Run control: starting, steering, continuing, and stopping a run

What the parent can do to a run once it has started, and why each of them is
shaped the way it is. The wire form of all four is
[STDIO-PROTOCOL.md](STDIO-PROTOCOL.md); this is the design behind them.

Closing a run that never returns is the case that matters most: a tool call
blocked on something that ignores cancellation does not end when its context
is cancelled, and this design exists to close that, not just to add a stop
button.

## Scope

- **Starting.** A create call publishes the prompt, the model, the permission
  mode and the directory the session works in.
- **Steering.** The parent appends a new user-authored message to a run in
  progress.
- **Continuing.** The parent sends the next message to a session whose run has
  already ended, continuing it in place rather than starting a new one
  ("Continuing" below).
- **Stopping.** Ends a run on request — including one wedged inside a tool
  call that ignores its context, which a naive `cancel()`-only stop would not
  fix.

### Explicitly out of scope

- **Approval-gated tool calls.** §4.6 rejected a loop that blocks on a human
  for exactly this reason: it stalls when nobody is watching. Steering adds a
  channel *into* the loop; it does not make the loop wait on it.
- **A stop control reaching into a background shell.** `Bash`'s
  `run_in_background`, `BashOutput`, and `KillBash` ([TOOLS.md](TOOLS.md))
  are the poll/kill affordances this section once named as not built, for a
  command a model chose to detach on purpose. This design's "kill a wedged
  call" is about ending a *foreground* call from outside when nothing chose
  to detach it; "Half one" below is what a background shell's own
  `cmd.WaitDelay` and process group reuse, not a substitute for either.
  `Executor.Close` ends every background shell still running when a run's
  own loop returns, on every path — the run-ending side of run control this
  section covers, applied to a process rather than a goroutine.
- **Killing one tool call and letting the run continue.** Stop is whole-run.
  See [What this does not promise](#what-this-does-not-promise).

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

**A stop control that only cancels a context looks identical to a timeout on
this exact failure — it does not return.** So the design has two halves, and
the first one is not a stop button at all.

### Half one: the tool layer stops producing wedged calls

Three properties of `execBash`, all of them correctness fixes that stand alone
whether or not anything above them ever calls stop:

1. **`cmd.WaitDelay`.** Go's `os/exec` was given this for exactly this
   failure. With a non-zero `WaitDelay`, `Wait` stops waiting indefinitely on
   I/O the process itself no longer owns: it closes the pipes and returns
   `exec.ErrWaitDelay`. Note what this fixes and when — **not only after `ctx`
   is done**. The commonest shape of the bug is a command that *succeeds*:
   `sh` exits 0 in a millisecond and a backgrounded grandchild holds the pipe
   for hours. `WaitDelay` bounds that case too, so a wedged `Bash` call ends a
   couple of seconds after the shell exits rather than at the tool timeout, or
   never.
2. **A process group.** `SysProcAttr{Setpgid: true}` makes the child a group
   leader, and `cmd.Cancel` is overridden to signal the whole group
   (`syscall.Kill(-pgid, …)`, SIGTERM then SIGKILL) rather than the single
   `sh` pid. The same kill runs after a `WaitDelay` expiry, because that path
   has no cancellation behind it — without it, the orphan survives, keeps the
   port it bound, and the next run's `npm run dev` fails on an address already
   in use.
3. **A mutex around the output buffer.** Once `WaitDelay` can make `Wait`
   return while a copy goroutine is still writing, the plain `bytes.Buffer`
   shared by `cmd.Stdout`, `cmd.Stderr` and the post-`Wait` read is a data
   race. `liveStdoutWriter` already takes a mutex for its own state; the
   captured buffer needs one for the same reason.

`WaitDelay` is a tool limit and lives with the others:
**`tools.bash_wait_delay`**, `GroupToolLimits`, default `2s`. Small on purpose
— by the time it fires, the command's own process is gone or has been killed,
and what is left is a grandchild holding a pipe the harness has no further use
for.

`errors.Is(runErr, exec.ErrWaitDelay)` gets its own `Result`: not "command
timed out" and not a bare `run command: …`, but a message naming the actual
mistake, because the model is the one who has to route around it —
"the command exited but left a process holding its output open; the harness
stopped waiting and killed the process group. Redirect output and detach
(`cmd >/tmp/x.log 2>&1 &`) if you meant to leave something running."
[PROMPTING.md](PROMPTING.md)'s rule applies: a tool result is an instruction to
the model, so say what to do differently.

### Half two: cancelling the run

A cancel is addressed at the one goroutine running the session, in this
process. It records the reason, cancels the run's context, and returns without
waiting — the run's ordinary finish path then publishes a `cancelled` result
exactly as it publishes any other terminal result.

A run wedged in workspace preparation is why the session row exists before the
workspace does: `Runner.Create` inserts it as `creating`, so a cancel during
preparation marks that row cancelled — `CancelRunningSession` accepts
`creating` — instead of finding no row to mark.

Two store-level fences keep a cancelled session's log from growing if the
wedged goroutine ever wakes:

- `AppendEvents` refuses a session whose status is `cancelled`, returning
  `store.ErrSessionCancelled`.
- `FinishSession` and `UpdateSessionStatus` refuse to move a row *out of*
  `cancelled`. Cancelled is terminal and final; every other terminal status
  is reachable only from a live session (`running`, or `creating` while the
  workspace is still being prepared).

This is worth more than tidiness: it is a second, independent stop. A wedged
goroutine that wakes finds its next append refused, fails the run, and unwinds
— so the leak is bounded by the wedged syscall, not by the run's remaining
sub-turn budget.

**A cancel must not relabel a run that finished first.**
`store.CancelRunningSession` refuses any row that is already terminal with a
typed `SessionFinishedError` naming what it finished as. A second cancel of an
already-cancelled row is a no-op rather than a version bump, so a retried stop
cannot move `finished_at` off the moment the stop actually landed.

**A cancelled parent takes its subagents with it.** A `Task` subagent runs
`Runner.Run` under the parent's `ctx`, so `cancel()` propagates without extra
work; the child fails normally and its own row records that. Nothing here
cancels a child independently, and nothing needs to.

### A cancelled tool round still commits its results

A sub-turn writes its `tool_call` events before the tools run and its
`tool_result` events after, in two batches. A cancel between the two is the
ordinary case, not a rare one: a stop is most often aimed at a session that
is busy, and a session is busy because a tool is running.

**Every `tool_call` in the log must have a `tool_result` or a `tool_denied`
beside it.** The fold turns each `tool_call` into a function-call item and
each result into the output item answering it, and a provider refuses a
request carrying a call with no output. DeepSeek answers `400
invalid_request_error: No tool output found for tool call <id>`. The log is
the only thing a request is ever built from, so a call left unanswered is not
one bad request — it is every request the session makes from then on, and no
retry, resume or new prompt gets past it.

Two things hold the invariant:

- The tool-result batch commits on a context detached from the run's, with
  its own short timeout (`internal/session/turn.go`). `Store.submit` returns
  `ctx.Err()` on a cancelled context without ever offering the batch to the
  writer, so sharing the run's context here is what orphaned the call. The
  `cancelled` status fence described above is not reached yet at this point:
  the row is still `running` until the run's finish path marks it.
- The fold closes off any call still unanswered once the log shows the
  conversation moved past it — the next sub-turn starting, or a user message
  being placed (`internal/fold/fold.go`). This is what makes a log written
  before the first fix replayable. It waits for one of those events rather
  than synthesising eagerly, because a call whose result is merely not
  committed *yet* is the ordinary mid-round state, and inventing an output
  for it would both lie to the model and break the fold's append-only
  property when the real result landed.

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

Somebody steers while a tool call is outstanding. The event lands in the log
at a `seq` **between** the assistant message's `tool_call` events and its
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
property; it is made on two other grounds, and a later reader deciding to
collapse the two kinds should weigh these rather than re-derive a prohibition
that does not exist:

- **The fold stays a dumb switch.** Buffering moves state into the one
  function whose simplicity the prompt cache rests on. The append-only proof
  would newly depend on the outstanding-call bookkeeping being right in every
  case — parallel calls, a denial among the results, a crash mid-round.
- **A wedged run stays legible.** A run that never reaches another boundary
  is the case that matters most. With two kinds, a steer sits in the log
  as sent-and-never-applied, which is exactly what a transcript should show.
  With one, "queued" and "delivered" are the same event and a reader cannot
  tell the difference — on precisely the run where the difference is the
  diagnosis.

The delivery point is identical either way: a steer reaches the model at the
next sub-turn boundary, never mid-call. So the split is made in the log:

| Kind | Appended by | In the fold? | Meaning |
| --- | --- | --- | --- |
| `steer_message` | the protocol handler, immediately | no | somebody sent this text at this instant |
| `steer_applied` | the session loop, at a sub-turn boundary | yes — a user message | the model was shown this text here |
| `steer_withdrawn` | the stdio host, when a run ends | no | the run ended before this text reached the model, and no later run will show it |

`steer_applied` is only ever appended where the message array is at rest, so
its fold position is final the moment it exists, and the append-only property
holds unchanged. `steer_message` contributes nothing to the messages array, the
way `tool_stdout` already contributes nothing.

`SteerMessagePayload` carries the text and its source. `SteerAppliedPayload`
carries `SourceSeq`, which links it to the `steer_message` it applies — and
which is what lets a resumed run recompute which steers are outstanding from
the log alone, with no in-memory high-water mark to lose.

### How the loop picks one up

At the top of `runSubTurn`, before folding: one indexed query for
`steer_message` events with `seq >` the highest `source_seq` across this
session's `steer_applied` and `steer_withdrawn` events, capped at a small
batch. For each, append a `steer_applied`
event, and push it onto the loop's local `allEvents` so the fold that follows
includes it. The applied high-water mark is derived from the log on entry to
`runLoop`, so `Resume` and a compacted successor both recompute it rather than
inheriting anything.

This is a store read per sub-turn against an indexed `(session_id, kind, seq)`
predicate, on a loop whose other step is a multi-second API call. It is not a
cost worth designing around.

**A steer on a wedged run stays unapplied while the run is wedged.** It sits
in the log as a `steer_message` with no `steer_applied`, which is exactly what
a client should show: *sent, not yet delivered*.

### A steer the run never reached

A run can end with no sub-turn boundary after a steer: the model answers with
no tool calls, calls `Complete`, runs out of sub-turns, is stopped, or fails.
Left in the log, that steer would be applied at the first boundary of
whichever run the session had next. A client that had sent it again as a new
message would then have it delivered twice.

`internal/stdiosession` closes each one when the run ends. After the loop
returns, the host stops accepting appends and calls
`store.WithdrawUnappliedSteers`, which appends one `steer_withdrawn` per
outstanding steer, naming it by `source_seq`, in one write transaction. The
high-water mark counts a withdrawn steer as consumed, so the next run's pickup
starts past it. The terminal response names the withdrawn steers by the
client's message ids ([STDIO-PROTOCOL.md](STDIO-PROTOCOL.md),
"`responses.append`"), and the client decides whether to send any of them
again.

Three things keep an accepted steer from being lost between the two:

- The append handler checks that the run is in progress and commits the
  `steer_message` under one lock. The host takes the same lock before the run
  leaves in progress and holds it through the sweep. An append either commits
  before the sweep looks or is refused.
- The handler records the client's message id under that lock too, and the
  frame pump waits on the lock before translating a `steer_applied`, so an
  applied steer's echo always carries its id.
- `WithdrawUnappliedSteers` writes to a cancelled session, which
  `AppendEvents` refuses ("Half two", above). A withdrawal adds nothing to the
  conversation, and a stopped run is the likeliest to leave a steer behind.

A process that dies before its run ends records no withdrawal. A session
resumed from its state directory after that still applies those steers at its
first boundary.

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
kind named "session started" appearing four times mid-run — and it gives a
consumer filtering by kind no way to find steers. The fold's switch gains one
case either way.

## Continuing

Steering is for a run that is still going. Continuing is the other half: the
next message after the loop already stopped.

Without it, the way on from a finished run is a fresh run against a fresh
workspace, discarding the model's context and the conversation — the wrong
unit of work for anything iterative: look, change something, look again.

`internal/session`'s `Resume` is the whole of it. It reads the frozen session
row back — workspace, model, effort, permission mode, deny patterns, the
stored `tool_schema` — promotes the row to `running`, appends the
continuation as a second `session_started`, and re-enters the same loop.

The row is promoted *before* the continuation is appended, and that ordering
is what makes a stopped session resumable at all: `cancelled` is one of the
terminal statuses Resume accepts, and `AppendEvents` refuses to write to a
session still in it ("Half two" fence, above). Promoting first, rather than
appending against the still-cancelled row and promoting once that succeeds,
is not a race against the stop that put it there — a session Resume reads as
`cancelled` has already had its last store write from whichever goroutine
cancelled it (`fail`, or `resumeTarget`'s reclaim of a row an earlier process
left `running`), so there is nothing left to race. If the append then fails
for some other reason, Resume writes the row back to the status and
`finished_at` it read at the start, rather than leaving a `running` row with
no goroutine behind it.

A continuation cannot change the directory the chain started in. One chain of
responses is one session, and one session is one workspace: the frozen head
was rendered against that path, and the tools resolve against it.

Two consequences worth naming for a client:

- **A terminal event does not end the transcript.** A continued session
  appends after its terminal event, and every later replay of its history
  carries that event in the middle of the log. Ending a stream is the
  server's call, announced with a marker frame, because the client cannot
  work it out for itself.
- **A resumed session has one `session_started` per run.** `internal/fold`
  turns every one of them into a plain user message, because that is what each
  is to the model. A client rendering a transcript has to decide for itself
  which of them opened the session.

## The result status

`cancelled` is distinct from `timeout`, which is deadline-driven with nobody
involved. The result carries `error.code` `"cancelled"` and the stated reason
as its message, so a consumer can tell a decision from a budget being
exhausted without parsing prose.

## What this does not promise

- **That the goroutine exits.** Marking the run stopped and publishing a
  terminal result does not wait for it. If the wedged syscall never returns,
  the goroutine leaks for the life of the process. It is logged, loudly, with
  the session id and the tool call it was last in — an abandoned goroutine
  that looks identical to a clean stop is the outcome worth refusing, not the
  leak itself.
- **That the workspace is cleaned up.** The parent owns the directory; a
  stopped run leaves it exactly as it stood.
- **That one wedged tool call can be killed on its own.** Stop is whole-run.
  Killing a single call and letting the run continue is a smaller, harder
  feature — the run loop would have to be told to synthesise a tool result for
  a call that never returned — and the whole-run stop is the one that matches
  what happened in practice.

## Open questions

- **Rate-limiting stop and steer.** Nothing bounds how fast a parent may steer
  a session, and a tight loop of steers drives its cost up.
- **A steer that should have been a stop.** Someone who steers "stop what you
  are doing" gets a message the model may or may not act on. That is the
  correct behaviour for a steer and a bad outcome for the person; whether a
  client should notice and suggest cancelling instead is a product question.
