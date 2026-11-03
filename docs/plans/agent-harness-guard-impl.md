# The autonomy guard in agent-harness

- **Slug**: agent-harness-guard-impl
- **Memory**: [memory/agent-harness-guard-impl.md](memory/agent-harness-guard-impl.md)

## What this builds

A `stdio-session` run under `full` permission screens every `bash` call with the autonomy guard before the
call runs. The guard applies exact-pattern rules, then a small local classifier (stage 1), then a Claude model
(stage 2). A parent turns it on with a `harness.guard` setting on `create`. It ships in `shadow` mode: the guard
decides and records every call, the command runs regardless, and the parent receives each decision as a frame.
The `enforce` path is built and tested but a create cannot select it yet.

[agent-harness-guard.md](agent-harness-guard.md) is the hand-off from classifier-research and states what the
guard decides, the two model calls and how the state is built. This plan is the harness's side of that
document. Where the two disagree on a point of harness behaviour, this plan wins. The decision rules, prompts
and cut-off belong to classifier-research and are not restated here.

## Not building

- Anything in classifier-research (hand-off phases 1, 2 and 7): the holdout score, the fixture export, the
  shadow-log import and the false-block measurement.
- Anything in Turret (hand-off phase 6).
- A way for a parent to select `enforce`. The code exists and is gated off.
- A guard on `gemini-session` or `claude-session`. They refuse the setting.
- The read-only allowlist in front of stage 1. It was never measured.
- Screening of `write`, `edit`, `apply_patch`, `web_fetch`, MCP tools or client function tools. The file tools
  already refuse paths outside the workspace.
- Watching what a background job does after it starts.
- Detecting files a `bash` command wrote by parsing the command. Origin for those comes from modification time.
- A refusal message that includes stage 2's reason. The model sees the guard and the stage. The reason goes to
  the log and the parent only.
- A new frame in the Interactions or Managed Agents vocabularies.
- Changing a rule, prompt, cut-off or model id. Those change in classifier-research first.

## Phases

| Phase | Name | Depends on | Runs beside |
|---|---|---|---|
| 1 | Port rules, script detection and package scripts | nothing, but see Open questions | 2 |
| 2 | Stage clients, pipeline and guard version | nothing | 1 |
| 3 | Session state, decision record and the `harness.guard` frame | 1, 2 | |
| 4 | The `create` setting, the executor hook and the modes | 3 | |

Phases 1 and 2 touch separate packages and share nothing but the data they read. Phase 2 codes against small
interfaces for rules and escalation, so it does not wait for phase 1. Phases 3 and 4 both change the session
and store packages, so they run in sequence.

### Phase 1: Port rules, script detection and package scripts

**Depends on**: nothing in this plan. The parity fixtures have to be committed first (see Open questions).

**What it builds**

A Go package that answers, for a command string: which rules block it, which local scripts it runs, whether it
should escalate straight to stage 2, and which package scripts it runs. It is a port of the Python modules
named in the "Source of truth" table of the hand-off. It also holds a word splitter that behaves like Python's
`shlex` on the fixture cases, including the fallback to whitespace splitting on unbalanced quotes.

**How to approach it**

