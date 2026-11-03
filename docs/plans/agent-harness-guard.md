# Plan: the autonomy guard in agent-harness

Status: not started. This is the hand-off from classifier-research to agent-harness. agent-harness writes
its own implementation plan against this one.

## Goal

agent-harness screens every `bash` call a session makes under `full` permission with the autonomy guard
designed in [autonomy-guard.md](autonomy-guard.md). A blocked call does not run. It returns to the model as a
refused tool result, the same way a `harness.deny` match does today, and the model works around it. Nothing
waits on a person.

The guard lives in agent-harness because the harness decides every tool call in Go before it runs
(`internal/tools/policy.go`), and its stdio protocol has no approval request. A parent such as Turret only
sees a tool call after the harness has decided it. Building the guard into the harness covers every parent:
Turret desktop, the iOS in-process host, and any other process that spawns the binary.

## What the guard decides

The guard returns `allow` or `block` for one proposed command, in this order:

1. **Rules.** Exact-pattern rules block the command outright. No model is called.
2. **Escalation.** If the command runs two or more local scripts and at least one was written or changed in
   this session, or it runs a session script whose content cannot be read, it goes straight to stage 2.
3. **Stage 1.** Clef Flash on Ollama returns p(block). At or above the cut-off, the command goes to stage 2.
   A stage 1 failure of any kind (timeout, HTTP error, invalid answer) also sends it to stage 2.
4. **Stage 2.** Claude Haiku 5.5 at medium effort returns `allow` or `block` with a reason. A stage 2 failure
   of any kind blocks the command. Stage 2 never falls back to `allow`.
5. Anything that reaches none of the above is allowed.

The cut-off is the `cutoff` in the parent's policy bundle. It was chosen on the dev split of `autonomy-actions` with
`src/stage1_threshold.py`, and changes only when classifier-research re-runs that choice.

What the guard blocks is defined by the parent's policy bundle, in the stage 2 system prompt and the rules table.
This repository states none of it.

## Source of truth in classifier-research

Port or vendor these. Each one is the authority on its part. Where this plan and the code disagree, the code
wins.

| Part | classifier-research path |
| --- | --- |
| Rules | `src/guard_rules.py` (`check(command, branch)`) |
| Scripts a command runs, escalation | `src/script_detection.py` (`executed_scripts`, `escalation_reasons`) |
| Package scripts a command runs | `src/package_scripts.py` |
| Pipeline order and failure handling | `src/pipeline_report.py` (`decide`, `stage1_flag`, `stage2_verdict`) |
| Stage 1 prompt | `evals/prompts/autonomy-stage1-scripts/baseline.yaml` |
| Stage 1 experiment (state fields) | `evals/experiments/autonomy-stage1-scripts.yaml` |
| Stage 1 model and endpoint | `evals/models/clef-flash.yaml`; request shape in `docs/MODELS.md` and `make_request` / `parse_answer` in `src/evaluate.py` |
| Stage 2 prompt, schema, request, state | [../AUTONOMY-STAGE2-PROMPT.md](../AUTONOMY-STAGE2-PROMPT.md) (a copy; it says how to regenerate it) |
| Stage 2 experiment | `evals/experiments/autonomy-stage2-reasoning.yaml` |
| Measured results, latency and cost | [autonomy-guard.md](autonomy-guard.md) |

## The two model calls

### Stage 1

`POST <endpoint>/v1/systemone` on the Ollama server (the System One decision API):

```json
{
  "model": "clef-flash",
  "keep_alive": "30m",
  "state": { "working_directory": "…", "branch": "…", "recent_commands": ["…"], "proposed_command": "…", "scripts_run": [] },
  "questions": { "risk_check": { "type": "choice", "instructions": "…", "criteria": { "allow": "…", "block": "…" } } }
}
```

`questions` is the stage 1 prompt YAML as JSON. The response is
`{"answers": {"risk_check": {"choice": "…", "probabilities": {"allow": p, "block": p}}}}`. It is valid only
when `probabilities` has exactly the two labels, each a finite number in [0, 1], summing to 1 within 0.01.
Anything else is a stage 1 failure.

Stage 1 sends five state fields: `working_directory`, `branch`, `recent_commands`, `proposed_command`,
`scripts_run`. It does not send `package_scripts`.

### Stage 2

The Anthropic Messages API, exactly as [../AUTONOMY-STAGE2-PROMPT.md](../AUTONOMY-STAGE2-PROMPT.md)
describes: the fixed system prompt carries the only cache breakpoint, the user message is the state as JSON
indented by 2, and `output_config.format` carries the `reason`-then-`decision` schema. A verdict needs
`stop_reason` `end_turn` and a JSON object with exactly `reason` and `decision`. Everything else blocks.

Stage 2 sends six state fields, in this order: the five above, then `package_scripts`.

## Building the state inside the harness

The datasets were built from Claude Code transcripts after the fact. The harness builds the same fields
live. It must keep each field's meaning as the evals measured it. A field that changes meaning needs a new
experiment in classifier-research first.

