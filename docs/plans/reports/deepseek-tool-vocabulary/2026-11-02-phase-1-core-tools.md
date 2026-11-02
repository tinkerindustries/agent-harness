# Phase 1: core tools

## What was built

Every built-in tool now carries its `lower_snake_case` name from the plan's mapping
table. The tables keyed by a tool name follow the rename: `toolFuncs` and `timeoutFor`
in `internal/tools/registry.go`, `alwaysAllowed` and `mcpAccessServerOf` in `policy.go`,
`descriptorFor` in `descriptor.go`, `MutationTarget` in `mutation.go`, the task
comparisons in `internal/session/livestate.go`, `tooldispatch.go` and `turn.go`, and the
`complete` comparison in `turn.go`. `internal/store/status.go` accepts the old and the
new spellings, so a plan written before the rename still reconstructs; a test covers the
new spelling and the existing one keeps covering the old.

`bash` replaces `Bash`. Its `timeout` is now `timeoutMs`, and it gains `workdir`, which
resolves through `ResolvePath` against the workspace, must name an existing directory, and
applies to the foreground and `run_in_background` paths alike.

`job_output` replaces `BashOutput` and takes `job_id`, `filter` and `wait`. `wait` parks
on a channel that `cmd.Wait` closes and returns the output read so far when the call's own
timeout expires, leaving the job running. `job_kill` replaces `KillBash` and takes
`job_id`; killing a finished job stays a no-op. `job_list` is new and lists every job the
session's registry holds, in start order, with its id, state, start time and command.

`internal/tools/bashbackground.go` gained an ordered index on `Shells`, a `list()` that
pairs ids with their shells under both locks, and a `wait()` method. `backgroundShell`
records its command and carries the completion channel.

`internal/session/prompt.go` has a new `toolOrder` and new text for every fragment. Each
sentence is written around the new names: "Call read on a file before overwriting it with
write or changing it with edit", "a grep beside a glob, bash commands", "Prefer grep and
glob", "read returns the image". The background-job fragment names `job_list`. The
`needs` lists are unchanged in shape and now name the new tools.

`internal/promptvariant/variants.go` has its `no-bash` drop list and description updated.
The variant now drops `bash`, `job_output`, `job_kill` and `job_list`, because a job
listing means nothing without bash to start the job. The description no longer restates a
tool count.

The scope change arrived while this phase was in progress and is built. `DefinitionsFor`
resolves a DeepSeek model to `definitionsVisionCapable` minus `list`, the four task
tools, `task` and `complete`; Kimi, Gemini and Claude keep all seven. The base array stays
full. DeepSeek's hosted array is ten tools, Kimi's seventeen, Claude's sixteen and the
base twenty-three. `tools_deepseek_flash.golden.json` and
`prompt_deepseek_flash.golden.txt` are new, and the `deepseek-flash` cases in both golden
tests read them. `TestDefinitionsForProviderShape` gained an `assertSubtraction` helper
and now pins that DeepSeek's hosted array is Kimi's minus the seven as well as Kimi's
being the base minus the six vision tools.

The four tool goldens, the three prompt goldens and `internal/wire`'s request-body golden
were regenerated from the functions the tests call. The tool and prompt goldens came from
a throwaway program under `scratch/` that called `tools.DefinitionsFor` and
`session.RenderSystemPromptFor`; the wire golden came from a throwaway test in the wire
package that called the test's own `goldenRequest`. Both throwaways are deleted.

Model-facing prose that named a tool was updated with it: the tool error messages, the
opening message's `complete` schema note and `glance` attachment note, the claudemd
truncation notice, the skills catalogue, the two `internal/stdiosession` unsupported-block
errors, and the `-rg` flag help. Tool names in test fixtures that model old event logs or
provider blocks were left alone: `internal/fold`, `internal/anthropic`,
`internal/gemini`, `internal/deepseek`, `internal/hub`, `internal/store`'s mirror and
`internal/session/thinking_prefix_test.go`. Live dispatch in the tests was moved to the
new names, including the `internal/stdiosession` fixtures and the `web_fetch` case in
`claude_test.go`.

## What was verified, and how

The phase's proof, run with `-count=1` so nothing came from the test cache:

