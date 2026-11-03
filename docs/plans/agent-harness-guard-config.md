# The autonomy guard, driven by configuration

- **Slug**: agent-harness-guard-config
- **Memory**: [memory/agent-harness-guard-config.md](memory/agent-harness-guard-config.md)

## What this builds

The autonomy guard's content comes from the parent instead of the binary. A parent such as Turret sends a policy
bundle inside `harness.guard` on `create`: the stage 1 questions, the stage 2 system prompt and schema, the
cut-off, the model ids and the rules' patterns. The harness keeps the mechanism: the pipeline, the stage clients,
quote and heredoc stripping, script detection, the state builder, recording, and the shadow and enforce modes.
Nothing that says what the guard blocks is in this repository any more. The guard version is a hash of the bundle.

The bundle is also what makes the guard fit other scenarios, such as a persistent assistant that has no repository.
It chooses which of the state fields each stage receives, and it may leave out stage 1. How the guard hooks into a
persistent assistant is a separate piece of work and is not planned here.

The guard shipped in the plan [archive/agent-harness-guard-impl.md](archive/agent-harness-guard-impl.md), against
the hand-off in [agent-harness-guard.md](agent-harness-guard.md). This repository is public and the prompts and
rules are to stay private. Content already published stays in git history, which this plan cannot undo.

## Not building

- Anything in classifier-research (exporting the bundle) or Turret (keeping the bundle and sending it, hand-off
  phase 6). Both get a note under Open questions.
- A change to any rule, prompt, cut-off or model id. The first bundle carries today's content unchanged.
- Customisation beyond the two points above: the recent-commands window, a scenario name on records, and a switch
  for script escalation stay in code. Screening tools other than `bash`, and hooking the guard into a persistent
  assistant, are separate work.
- The mechanism's heuristics as configuration: the wrapper and interpreter lists in script detection, the
  package-script resolution and the `git push` logic stay in code. Only the default branch names move.
- Writing the bundle into the state directory, the settings table or a log. The harness holds it in memory.
- Signing or verifying a bundle, reloading one mid-session, or a different bundle per call.
- A mechanism revision in the guard version. The version covers the bundle alone.
- Rewriting git history, or making the repository private.
- Any change to `enforce` being refused at create, to `stdio-session` being the only subcommand that accepts the
  guard, or to `ANTHROPIC_API_KEY` being required.

## Phases

| Phase | Name | Depends on | Runs beside |
|---|---|---|---|
| 1 | The policy bundle in the guard package | nothing | |
| 2 | The bundle through the create and the session, and the docs | 1 | |

Both phases change the guard package and the session package's callers of it, so they run in sequence.

### Phase 1: The policy bundle in the guard package

**Depends on**: nothing. The orchestrator copies the synthetic `check.jsonl` out of the repository first (see Open
questions).

**What it builds**

A policy bundle type with a schema version, a loader that parses and validates it, and a hash that becomes the
guard version. The stage 1 and stage 2 clients take their prompts, schema, models, effort and token limit from the
bundle. The pipeline takes its cut-off from it. The rule checker is built from the bundle's rules: a table of
RE2 patterns, each with a rule name, plus the default branch names. The bundle lists the state fields each stage receives, drawn from the six the state
builder knows, and stage 1 is optional: a bundle without it sends every command the rules do not catch straight to
stage 2. The embedded prompt assets, the embedded Python
sources and the constants that hold prompt, cut-off or model content are gone. The first real bundle is converted
once from today's content and written to a local path outside the repository.

**How to approach it**

Treat the bundle as opaque to everything except the loader. The stage 1 `questions` and the stage 2 schema are JSON
objects the harness sends as given, so keep their bytes and do not re-encode them through a map. The rules need
a pattern format. Today's table builds each pattern from the Unicode-aware whitespace and word classes, anchors
it at a command start, and has one rule whose negative lookahead is checked by hand. Give the bundle named macros
for the shared classes, allow several patterns under one rule name, and give the exception a field the matcher
evaluates by hand, so every current rule is expressible. The default branch names are a list. An empty list turns
the push check off. Each stage's field list decides the JSON the stage receives, in the order given, with the same field names as
today, so build the state type from the list and not from a fixed struct. A list that names an unknown field, or
leaves out `proposed_command`, is refused, because without the command there is nothing to judge. Skip the work
for a field no stage receives, since resolving package scripts reads files. Stage 2 stays mandatory so the guard
never allows on the rules alone. When there is no stage 1, record that as a reason the decision can be told apart
by, and not as a stage 1 failure. Validate at load and name the field in every error. Check that the cut-off is a probability,
that every pattern compiles and uses only known macros, that both questions and the schema decode, and that no
model is empty. Never put bundle text in an error or a log line. For the conversion, work from the files as they
are on `main` before deleting them, write the bundle to the local path the job prompt names, and do not commit the
converter or its output. Public tests use a small bundle of invented rules and prompts that exercises every part of
the format: each macro, several patterns under one name, the exception field, the default branches, every
command-start position the matcher recognises, a bundle with a reduced field list and a bundle with no stage 1. Tests that need a model id name one themselves.

**Proof**

