# The autonomy guard in agent-harness: feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

- `harness.guard` on `create`, `stdio-session` only: mode (`off`, `shadow`, `enforce`), stage 1 endpoint, stage 2
  model. Stored on the session row. `enforce` is refused at create.
- `internal/guard` holds the whole decision: `Rules`, `ScriptDetector`, `Pipeline.Decide`, the two stage clients,
  `Version` (take `RulesIdentity()` for `VersionInputs.Rules`) and `StateBuilder`. `Decision` has optional `PBlock`
  and `Stage2Usage`, and no mode.
- `tools.Guard` is the seam `Executor.Execute` consults for `bash` under `full`. `session.runGuard` implements it,
  `Runner.Guards` is the factory `cmd/harness` supplies (the only place the Anthropic key is held), and
  `guard.NewPipeline` assembles the parts. `guardtest` holds the shared fake stage servers. The guard setting is
  the one stored session attribute a resume may replace.
- The record is the `guard_decision` event, keyed by tool call id, with the mode set by the caller. A stage 2
  answer also writes a `usage` event with `Model` set. `session.GuardEvents` builds both. The Responses
  translator turns the event into the `harness.guard` frame. The other two translators skip it.
- `session.BuildGuardState` builds the state from the log and the round's calls (`GuardCall.Round`).
  `session.LoadGuardHistory` follows `ParentID` through compaction only, not through `task` subagents.

## Invariants

- The guard decides `bash` calls under `full` only. `readonly` already refuses `bash`.
- Stage 1 failure of any kind escalates. Stage 2 failure of any kind blocks. Stage 2 never falls back to allow.
- A guard decision never reaches the model's request: `internal/fold` ignores the event kind.
- `shadow` never delays or stops a command. `enforce` refuses through the existing denial route.
- Results append in `tool_calls` order whatever order the checks finish in.
- A run's last event closes the parent's subscription, so a run that is ending waits for shadow checks in flight.
- A write in the same tool round as the checked `bash` has no result yet, so its script is unreadable and escalates.
- No rule, prompt, cut-off or model id changes here. The guard version changes when any of them does.
- A resume does not re-run a tool call, so a recorded decision is never read back.
- Nothing but protocol frames reaches stdout. The guard logs to stderr.
- Nothing from the full parity export is committed, quoted in a PR body or printed into a log. This repository is public.
- `gemini-session` and `claude-session` refuse the setting. A guard without `ANTHROPIC_API_KEY` is refused.

## Files that matter

| Path | Why an agent needs it |
|---|---|
| `internal/tools/policy.go`, `registry.go` | `Policy.Check` sees only the descriptor. `Executor.Execute` checks it, then runs the tool. |
| `internal/session/tooldispatch.go`, `turn.go` | A round's calls run concurrently. Results commit together, and a denial becomes `tool_denied`. |
| `internal/session/lifecycle.go`, `resume.go` | Each builds a `Policy` and an `Executor`, and both hand run options to `task` subagents. |
| `internal/session/guardstate.go` | `BuildGuardState`, `GuardEvents`, `GuardCall`. Phase 4 calls these from `Runner`. |
| `internal/guard/pipeline.go`, `builder.go` | `Pipeline.Decide`, `Decision`, and `StateBuilder.Build`. |
| `internal/store/events.go`, `schema.go` | The guard event payload, and the session row the setting is stored on. |
| `internal/stdiosession/methods.go`, `resume.go` | `CreateHarness`, and the checks a resume makes against the row. |
| `docs/STDIO-PROTOCOL.md` | The wire contract, including the `harness.guard` section. Read before touching `internal/stdiosession`. |

## External facts

| Fact | Source |
|---|---|
| Stage 1 is `POST <endpoint>/v1/systemone` on an Ollama server, model `clef-flash`, `keep_alive` `30m`. A valid answer has exactly the `allow` and `block` labels, each in [0, 1], summing to 1 within 0.01. | The hand-off, "Stage 1". Exact bytes are in the guard assets. |
| Stage 2 is the Anthropic Messages API, model `claude-haiku-5-5`, effort medium, one cache breakpoint, a `reason`-then-`decision` schema. `internal/provider` routes no Haiku model, which is fine because it is not a session model. | The hand-off, "Stage 2". Exact request is in the guard assets. |
| `Table.Cost` has one rate per model. Haiku 5.5's rates above 100,000 prompt tokens are not represented. | `configs/prices.json`, the `claude-haiku-5-5` entry. |
| The guard assets (prompts, models, cut-off 0.171, the Python sources) are under `internal/guard/testdata/assets/`. Synthetic parity fixtures are under `internal/guard/testdata/parity/synthetic/`. The full export is local only, through `GUARD_PARITY_DIR`. | The assets README; the plan's Open questions. |

## Vocabulary

- **Stage 1, stage 2**: the local classifier, then the Claude model. Rules run before both.
- **Escalation**: sending a command straight to stage 2 because it runs session-written scripts.
- **Origin**: `written_this_session`, `changed_this_session` or `existing`, per script a command runs.
- **Shadow, enforce**: record only, or record and refuse.