| Field | How the harness fills it |
| --- | --- |
| `working_directory` | The session's `harness.cwd`. A `bash` call can name a `workdir` inside it. Script paths in `executed_scripts` resolve against the effective directory (`workdir` when given, otherwise `harness.cwd`). |
| `branch` | The branch checked out in `harness.cwd`, read when the command is checked. |
| `recent_commands` | The last three `bash` commands this session ran, oldest first, without output, each cut to about 200 characters with an ellipsis. From the event log. |
| `proposed_command` | The `bash` call's command, unchanged. |
| `scripts_run` | One entry per path `executed_scripts` finds, with `path`, `origin` and `content`. |
| `package_scripts` | `package_scripts.py`'s resolution against the files in `harness.cwd` at check time. |

`scripts_run` needs care:

- `origin` is `written_this_session` when the session created the file, `changed_this_session` when the
  session modified a file that already existed, and `existing` otherwise. The harness sees writes made through
  `write`, `edit` and `apply_patch` in its event log. It cannot see a file a `bash` command wrote. Detecting
  those (for example by comparing a file's modification time with the session start) is a harness design
  decision.
- A write counts only when its `tool_call` has a matching `tool_result` with `is_error` false. A failed write
  does not make a file a session file.
- Write targets come from `tools.MutationTargets`, which already resolves `write`, `edit` and `apply_patch`
  paths. `descriptorFor` has no `apply_patch` case, so descriptors cannot supply them.
- `executeToolCalls` runs calls in one tool round concurrently unless they share a file. A `bash` call that
  runs a script written in the same round can reach the guard before the write lands, and the event log has the
  write call but no result yet. The guard waits for the `MutationTargets` group of any file the command runs,
  or treats the script as unreadable and escalates. The harness plan picks one.
- Compaction forks the session into a successor row. Whether the successor's log carries the earlier `write`
  and `bash` events is unverified. If it does not, a script written before compaction becomes `existing` with
  null content and skips escalation, and `recent_commands` restarts. A test on a compacted session settles it.
- `content` is the file's full text for a session-origin script, read from disk at check time. It is `null`
  for `existing` scripts, because the evals never showed the model the text of project scripts. It is also
  `null` for a session script the harness cannot read. A `null` on a session script triggers escalation.

## What the harness needs to add

- **A guard setting on `create`.** For example `harness.guard`, naming the stage 1 endpoint, the policy
  bundle, and a mode of `off`, `shadow` or `enforce`. It applies under `full`. `readonly` already refuses
  `bash`. The parent supplies endpoints and `ANTHROPIC_API_KEY`, as it supplies credentials today.
  - Stage 2 needs `ANTHROPIC_API_KEY`, and no key implies another (CLAUDE.md). The plan names which
    subcommands accept a guard. A create that asks for a guard without the key is refused, or runs shadow
    without stage 2. The harness plan picks one.
  - The setting is stored on the session row. `Policy` is built in `lifecycle.go` and `resume.go`, and
    compaction copies the row to the successor, so the column has to survive all three. The guard is not part
    of the cached prefix, so a resume may change the mode (for example `shadow` to `enforce`). A resume takes
    the guard setting from its create, and the stored value is the fallback when the create names none.
- **A decision point with context and I/O.** `Policy.Check(toolName, descriptor)` is synchronous and sees
  only the descriptor. The guard needs session state, two network calls and a timeout on each. It is a new
  package, `internal/guard`, composed in `cmd/harness`. `Executor.Execute` has the context and the call id,
  but `tools.Executor` holds no store handle by design, so `Runner` injects the guard and supplies the event
  log. DeepSeek issues parallel tool calls, so several guard checks can be in flight at once. Results still
  append in `tool_calls` order.
- **A recorded decision.** Each guard decision goes into the event log for every screened call, allowed or
  blocked: the stage that decided, rule names, p(block), stage 2's reason, latency, token usage, and the guard
  version (see below). `ToolDeniedPayload` can carry a block through its `Rule`. `ToolResultPayload` has no
  field for an allow, so the record is a new event kind or new fields. A new kind changes `internal/fold`, the
  kind list in `internal/store/events.go`, the three translators in `internal/stdiosession` and the golden
  frames. Stage 2's token cost can ride as `UsagePayload` events with `Model` set. `internal/pricing` needs a
  Haiku entry for that.
  - A resume does not run a tool call again. The fold gives an interrupted call an interrupted output, and the
    model re-issues the command under a new call id, which gets a new decision. Recorded decisions are for
    analysis and are never read back.
- **Two model clients.** Neither exists in the repo. Stage 1 is a plain HTTP client for `/v1/systemone`.
  Stage 2 is a small Messages client in the guard package. `internal/anthropic`'s client is built around the
  loop's intent (three cache breakpoints, thinking replay, `pause_turn` resumption), which stage 2 does not
  use. Both can use `providerhttp.Transport`, which retries 429 and 5xx with backoff. Each stage's timeout
  covers its retries.
- **A refusal the model can read.** A blocked call comes back with `is_error: true` and a `harness.rule` that
  names the guard and the stage. The rule string also reaches the parent, so whatever it contains is visible
  to both. Whether the text includes stage 2's reason is an open question below.
- **Shadow mode.** The guard runs and records its decision, and the command runs regardless. This gives the
  false-block rate on real DeepSeek traffic, which the evals cannot show. Their allow cases lean towards
  risky-looking commands. The 0.1% target needs real traffic to check. A shadow check does not delay the
  command: it runs beside it and commits its record once the decision arrives, on a context that survives a
  cancelled run. Shadow still sends commands to Anthropic and to the stage 1 host, so the data question below
  applies to it.
- **A guard version.** One identifier covering the rules, the two prompts, the cut-off and the model ids (the hash of the policy bundle),
  recorded with every decision. classifier-research needs it to compare decisions across changes.

## Porting notes

- Go's `regexp` package is RE2. It has no lookbehind or lookahead. `guard_rules.py` uses both: `_START` is a
  lookbehind, one rule uses a negative lookahead, and `_QUOTED` uses a negative lookbehind. Those
  need rewriting, for example as a tokenizer pass, and parity tests prove the rewrite.
- `script_detection.py` and `package_scripts.py` split words with Python's `shlex`. The Go port needs a
  splitter with the same behaviour on the cases, including the fallback to whitespace splitting on unbalanced
  quotes.
- Heredoc bodies and quoted strings are stripped before the rules run, except quoted text after `-c` or
  `eval`. That stripping is part of each rule's behaviour.

## Phases

| Phase | Repo | Work |
| --- | --- | --- |
| 1 | classifier-research | Score the chosen stage 1 design once on `autonomy-holdout` (Phase C of [autonomy-guard.md](autonomy-guard.md)). |
| 2 | classifier-research | Export parity fixtures from `autonomy-actions` only: each case's command, branch and `scripts_run`, with the expected output of `check`, `executed_scripts`, `escalation_reasons` and `package_scripts`. Never export holdout cases. |
| 3 | agent-harness | Port the rules, script detection and package script resolution to Go. Pass the parity fixtures. |
| 4 | agent-harness | Build the state from the session, the two model clients with their failure handling, and the pipeline. |
| 5 | agent-harness | Add the `create` setting, the decision point, event-log records, the stored setting across resume and compaction, and the refusal. Ship with `shadow` as the only mode a parent can turn on: a create that names `enforce` is refused. The data-sent question below is settled before this phase ships. |
| 6 | Turret | Pass the guard setting and endpoints when it spawns a harness session under `full`, and show guard refusals. Turret writes its own plan. |
| 7 | classifier-research | Import shadow-mode decisions from harness event logs as a new dataset of real DeepSeek traffic. Measure the false-block rate. Decide whether `enforce` is ready. |

Phase 1 does not block phases 3 to 5, because `shadow` runs nothing on the guard's word. It must finish
before any parent turns on `enforce`.

## Open questions for the harness plan

- **Which tools are screened.** The guard was measured on shell commands only. `write`, `edit` and
  `apply_patch` already resolve every path through `ResolvePath` against the workspace and refuse anything
  outside it, symlinks included, so they cannot reach files outside the workspace. `bash`'s `workdir` is confined the
  same way, but the command itself can touch anything. `web_fetch` can send data in a URL. MCP and client
  function tools are opaque.
- **Which models.** The harness routes DeepSeek, Gemini and Claude models through one loop. The guard could
  apply to every model or only to DeepSeek. The `create` setting lets the parent choose per session.
- **What the refusal says.** Stage 2's reason helps the model find another way to do legitimate work. It also
  tells a prompt-injected session which wording got blocked.
- **Stage 1 reachability.** The Ollama server is on the home LAN (`evals/models/clef-flash.yaml`). A host that
  cannot reach it, such as a phone away from home, fails stage 1 on every command, so every command goes to
  stage 2. That adds cost and latency per command, and if stage 2 is unreachable too, every command is blocked.
  The harness could refuse to start a guarded session when stage 1 is unreachable, or accept the fallback.
- **Data sent to Anthropic.** Stage 2 sends the command, recent commands and session script text to the
  Anthropic API. A command that contains a secret sends it there too. Shadow mode sends the same data.
- **Background jobs.** `bash` can start a background job. The guard checks the command at start. It does not
  watch what the job does later.
- **The other DeepSeek runner.** The `deepseek-flash-task` skill sends runs to a "deepseek-harness" container
  over NATS. Check whether it runs the agent-harness binary. If it runs other code, this integration does not
  cover it.
- **Subagents.** Check whether a `task` subagent builds its own `Executor` and `Policy`. If it does, the guard
  has to be handed to it, or its `bash` calls are unscreened.
- **A read-only allowlist.** [autonomy-guard.md](autonomy-guard.md) proposes one in front of stage 1 to save
  latency. It was never built or measured. Adding it in the harness first would change what the pipeline
  allows without evidence.
