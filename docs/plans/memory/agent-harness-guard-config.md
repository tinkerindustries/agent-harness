# The autonomy guard, driven by configuration: feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

- The policy bundle is a JSON object: `schema_version`, `cutoff`, `stage1` (optional: `model`, `state_fields`,
  `questions`), `stage2` (`model`, `effort`, `max_tokens`, `state_fields`, `system`, `schema`) and `rules` (`macros`,
  `table` of `{name, patterns:[{match, unless_followed_by}]}`, `default_branches`). `guard.LoadBundle([]byte)`
  returns an opaque `*Bundle` or a `*BundleError{Field, Reason}` whose `Field` is relative to the bundle, so the
  create prefixes `harness.guard.policy.`. `Bundle.Version()` is the guard version, `HasStage1()` says whether
  `stage1_endpoint` is needed, and `StateFields()` is what a `StateBuilder.Fields` must fill.
- `NewPipeline` takes `PipelineConfig.Bundle`, the stage 1 endpoint and the key. A bundle with no stage 1 shows on
  records as `no_stage1` in `escalation`, never as a stage 1 error.
- The state fields are `working_directory`, `branch`, `recent_commands`, `proposed_command`, `scripts_run` and
  `package_scripts`. Each stage's list is a subset, in the order sent, and always includes `proposed_command`.
- `harness.guard` on create is `mode`, `stage1_endpoint` and `policy`. `stage2_model` is gone.
- The session row's guard setting holds the mode, the stage 1 endpoint and the bundle's hash. It never holds the
  bundle.
- The guard version on every record is the bundle's hash.

## Invariants

- No prompt, rule pattern, cut-off or model id is in this repository, in a test, a doc or a PR body. Tests use an
  invented bundle.
- The bundle is never written to the store, the state directory, a frame or a log, and no error repeats its text.
- The matching machinery, script detection, package-script resolution and the `git push` logic stay in code.
- Stage 1 failure of any kind escalates, and no stage 1 escalates every command the rules miss. Stage 2 failure of
  any kind blocks. Stage 2 is mandatory and never falls back to allow.
- `shadow` never stops a command. `enforce` is refused at create. Only `stdio-session` accepts the guard.
- A resume may replace the bundle. One that leaves the guard active with no bundle is refused unless the process
  already holds one.
- Nothing from the full parity export or the converted bundle is committed or quoted. This repository is public.

## Files that matter

| Path | Why an agent needs it |
|---|---|
| `internal/guard/bundle.go` | The bundle type, `LoadBundle`, `BundleError`, validation and the hash. |
| `internal/guard/compose.go`, `guardtest/bundle.go` | `NewPipeline`, and `BundleJSON`, the invented bundle every package's tests use. |
| `cmd/harness/stdiosession.go` | The `runner.Guards` factory, a stub that fails a guarded create until phase 2 fills it. |
| `internal/stdiosession/guardsetting.go` | `GuardConfig`, `resolveGuard` and `mergeGuard`, where the create's setting is checked. |
| `internal/session/guard.go`, `lifecycle.go`, `resume.go` | `Runner.Guards`, the run guard, and where run and resume options carry the setting. |
| `internal/store/sessions.go` | `GuardSetting` and the `guard` column. |
| `docs/STDIO-PROTOCOL.md` | The wire contract, including the create table and the autonomy guard section. |

## External facts

| Fact | Source |
|---|---|
| Go's `regexp` is RE2: no lookaround and no backreferences, which is why the rule format needs macros and a hand-checked exception. | https://pkg.go.dev/regexp/syntax |
| The private proofs read `GUARD_POLICY_FILE` (the converted bundle) and `GUARD_PARITY_DIR` (the private fixtures). The job prompts name the local paths. | The plan's Open questions. |

## Vocabulary

- **Policy bundle**: the parent-supplied content that says what the guard blocks.
- **Mechanism**: the code that applies a bundle. It is public.
- **Scenario**: a use of the guard with its own bundle, such as a coding agent or a persistent assistant.
- **Converted bundle**: the one-off bundle made from the content that was embedded before this feature.