`go test ./internal/guard/...` passes with the invented bundle. The tests cover a valid bundle loading, each class of
invalid field being refused with the field named (including an unknown state field and a list without
`proposed_command`), the version changing when any bundle field changes, every part of the rule format, both stage
clients sending exactly the bundle's content with only the listed state fields in the listed order, and a bundle
with no stage 1 sending a clean command to stage 2. `git grep -n 'go:embed' --
internal/guard` prints nothing, and the assets, Python source and `check.jsonl` paths no longer exist in the tree.
With `GUARD_POLICY_FILE` naming the converted bundle and `GUARD_PARITY_DIR` naming the private fixtures, the parity
test passes every case for all four functions in both the full export and the moved synthetic `check` set. The PR
body gives counts and nothing from either. `scripts/build.sh` passes and ends by printing `bin/harness`.

**Out of scope for this phase**

The `create` field, the session row, the in-memory holding of the bundle, resume and the docs.

### Phase 2: The bundle through the create and the session, and the docs

**Depends on**: 1

**What it builds**

A `policy` object in `harness.guard` on `create`, loaded and validated by phase 1's loader. A refused bundle is
`-32602` and names the field. `stage1_endpoint` is required only when the bundle has a stage 1. `stage2_model` leaves the create, the stored setting and the docs, because the bundle
names the models. The session row stores the bundle's hash and the mode and stage 1 endpoint, and never the bundle.
The harness holds the bundle in memory for the session's life, so a continuation, a background wake, a compaction
successor and a `task` subagent all screen with it. A resume that leaves the guard active and sends no bundle is
refused when this process does not already hold one. A resume that sends a different bundle replaces it, and the
decisions it records carry the new hash as their version. The documents are updated, and the block-list wording is
scrubbed from the public docs.

**How to approach it**

The bundle is not part of the cached prefix, so a resume may change it as it may change the mode. The pieces that
move with a session are the places to check: the run options and resume options the loop receives, the factory
`cmd/harness` supplies to build a decider, the `followCompaction` seam that points per-conversation state at a
compaction successor, and the options a `task` subagent inherits. Hold the bundle where the conversation's other
in-process state is held rather than on a global, and release it with that state on `harness.close_session`. Do not
put the bundle on a struct that is ever encoded to the store, a frame or a log. The endpoint check currently runs before anything reads a bundle, so reorder it. For the docs, describe the create
field's shape and what the harness does with each part, and say that the content is the parent's. Document the
create table in the wire reference, the Permissions section of the tools reference, the architecture and
codemap entries, and the hand-off and archived documents. In each, replace any statement of what the guard blocks
with a pointer to the parent's bundle. Grep the whole tree for the categories the block list names, and scrub what
describes them. Leave the mechanism's description alone.

**Proof**

`go test ./internal/stdiosession ./internal/session ./internal/store ./internal/guard/... ./cmd/...` passes with
new tests, run against fake stage servers and an invented bundle, that show: a create with an active guard and no
bundle is refused; an invalid bundle is refused naming the field; a bundle with no stage 1 starts without an endpoint and one with a
stage 1 and no endpoint is refused; a shadow session records decisions whose version
is the bundle's hash; the stored row holds the hash and no bundle text; a resume with no bundle is refused when the
process holds none, and works when it does; a resume with a different bundle changes the recorded version; a
subagent's `bash` call and a compaction successor are screened with the same bundle; `harness.close_session`
releases it. `go test ./internal/stdiosession -run TestGoldenFrames` and `-run TestManagedAgentsGoldenFrames` pass
without regenerating a golden. A search of the tree for the block-list categories finds only the mechanism's
description and pointers to the bundle. `scripts/build.sh` passes.

**Out of scope for this phase**

The bundle format and loader, which phase 1 owns, and anything in classifier-research or Turret.

## Open questions

- **Before phase 1 starts**, the orchestrator copies the synthetic `check.jsonl` from
  `internal/guard/testdata/parity/synthetic/` to the private fixtures directory on Geoff's machine, and names that
  directory and the local path for the converted bundle in the job prompt. The synthetic `executed_scripts`,
  `escalation_reasons` and `package_scripts` files stay committed. Owner: the orchestrator.
- classifier-research should export the bundle itself, as it exports parity fixtures today, so the source of truth
  stays there. Until then the converted bundle is a one-off. Owner: Geoff.
- Turret keeps the bundle in its private configuration, loads it when it spawns a guarded session, and sends it
  inside `harness.guard.policy` on every create, including a resume. That is hand-off phase 6 and needs its own
  plan. Owner: Geoff.
- The prompts, rules and Python sources are in the public history from the guard's merge. Removing them from the
  tip does not remove them from the history or from the pull request refs. A bundle that differs from the published
  content is the practical mitigation, and classifier-research owns that. Owner: Geoff.
- How the guard hooks into a persistent assistant: which of its calls are screened and what state it supplies.
  The state builder reads `bash`, `write`, `edit` and `apply_patch` traffic and the working directory, which an
  assistant may not have. The field lists let a bundle ignore what does not apply. That work needs its own plan.
  Owner: Geoff.
- Per-stage deadlines (3 s and 20 s) are code defaults and are unchanged by this plan. Owner: Geoff, to confirm
  before a parent turns shadow on.
