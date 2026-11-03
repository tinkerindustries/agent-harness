# The autonomy guard in agent-harness: feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

- `harness.guard` on `create`, `stdio-session` only: mode (`off`, `shadow`, `enforce`), stage 1 endpoint, stage 2
  model. Stored on the session row. `enforce` is refused at create.
- One new event kind, a guard decision, keyed by tool call id: deciding stage, rule names, p(block), stage 2
  reason, latency, token usage, guard version, mode.
- One new Responses-vocabulary notification, `harness.guard`, carrying that record. The other two vocabularies
  skip the event.
- `internal/guard` is one package shared by phases 1 to 4. Phase 1's rules type satisfies `RuleChecker` and its
  script type satisfies `Escalator` (both in `pipeline.go`), and it passes `ScriptEntry` from `state.go`. `Version`
  takes a rules identity string that the rules code supplies. `Decision` has optional `PBlock` and `Stage2Usage`
  (four token counts); phase 3 persists it and adds the mode.

## Invariants

- The guard decides `bash` calls under `full` only. `readonly` already refuses `bash`.
- Stage 1 failure of any kind escalates. Stage 2 failure of any kind blocks. Stage 2 never falls back to allow.
- A guard decision never reaches the model's request: `internal/fold` ignores the event kind.
- `shadow` never delays or stops a command. `enforce` refuses through the existing denial route.
- Results append in `tool_calls` order whatever order the checks finish in.
- No rule, prompt, cut-off or model id changes here. The guard version changes when any of them does.
- A resume does not re-run a tool call, so a recorded decision is never read back.
- Nothing but protocol frames reaches stdout. The guard logs to stderr.
- Nothing from the full parity export is committed, quoted in a PR body or printed into a log. This repository is public.
- `gemini-session` and `claude-session` refuse the setting. A guard without `ANTHROPIC_API_KEY` is refused.

## Files that matter

| Path | Why an agent needs it |
|---|---|
| `docs/plans/agent-harness-guard.md` | The hand-off: decision order, both model calls, the state field table. |
| `internal/tools/policy.go` | `Policy.Check` is synchronous and sees only the descriptor. The guard sits beside it. |
| `internal/tools/registry.go` | `Executor.Execute` runs the policy check and then the tool. |
| `internal/tools/mutation.go` | `MutationTargets` resolves `write`, `edit` and `apply_patch` paths. |
| `internal/session/tooldispatch.go` | Runs a round's calls concurrently and orders results. |
| `internal/session/turn.go` | Commits tool calls, then results, and routes a denial to `tool_denied`. |
| `internal/session/compact.go` | Compaction forks a successor row, so state history can be cut. |
| `internal/session/lifecycle.go`, `resume.go` | Each builds a `Policy` and an `Executor`. Both pass run options to subagents. |
| `internal/store/events.go`, `schema.go` | Event kinds and payloads, and the session row. |
| `internal/fold/fold.go` | Folds the log into the request. Gives an interrupted call an interrupted output. |
| `internal/stdiosession/translate.go` | The Responses translator. Its `harness.usage` handling is the model for `harness.guard`. |
| `internal/stdiosession/methods.go`, `resume.go` | `CreateHarness`, and the checks a resume makes against the row. |
| `internal/guard/pipeline.go`, `state.go` | `Pipeline.Decide`, `Decision`, the stage and rule interfaces, and the `State` the model sees. |
| `internal/providerhttp` | `Transport` retries 429 and 5xx with backoff. A stage deadline covers the retries. |
| `docs/STDIO-PROTOCOL.md` | The wire contract. Read before touching `internal/stdiosession`. |

## External facts

| Fact | Source |
|---|---|
| Stage 1 is `POST <endpoint>/v1/systemone` on an Ollama server, model `clef-flash`, `keep_alive` `30m`. A valid answer has exactly the `allow` and `block` labels, each in [0, 1], summing to 1 within 0.01. | The hand-off, "Stage 1". Exact bytes are in the guard assets. |
| Stage 2 is the Anthropic Messages API with `output_config.format` carrying a `reason`-then-`decision` schema, one cache breakpoint on the fixed system prompt, and effort medium. A verdict needs `stop_reason` `end_turn` and exactly the keys `reason` and `decision`. | The hand-off, "Stage 2". Exact request is in the guard assets. |
| The stage 2 model is `claude-haiku-5-5`. `internal/provider` routes no Haiku model, which is fine because it is not a session model. | The hand-off; `docs/ANTHROPIC-INTEGRATION.md` for the Messages client's conventions. |
| The guard assets (prompts, models, cut-off 0.171, the Python sources) are committed under `internal/guard/testdata/assets/`, and its README names each file. Synthetic parity fixtures are committed under `internal/guard/testdata/parity/synthetic/`. The full export is local only and reached through `GUARD_PARITY_DIR`. | The assets README; the plan's Open questions. |

## Vocabulary

- **Stage 1, stage 2**: the local classifier, then the Claude model. Rules run before both.
- **Escalation**: sending a command straight to stage 2 because it runs session-written scripts.
- **Origin**: `written_this_session`, `changed_this_session` or `existing`, per script a command runs.
- **Shadow, enforce**: record only, or record and refuse.
