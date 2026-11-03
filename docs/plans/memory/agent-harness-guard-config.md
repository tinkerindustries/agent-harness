# The autonomy guard, driven by configuration: feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

- The policy bundle, a JSON object the parent sends in `harness.guard.policy`. Top-level fields: a schema version,
  `cutoff`, `stage1` (optional: `model`, `questions`), `stage2` (`model`, `effort`, `max_tokens`, `system`,
  `schema`), the state fields each stage receives, and `rules` (the pattern table and `default_branches`). Phase 1
  settles the rules' inner shape and the loader's names.
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
| `internal/guard/assets.go`, `rulesidentity.go`, `version.go` | The embedded content and the version that phase 1 replaces with the bundle. |
| `internal/guard/rules.go` | The rule table and the matcher that phase 1 builds from the bundle. |
| `internal/guard/stage1.go`, `stage2.go`, `compose.go` | The clients and `NewPipeline`, which take their content from the bundle. |
| `internal/guard/guardtest/guardtest.go` | Fake stage servers shared by the session and stdiosession tests. |
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