The fixtures are the specification. Read them before the Python, because the hand-off says the code wins and
the fixtures are exported from the code. Go's `regexp` is RE2 and has no lookaround. The Python rules use a
lookbehind to find the start of a command, a negative lookahead in one rule and a negative lookbehind for
quoted text. Replace those with a tokenising pass that tracks quoting and command boundaries, and keep each
rule's regex simple. The stripping of heredoc bodies and quoted strings, except text after `-c` or `eval`, is
part of each rule's behaviour. Write the stripping first and test it alone. Resolve scripts against the
effective directory the caller passes in (the call's `workdir`, otherwise `harness.cwd`), and take the
filesystem as an interface so tests need no disk. Do not read `autonomy-holdout` data, and do not add cases of
your own to the fixture directory.

**Proof**

`go test ./internal/guard/... -run Parity` passes every fixture case for `check`, `executed_scripts`,
`escalation_reasons` and `package_scripts`, and `scripts/build.sh` passes and ends by printing `bin/harness`.
The PR body lists any fixture case it had to skip and why. It should list none.

**Out of scope for this phase**

Reading session state, calling a model and anything under `internal/session` or `internal/stdiosession`.

### Phase 2: Stage clients, pipeline and guard version

**Depends on**: nothing in this plan. The guard assets are committed under `internal/guard/testdata/assets/`.

**What it builds**

The stage 1 client for `POST <endpoint>/v1/systemone`, the stage 2 client for the Anthropic Messages API, and
the pipeline that combines rules, escalation, stage 1 and stage 2 into one `allow` or `block` with the record
of how it got there. It also builds the guard version identifier.

**How to approach it**

Take the request and response shapes, validity checks and failure handling from the hand-off's "The two model
calls" and from the stage 1 and stage 2 assets, byte for byte where the assets give bytes. A stage 1 failure of
any kind escalates. A stage 2 failure of any kind blocks. Stage 2 never falls back to `allow`. Stage 2 is a
small purpose-built client, not `internal/anthropic`'s: that one is built around the loop's intent, with three
cache breakpoints, thinking replay and `pause_turn` resumption, and stage 2 uses none of them. Both clients can
use `providerhttp.Transport`, which retries 429 and 5xx with backoff, so each stage's deadline has to cover its
retries. The pipeline depends on interfaces for rules and escalation that phase 1 will satisfy, and tests use
fakes. The guard version is one identifier derived from the rules, both prompts, the cut-off and the model ids,
so it changes when any of them does. Stage 2 returns token usage for the caller to record.

**Proof**

`go test ./internal/guard/... -run 'Stage|Pipeline|Version'` passes. The tests use `httptest` servers and cover
every failure path in the hand-off (stage 1: timeout, HTTP error, probabilities with the wrong labels, a value
outside [0, 1], a sum off by more than 0.01; stage 2: a `stop_reason` other than `end_turn`, extra or missing
keys, a timeout) and the order of decisions. A test shows the version changes when each input changes.
`scripts/build.sh` passes.

**Out of scope for this phase**

Building the state from a session, recording anything in the event log and the `create` setting.

### Phase 3: Session state, decision record and the `harness.guard` frame

**Depends on**: 1, 2

**What it builds**

A builder that turns a session's event log and the working directory into the guard's state at the moment a
`bash` call is checked: working directory, branch, the last three `bash` commands, the proposed command, the
scripts it runs with their origin and content, and the package scripts. A new event kind that records one
guard decision per screened call, keyed by tool call id. A `harness.guard` notification in the Responses
vocabulary that carries each recorded decision to the parent. A price entry so stage 2's tokens have a cost.

**How to approach it**

Follow the field table in the hand-off. A write counts toward a script's origin only when its tool call has a
matching result that is not an error. `tools.MutationTargets` already resolves the paths of `write`, `edit`
and `apply_patch`. Use it rather than parsing descriptors, because `descriptorFor` has no `apply_patch` case.
Files written by a `bash` command are seen through modification time against the session start. A script that
a call in the same tool round also writes is not readable yet, so treat its content as unreadable and let it
escalate. Compaction forks a successor session. Test what a compacted session's builder sees. If the successor's
log lacks the earlier writes and commands, follow the parent link so history carries across. The decision event
is ignored by `internal/fold`, like `tool_stdout`, so it never reaches a request. The Interactions and Managed
Agents translators skip it. The Responses translator emits `harness.guard` as a standalone notification like
`harness.usage`, because the decision may be committed after the call's result. Record stage 2's tokens as
usage events with the model set, which the cost summing already separates.

**Proof**

`go test ./internal/guard/... ./internal/fold/... ./internal/store/... ./internal/stdiosession/...` passes with
new tests that cover: the three-command window and the 200-character cut; origin for session-written,
session-changed and existing scripts; a failed write; two calls in one round; a compacted session; and the
Responses notification for a hand-committed decision event.
`go test ./internal/stdiosession -run TestGoldenFrames` and `-run TestManagedAgentsGoldenFrames` pass without
regenerating a golden. `scripts/build.sh` passes.

**Out of scope for this phase**

Calling the guard from the tool path, the `create` setting and the modes.

### Phase 4: The `create` setting, the executor hook and the modes

**Depends on**: 3

**What it builds**

A `harness.guard` setting on `create` for `stdio-session`: a mode of `off`, `shadow` or `enforce`, the stage 1
endpoint, and the stage 2 model with a default. The setting is stored on the session row and survives resume,
compaction and `task` subagents. The guard runs on each `bash` call under `full`. In `shadow` it runs beside
the command and records its decision when it arrives. In `enforce` a block returns a refused result with
`is_error` and a `harness.rule` that names the guard and the stage, and the command does not run. A create that
selects `enforce` is refused. A create that asks for a guard without `ANTHROPIC_API_KEY` is refused with a
message naming the variable. `gemini-session` and `claude-session` refuse the setting as unsupported.

**How to approach it**

`Executor.Execute` has the context and call id, but `tools.Executor` holds no store handle by design, so
`Runner` injects the guard and supplies the event log, and `cmd/harness` composes it. `Policy` is built in the
run and resume paths and compaction copies the session row, so check all three carry the setting. The guard is
not part of the cached prefix, so a resume may change the mode: take the setting from the create, and use the
stored value when the create names none. A `task` subagent runs through the same loop with its own executor,
so hand the guard through the run options and test that a subagent's `bash` call is screened. A shadow check
must not delay the command, and its record commits on a context that survives a cancelled run. Under `enforce`
a block takes the existing denial route, so the model and the parent see it as they see a deny pattern today.
Several checks can be in flight at once and results still append in `tool_calls` order. Test the enforce path
by constructing it directly, since no create can select it. Update the documents that describe current
behaviour: the `create` table and the new frame in `docs/STDIO-PROTOCOL.md`, the Permissions section of
`docs/TOOLS.md`, `ARCHITECTURE.md` and the codemap in `internal/CLAUDE.md`. Describe what the code does and
leave out counts and history.

**Proof**

`go test ./internal/stdiosession ./internal/session ./internal/tools ./internal/guard/...` passes with new tests
that run a scripted session against fake stage 1 and stage 2 servers and show: in `shadow`, the command runs
and a `harness.guard` frame arrives for it; in `shadow`, a stage 2 outage still lets the command run and
records the failure; a create with no `ANTHROPIC_API_KEY`, a create naming `enforce`, and either other
subcommand receiving the setting are all refused; a resumed session takes a changed mode from its create; a
subagent's `bash` call is screened; a directly constructed `enforce` guard blocks a call without running it.
`go test ./internal/stdiosession -run TestGoldenFrames` and `-run TestManagedAgentsGoldenFrames` pass without
regenerating a golden. `scripts/build.sh` passes.

**Out of scope for this phase**

Selecting `enforce`, any change to a rule, prompt or cut-off, and Turret's use of the setting.

## Open questions

- **Before phase 1 can start**, a classifier-research session commits the parity fixtures under
  `internal/guard/testdata/parity/`, exported from `autonomy-actions` only, covering `check`,
  `executed_scripts`, `escalation_reasons` and `package_scripts`. They do not exist yet, and the exporter has to
  be written in classifier-research first. The orchestrator checks they are present before spawning phase 1.
  Owner: Geoff, through classifier-research.
- The guard assets are committed under `internal/guard/testdata/assets/`, with a README that names each file's
  origin and the values that are not in a file (the cut-off, the stage 1 request, latency figures). They include
  the Python sources phase 1 ports. The Ollama server's address and the guard results document are not in
  them, because this repository is public. Phase 2 can start now.
- The fixture file format is whatever classifier-research exports. Phase 1 reads it as found. If the format is
  ambiguous, the agent asks the orchestrator and does not guess.
- The evals set no per-stage deadline. Phase 2 picks defaults from the latency figures in the assets README and
  states them in its PR body. Owner: Geoff, to confirm them before a parent turns shadow on.
- The guard's prompts and rules are in a public repository once phases 1 and 2 land. That is inherent in a Go
  port, and it lets anyone read what the guard blocks. Owner: Geoff.
- Modification time as the origin signal for files a `bash` command wrote is a weaker signal than the evals
  had. Shadow records show how often it matters. Owner: Geoff, after shadow data exists.
- The plan accepts the hand-off's fallback when stage 1 is unreachable: every command goes to stage 2. Whether
  a guarded session should refuse to start instead is open until before `enforce`. Owner: Geoff.
- Whether secrets in commands may be sent to Anthropic in `shadow` mode. Shadow sends the same text stage 2
  would. Owner: Geoff, before a parent turns shadow on outside testing.
- Whether the `deepseek-harness` container runs this binary. If it runs other code, this guard does not cover
  it. Owner: Geoff.
