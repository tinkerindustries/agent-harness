# Phase 4 — the claude-session subcommand

## What was built

**The dialect.** `ManagedAgents` in `internal/stdiosession/managedagents.go`,
alongside `Responses` and `Interactions`: `sessions.create` / `.get` /
`.delete` plus one `sessions.events` covering steering, interrupting and
answering a custom tool call, exactly following the file split
`interactions.go`/`interactionstranslate.go`/`interactionswire.go` set —
`managedagents.go`, `managedagentstranslate.go`, `managedagentswire.go`, plus
a fourth file, `managedagentsevents.go`, for the one genuinely new piece:
`Server.events` and the pending-custom-tool registry.

**The seam changes.** `Dialect` gained `AddressID(runID, sessionID) string`
(as proposed) and `AddressesSession() bool` (the boolean alternative the
plan named as an option) — the latter is what `Server.get`/`.delete` actually
use, falling back to a new `Server.lookupBySession` when a bare id does not
resolve as a run id first. `Dialect.NewTranslator` gained a `sessionID`
parameter the original seam plan did not foresee: `RunView` already carrying
both ids covers every terminal frame, but ManagedAgents' *every* frame — not
only its terminal one — carries `session_id`, and `Live`/`Event` fire
throughout a turn with no `RunView` in sight. `Server.create`'s tail (minting
a run, opening its translator and pump, starting the loop) is factored into
a new `Server.beginRun`, and `append`/`cancelRun` into `appendInput`/
`cancelRunItem` against an already-resolved `*run` — both reused by
`sessions.events`, whose `user.message` on a running session steers
(`appendInput`) and on an idle one starts the next turn (`beginRun` with
`resume: true`, reusing the session's own `hostTools` instance kept on a new
`run.host` field, since nothing on this dialect's wire ever re-declares
tools).

**Per-turn id.** `ManagedAgents.NewRunID` mints `turn_<hex>`, never
serialised as a top-level id — only nested under `harness.turn_id` on every
notification and result, matching the document.

**Async client-declared tools.** `agent.custom_tool_use` is not a call
`hostTools.Call` sends and waits on; the translator emits it directly from
the ordinary `KindToolCall` event (which — internal/session/turn.go commits
a sub-turn's whole tool-call batch before running any of it — always reaches
the client before the corresponding tool-dispatch goroutine even starts).
`KindTurnFinished` (which always follows that same batch, still before any
result) is where a non-empty pending-custom set is announced as
`session.status_idle{stop_reason: requires_action}` — no waiting on other
calls to finish is needed, because nothing commits any result until every
call in the batch, custom ones included, does. `hostTools` gained an `async`
field (`func(ctx, id, name string, args) (tools.MCPContent, error)`), set
instead of `call` when the dialect is `ManagedAgents`; it blocks on a
map-plus-channel registry on `Server` (`customPending`, plus `customReady`
for a real registration race a local pipe makes visible — see "Bugs found").
`session.Runner`'s dispatch needed no change at all, confirming the plan's
own prediction.