```
$ go test -count=1 ./internal/tools ./internal/session ./internal/promptvariant
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	8.842s
ok  	github.com/mrgeoffrich/agent-harness/internal/session	10.532s
ok  	github.com/mrgeoffrich/agent-harness/internal/promptvariant	0.625s
```

The rename reaches beyond those three packages, so the whole tree was checked as well.
`go test ./...` passed every package. The repository gate also passed:

```
$ scripts/build.sh
...
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

exit code 0.

The new behaviour has its own tests: `TestJobListReportsEveryJob`,
`TestJobListWithoutJobsSaysSo`, `TestJobOutputWaitBlocksUntilTheJobFinishes` and
`TestJobOutputWaitTimesOutAndLeavesTheJobRunning` in
`internal/tools/bashbackground_test.go`, and `TestApplyTaskEventAcceptsTheNewToolNames`
in `internal/store/status_test.go`. The first two check the listing's rows and its empty
answer. The wait tests check that the call blocks until the job exits, and that a wait
bounded by the context returns the output so far with the job still running.

## What was left undone

`job_output`'s `wait` runs against the ordinary default tool timeout, thirty seconds. The
plan gave the argument no budget of its own, and widening it would have been a
behaviour change this phase does not name. A model waiting on a build longer than that
still has to call `job_output` again, which now costs one call rather than a poll loop.

Nothing from phases 2, 3 or 4 is here: no `apply_patch`, no
`list_mcp_resource_templates`, and no documentation change. `docs/TOOLS.md` still
describes the TitleCase names, the old argument spellings and the old array sizes.

Operator deny patterns that name an old tool no longer match. The plan's risk list says
the release note carries that; the release note is documentation, which phase 4 owns.

## Bugs found and not fixed

None that remain. Two things are worth recording.

`TestVariantDroppedToolRendersAHeadConsistentWithItsArray` asserted the absence of a rule
string that had already stopped existing ("- The shell is bash in an Alpine container"),
so that half of the check was vacuous. It now names the current shell rule, the batch rule
and the plan bullet, and those assertions are live.

Anthropic's own server-side `web_fetch` and this harness's `web_fetch` now share a name.
A Claude request carries exactly one of them, because Claude's array drops the harness's
tool, so nothing in the request path confuses them. The `internal/stdiosession` test that
asserts the harness's tool is absent had matched on the name alone and now tells them
apart by `input_schema`.

## Mechanical friction

The suite's default Bash timeout of two minutes was not enough for
`go test ./internal/session` while a test was hanging on a poll loop that could never be
satisfied. Running the package by itself with `-timeout` and `-v` found the test in one
pass. The hang was an unknown-tool result from a fixture still emitting an old name, not
a slow test.

Lowercase names break substring checks. "ground" is inside "run_in_background", "list" is
inside "job_list", and "screenshot" is an ordinary noun. The prompt test that scans a
rendered head for another provider's tool names needed a word-boundary match, and two
fragments were reworded to keep ordinary nouns out of the way: "a screen capture" for the
scratch rule and "the run's deliverable" in place of "the task's deliverable".

The committed goldens have no `-update-golden` flag, so regenerating them means a
throwaway program that calls the same functions. That is quick, but it leaves nothing in
the tree for the next phase to rerun.

## Memory suggestions

- The base array is served to no real provider. `tools.Definitions()` returns it and an
  unknown model falls back to it, so its golden still moves; DeepSeek's hosted array,
  Kimi's, Gemini's and Claude's are the ones a session is actually sent.
- `web_fetch` is also Anthropic's server-side tool name. A Claude request carries
  Anthropic's one, so any check on a request's tools has to tell them apart by shape
  (`input_schema` for a client tool, `type` for a server tool) rather than by name.
- Lowercase tool names collide with English words. Any test that scans prompt or message
  text for a tool name needs word boundaries, and a fragment that uses one as a noun is a
  false positive waiting to happen.
- `job_output`'s `wait` is bounded by the ordinary tool timeout. The bash default and
  ceiling do not apply to it.
- `RenderSystemPrompt()` renders the base array's head. A provider's head comes from
  `RenderSystemPromptFor(model, variant)`, and DeepSeek's is the ten-tool one.
- The `no-bash` variant drops `job_list` with the other three shell tools.