**The timeout change.** `tools.Timeouts` gained one field, `HostTool`,
consulted in `timeoutFor` only for the reserved client-tool namespace
(promoted to an exported `tools.ClientToolServerName` constant, formerly a
private `stdiosession.HostServerName` literal) and only when non-zero — every
existing caller keeps the ordinary MCP timeout. Setting it needed a new
`session.Runner.ToolTimeouts tools.Timeouts` field, threaded into the
`tools.Executor` both `Run` and `Resume` already build (mirroring
`ToolEnv`/`ToolEnvFilter`'s own pattern exactly). `harness claude-session`
sets it to 24 hours — large, not unbounded, since `user.interrupt` is the
documented escape hatch and a truly wedgeable process is a worse failure mode
than a long, finite one.

**`cmd/harness`.** A new `claudesession.go`, `runClaudeSession`, composing its
own store/hub/MCP manager/Runner rather than extending `runStdioSession`: it
reads `ANTHROPIC_API_KEY` alone, hosts the three Claude models
(`claudeHostedModels`), defaults to `claude-sonnet-5`, and dispatches from
`main.go`'s new `"claude-session"` case. `harness help` names it.

**Tests.** `internal/stdiosession/managedagents_test.go`: a create with the
async custom-tool round (including a stray `user.message` refused while the
result is pending, and a duplicate answer refused), steer, interrupt, get by
turn id, delete, and cross-process resume — all against a fake Anthropic
Messages API recorder, the `claude_test.go` two-ended shape. A golden capture
(`TestManagedAgentsGoldenFrames`, `testdata/managedagents-frames.json`)
beside `TestGoldenFrames`. `cmd/harness/claudesession_test.go` covers key
resolution, the hosted-model list, an unknown `-model` refusal, and that the
key never reaches the state directory's database.

**Docs.** `docs/STDIO-PROTOCOL.md`'s intro and porting-table section now name
the third vocabulary and its credential; `CLAUDE.md`, `ARCHITECTURE.md` and
`internal/CLAUDE.md` name the third subcommand, dialect and files;
`docs/TOOLS.md`'s per-tool-timeout note gained the one exception;
`TESTING.md` names the new test file and golden test.
`docs/STDIO-MANAGED-AGENTS.md` gained seven new numbered deviations (12–17
plus a "What phase 4 actually built" note under "The seam") recording every
place the build departed from the phase-1 draft: no zero-`initial_events`
idle-no-turn session (not built this phase), `agent.custom_tool_use`'s `id`
reuses the tool call's own id rather than a minted `sevt_…` one, no
`evaluated_permission` on a tool-use notification (permission is not known
that early), `terminated` is never rendered, the interrupt/message exclusion
is per-call, a fixed `max_tokens` on every request (see "Bugs found"), and
the `AddressID`/registration-race corrections below.

## What was verified, and how

`scripts/build.sh`'s tail:

```
=== gofmt
clean

=== go vet
clean

=== tests
ok  	github.com/mrgeoffrich/agent-harness/cmd/harness	0.422s
[... every other package ok ...]
ok  	github.com/mrgeoffrich/agent-harness/internal/stdiosession	(cached)
[... every other package ok ...]

=== cross-compile
darwin/arm64
darwin/amd64
linux/amd64
linux/arm64
android/arm64
windows/amd64

=== go build
bin/harness
```

`go test ./internal/stdiosession -run TestGoldenFrames` and
`-run TestManagedAgentsGoldenFrames` both pass without `-update-golden`.
`go test ./internal/stdiosession -race` passes.

**The live check.** `harness claude-session -env
~/.config/agent-harness/anthropic.env` on `claude-sonnet-5`, driven by a
small Python driver over the real pipe (kept out of the repository), through
create, a tool round with an async custom tool, `sessions.get`, a steer,
`user.interrupt`, cross-process resume (`harness.resume_session_id`, with the
custom tool re-declared as the document requires) and a final steer proving
the resumed history round-tripped. Full findings in `docs/OBSERVED.md`,
"Claude Managed Agents vocabulary — Phase 4 live check". The excerpt the
proof asks for — create, a tool round, the session going idle:

```jsonc
// create
>>> {"id":"2","method":"sessions.create","params":{
    "agent":{"type":"agent_with_overrides","model":{"id":"claude-sonnet-5","effort":"low"}},
    "initial_events":[{"type":"user.message","content":[{"type":"text",
      "text":"Run `echo hello-claude-live` with Bash. Then call the log_note tool once with a short note about what you did. Then tell me in one short sentence what the Bash command printed. Do nothing else."}]}],
    "tools":[{"type":"function","name":"log_note","description":"Record a short progress note.",
      "parameters":{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}}],
    "harness":{"cwd":"/tmp/claude-live/work","permission_mode":"full"}}}
<<< {"id":"2","result":{"session":{"id":"sess-1fb9…","status":"running", …}}}

// the model's custom tool call
<<< {"method":"agent.custom_tool_use","params":{"session_id":"sess-1fb9…",
    "id":"toolu_01LpiYZVkNY9E7UpbopxEcuo","name":"log_note",
    "input":{"note":"Ran `echo hello-claude-live` via Bash; it printed hello-claude-live."},
    "harness":{"turn_id":"turn_f7e6…","sub_turn":2}}}

// the session goes idle, requiring the client's answer
<<< {"method":"session.status_idle","params":{"session_id":"sess-1fb9…",
    "stop_reason":{"type":"requires_action","event_ids":["toolu_01LpiYZVkNY9E7UpbopxEcuo"]},
    "harness":{"turn_id":"turn_f7e6…","sub_turn":2}}}

// answered through sessions.events
>>> {"id":"3","method":"sessions.events","params":{"session_id":"sess-1fb9…","events":[
    {"type":"user.custom_tool_result","custom_tool_use_id":"toolu_01LpiYZVkNY9E7UpbopxEcuo",
     "content":[{"type":"text","text":"noted"}],"is_error":false}]}}
<<< {"id":"3","result":{"session_id":"sess-1fb9…","results":[
    {"type":"user.custom_tool_result","custom_tool_use_id":"toolu_01LpiYZVkNY9E7UpbopxEcuo",
     "harness":{"turn_id":"turn_f7e6…"}}]}}

// the turn resumes and ends cleanly
<<< {"method":"session.status_running", …}
<<< {"method":"agent.message","params":{ …,"content":[{"type":"text",
    "text":"The command printed \"hello-claude-live\"."}], …}}
<<< {"method":"session.status_idle","params":{"session_id":"sess-1fb9…",
    "stop_reason":{"type":"end_turn"},"harness":{"turn_id":"turn_f7e6…","reason":"complete"}}}
```

## What was left undone

- **A session created idle with `initial_events` empty (no turn started).**
  Documented in the plan and in `docs/STDIO-MANAGED-AGENTS.md`'s original
  draft; this build refuses it with `-32004` instead. Building it correctly
  needs `Server` to hold a session's frozen create-time facts (cwd, model,
  tools, permission mode) in memory *before* any run exists, which the
  current `create()`/`beginRun` split does not do — every path today mints a
  run immediately. A real client of this dialect will in practice almost
  always supply an opening message anyway (Anthropic's own examples do), so
  this was judged a safe, disclosed scope reduction rather than something to
  force into an already-large phase; see the correction in
  `docs/STDIO-MANAGED-AGENTS.md`, Deviation 12, and the "Not built" entry.
- **`terminated` is never rendered** — every non-running session reports
  `idle`, including one that failed unrecoverably. `docs/STDIO-MANAGED-AGENTS.md`
  itself only narrowly distinguishes the two ("idle ... includes ... one
  that failed"), so this is a minor simplification, not a departure from
  anything a client is likely to depend on.
- **The registration-race stash (`Server.customReady`) cannot tell "answered
  before its call registered" apart from "an id this process never
  declared."** A `user.custom_tool_result` for a bogus id is therefore
  accepted rather than refused with `-32002`, in the narrow window before
  the real call's `hostTools.Call` runs. See "Bugs found" below for why this
  matters in practice (it is not merely theoretical) and why a full
  three-state registry was judged not worth building this phase.
- **No test drives `cmd/harness`'s `runClaudeSession` to a full create/tool
  round against a live or fake Anthropic server from that package** — mirrors
  phase 3's own identically-scoped gap for `stdio-session`, for the same
  reason: `internal/stdiosession/managedagents_test.go` exercises the same
  `Server`/`Runner` wiring one layer down, and the live check covers the
  rest.
- **`CacheSlack`, effort defaults, and per-model behaviour were not
  re-measured under ManagedAgents' own traffic shape** — the live check here
  was three short sessions on a trivial task, not a long multi-sub-turn one;
  phase 2/3's own figures (1024, generous) are inherited unchanged and
  nothing in this phase's traffic contradicted them.

## Bugs found and not fixed

None left unfixed. Two were found during this phase's own live check and
fixed before this report, both recorded fully in `docs/OBSERVED.md`:

1. **Every request under this dialect crashed the live API with
   `max_tokens: 0`.** This vocabulary has no client-settable
   `max_output_tokens` field at all, so `CreateRequest.MaxOutputTokens`
   silently stayed zero and `internal/anthropic` sent it verbatim as
   `max_tokens`, which the live Messages API refuses outright for a
   streaming request. Fixed by sending a fixed `8192` on every request this
   dialect builds (`maDefaultMaxOutputTokens`, in both `DecodeCreate` paths
   and the idle-session-continuation path in `managedagentsevents.go`).
   Neither the unit tests nor the fake-recorder end-to-end test would ever
   have caught this — the fake server does not validate the body the way the
   real one does — which is itself worth noting: **a fake-provider
   end-to-end test proves the wire shape, not that the provider will accept
   it.**
2. **A steer's `harness.turn_id` echoed the session id instead of the actual
   turn id.** `appendInput` ran the turn id through the newly added
   `Dialect.AddressID(runID, sessionID)` before handing it to
   `AppendResult`, which was backwards: `AppendResult`'s own per-dialect
   implementation already decides where the id goes (nested under
   `harness.turn_id` for ManagedAgents, top-level for the other two), so the
   plain turn id needs to reach it unchanged on every dialect. Caught by
   reading the live transcript's own `resume steer` result, not by any
   automated test — the fake-recorder test asserted the *presence* of
   `harness.turn_id` on a result, not that its value was the turn id and not
   the session id, because both are syntactically valid non-empty strings.
   Fixed in `Server.appendInput`; `AddressID` is kept on the `Dialect`
   interface, now genuinely unused, rather than removed mid-phase — see the
   "Memory suggestions" note on this.

## Mechanical friction

- **macOS has no `timeout(1)`.** The first hand-driven pipe test used it and
  failed immediately; every later manual/live-check drive used a short
  Python driver instead (kept out of the repository), which turned out to be
  the right tool anyway once the async custom-tool round needed real
  request/response and notification interleaving to drive by hand.
- **Working out where the async custom-tool value-passing registry could
  safely live took the most design time in this phase**, more than the
  translator or the dialect's own decode/encode code. The key unlock was
  noticing that `internal/session/turn.go` commits a sub-turn's whole
  `KindToolCall` batch, and the immediately-following `KindTurnFinished`,
  strictly before `executeToolCalls` runs any of them — which is what let the
  design avoid touching `internal/session` at all, matching the phase
  brief's own steer ("session.Runner's dispatch should need no pause and
  resume").
- **The registration race (bug 2 in spirit, though not the one written up
  above) is real on a local pipe, not a network-latency-only concern.** A
  first draft of `TestManagedAgentsCreateToolRoundAndIdle` failed with
  `"toolu_1" is not a pending custom tool call` because the test's own
  `sessions.events` answer reached the server before the tool-dispatch
  goroutine had registered its wait — everything here runs on loopback pipes
  with no real delay between "the client saw the notification" and "the
  client answered it." Worth remembering for any future async wire flow in
  this codebase: a design that only works because "the client is slower than
  us" is not actually safe, even in this repository's own tests.

## Memory suggestions

- **`internal/session/turn.go` commits a whole sub-turn's `KindToolCall`
  batch, plus the trailing `KindTurnFinished`, in one `AppendEvents` call —
  strictly before dispatch runs any of the calls.** This is *the* fact that
  makes an asynchronous, client-answered tool call buildable without
  touching `internal/session` at all: a translator watching the event
  stream already knows, the moment it has seen a batch's `KindTurnFinished`,
  that nothing else can happen until every custom call in that batch
  resolves, because no `KindToolResult` of any kind — custom or ordinary —
  commits until the *whole* batch (`executeToolCalls`'s one `wg.Wait()`)
  finishes. Any future async-tool or long-poll feature in this harness
  should start from this fact rather than rediscovering it.
- **`Dialect.AddressID` is unused in the code as it stands after this
  phase.** It was added for `AppendResult`'s call site and turned out
  unnecessary there (see "Bugs found" #2) — `AppendResult`'s own per-dialect
  body already decides where an id goes. Kept on the interface rather than
  removed, since a future dialect might genuinely need it, but a session
  reading this codebase should not assume it is load-bearing anywhere today;
  `grep -rn "AddressID(" internal/stdiosession` to check whether that has
  changed.
- **A fake-provider end-to-end test in this package proves the wire shape a
  parent sees; it does not prove the live provider will accept the request
  the harness builds.** Both bugs this phase found and fixed were only
  visible against the real Anthropic API — the `max_tokens: 0` body decodes
  and streams fine against a fake `httptest` recorder that echoes back a
  canned response regardless of what it was sent. A future phase adding a
  new required-but-currently-optional-looking field to any provider's
  request body should budget for a live check before trusting the fake-server
  suite alone, this codebase's own convention already says as much
  (`TESTING.md`, "Nothing in the suite calls `api.deepseek.com`") but it is
  easy to read that as "the suite is complete for the wire contract," which
  it is not for a provider-side body constraint.
- **The registration-race pattern (client answers before this side finishes
  registering the wait) is worth naming for whoever next builds an
  asynchronous wire flow here.** `Server.customPending`/`.customReady`'s
  two-map stash is the fix; it trades exactness (a bogus id is accepted
  rather than refused) for simplicity, on the judgment that a client-supplied
  id colliding with a real one is vanishingly unlikely. A future feature with
  a stricter correctness requirement here should build the full three-state
  registry this phase deliberately did not.
