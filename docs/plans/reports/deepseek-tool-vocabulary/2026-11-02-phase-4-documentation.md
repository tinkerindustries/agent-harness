# Phase 4: documentation

## What was built

The catalogue and every current-state document speak the `lower_snake_case`
vocabulary the code has carried since phase 1, with phase 2's `apply_patch` and
phase 3's `list_mcp_resource_templates` included.

`docs/TOOLS.md`, rewritten throughout:

- "Why these names" gives the real reason now: the vocabulary is the one
  [`deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness)
  uses, and `apply_patch` is the one tool name DeepSeek's API documents as
  tuned. The old text argued from Claude Code's `TitleCase` set.
- "The built-in set" (was "The v1 set") carries the new table. `apply_patch`,
  `job_list` and the argument spellings are all there: `timeoutMs`, `workdir`,
  `job_id`, `filter`, `wait`, `input`.
- The per-provider arrays are recorded with no counts. Kimi, Gemini and Claude
  carry the vision-free array and keep all seven harness-added tools. DeepSeek
  carries that array minus `list`, the four task tools, `task` and `complete`.
  The authority named is `internal/tools/definitions.go`.
- A new `### apply_patch` section records the grammar, the in-memory staging,
  the prior-read gate, the exact hunk matching and the `no-patch` variant.
- `### bash` gains `workdir`, and the job tools' `wait` and listing behaviour.
- Every per-tool heading is the tool's own name.
- "Web search is not on the endpoint we use" became "Web search is not available
  on the surface we speak". It says this process posts to `/responses`, that the
  surface supports `function` tools and the `apply_patch` custom tool and
  ignores every built-in tool type, and that `web_fetch` is the harness's own.
- Permissions: `apply_patch` is denied in read-only mode. Its descriptor is its
  bare name, so `-deny apply_patch` is the pattern that targets it. `crop` left
  the read-only denied list, which contradicted both the code and the `crop`
  section above it.
- Execution rules name `tools.MutationTargets` (phase 2 renamed it from
  `MutationTarget`) and cover `apply_patch` in the same-file ordering rule.
- "Deferred" no longer lists background-shell polling and kill, which exist.

`docs/MCP.md`: the five access tools are named by the names they have, in the
resources table and in the prose below it. "alongside the built-in twenty" lost
its count. The MCP protocol's `completion/complete` request is spelled that way
so it cannot be read as this harness's `complete`.

`ARCHITECTURE.md`, `internal/CLAUDE.md` and `docs/DESIGN.md`: every tool name,
the array descriptions and the counts. `internal/CLAUDE.md`'s `internal/tools`
entry now says what each array is and where it is built.

The remaining current-state documents: `docs/CACHE.md`,
`docs/VISION-TOOLKIT.md` (section 7 onward), `docs/DEEPSEEK-VISION.md`,
`docs/RUN-CONTROL.md`, `docs/GEMINI-INTEGRATION.md`,
`docs/KIMI-INTEGRATION.md`, `docs/ANTHROPIC-INTEGRATION.md`,
`docs/STDIO-PROTOCOL.md`, `docs/STDIO-INTERACTIONS.md`,
`docs/STDIO-MANAGED-AGENTS.md`, `docs/MODELS.md`,
`docs/GEMINI-BUILTIN-TOOLS.md`, `README.md` and `TESTING.md`.

Two documents outside the phase's list needed the same work and got it:

- `CLAUDE.md` (the repository's own), which named `WebFetch`.
- `docs/VALIDATION.md`, whose "Unvalidated" section still argued for the Claude
  Code vocabulary, and whose last paragraph said the `deepseek-ai` org had no
  harness repository. Left alone, it would have contradicted the new
  `docs/TOOLS.md` that cites it.

Code comments and non-model-facing strings:

- Every comment naming an old tool now names the current one, across
  `internal/tools`, `internal/session`, `internal/store`, `internal/deepseek`,
  `internal/anthropic`, `internal/gemini`, `internal/fold`, `internal/skills`,
  `internal/hub`, `internal/claudemd`, `internal/provider`, `internal/wire`,
  `internal/stdiosession`, `internal/promptvariant`, `internal/settings` and
  `cmd/harness`. A second pass after review found the first one too narrow in
  both directions. It had only searched for the names with no English meaning,
  so `Bash`, `Screenshot`, `Glance`, `Ground`, `Detect`, `Crop` and
  `Transcribe` were still in comments in more packages. And it had not
  searched at all for `Read`, `Write`, `Edit`, `List`, `Glob`, `Grep`,
  `Complete`, `Task` or `TodoWrite`, which are the names most likely to be
  mistaken for English. `TodoWrite` is the plan tool's predecessor and named
  the wrong writer for the plan column in five files; it now says
  `task_create` and `task_update`.
- Every comment restating an array size names `internal/tools/definitions.go`
  instead: three in `definitions.go`, and one each in `policy.go`,
  `registry.go`, `promptvariant/variants.go`, plus two in `prompt.go` and three
  in `prompt_test.go`. The assertions in `prompt_test.go` that pin a count word
  stay, because they are the one place a size change has to be acknowledged.
- The `internal/settings` descriptions are operator-facing text rather than
  model-facing. They named `WebFetch`, `Task`, `Glance`, `Ground`, `Detect`,
  `Crop`, `Screenshot`, `Transcribe` and `Bash`, and now name the current
  tools. No test asserts on them. The first pass updated five of them and left
  the rest; the review's count found the gap and it is closed.
- No tool description, no system prompt fragment and no opening message
  changed, and no golden file moved.

Three model-facing strings outside the frozen head did change, all of them
naming tools the model does not carry:

- `internal/store/events.go`'s `BackgroundFinishedPayload.Message()` told the
  model to read a finished shell "with BashOutput using bash_id". It now says
  `job_output` and `job_id`, and `internal/store/events_test.go` pins the whole
  notice. `TestFoldBackgroundFinished` compares `Message()` to itself, so it
  could not have caught this.
- `internal/promptvariant/reminders.go`'s three search reminders said "search
  with the Grep and Glob tools rather than through Bash", and the read reminder
  named `Read`, `Edit` and `Write`. All four now use the current names.
- The test failure messages that named a tool by its old name now name the
  current one. No assertion changed.

`docs/TOOLS.md`'s paragraph after the built-in table said the vision tools
exist "because most of the models this harness runs cannot see images". Every
model this harness routes reads images natively, so no routed session is
offered the six vision tools; they remain only in the base array. The
paragraph and the `glance` section's opening now say that.

## What was verified, and how

`gofmt -l cmd internal` printed nothing, and `go vet ./cmd/... ./internal/...`
printed nothing, before the gate ran.

The gate, `scripts/build.sh`, exit code 0:

```
=== gofmt
clean

=== go vet
clean

=== tests
?   	github.com/mrgeoffrich/agent-harness/cmd/drive	[no test files]
ok  	github.com/mrgeoffrich/agent-harness/cmd/harness	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/anthropic	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/deepseek	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/fold	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/gemini	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/mcpclient	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/promptvariant	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/session	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/settings	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/skills	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/stdiosession	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/store	(cached)
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	9.891s
ok  	github.com/mrgeoffrich/agent-harness/internal/wire	(cached)

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

That run came after one that failed on
`TestJobOutputConcurrentPollsDoNotPanicOrDuplicate` (reported below). Before
the second attempt the test cache had been cleared, so the packages above that
say `(cached)` had run for real in the failed attempt and passed; `internal/tools`
ran again and passed. The gate is the proof for the Go comments. The documents are not tested, so the
searches are their proof. Each ran from the workspace root, over `*.md` outside
`third_party/`, `docs/plans/`, `docs/reviews/`, `docs/sources/` and
`docs/OBSERVED.md`:

- `\b(BashOutput|KillBash|WebFetch|TaskCreate|TaskGet|TaskList|TaskUpdate|MCPListResources|MCPReadResource|MCPListPrompts|MCPGetPrompt)\b`:
  no hits.
- `\b(Screenshot|Glance|Transcribe|Ground|Detect|Crop)\b`: four hits, each
  judged and kept. Two are Playwright's own "Screenshot" in the generated
  skill, one is `Crop` in `docs/VISION-TOOLKIT.md` §6's historical proposal,
  and one is the verb "Detect" in `docs/TOOLS.md`.
- `\b(twenty|fourteen|sixteen|seventeen|eighteen|nineteen)\b`: the only array
  count left is `docs/MCP.md`'s Blender tool count, which is the server's own
  and not this harness's array.
- The same old-name search over `internal/` and `cmd/` for `*.go`, filtered to
  hits that are not part of a Go identifier: the only names left are the
  deliberate ones. `internal/store/status.go`'s two `"TaskCreate"` /
  `"TaskUpdate"` cases and `internal/store/status_test.go`'s fixtures replay a
  pre-rename plan on purpose. `internal/tools/checklist.go` names Claude Code's
  own `TaskCreate`. `internal/tools/bash_windows.go` names Git Bash.
  `internal/store/events_test.go` lists the old names to prove they are absent.
  `internal/tools/registry.go`'s `WebFetch`, `Screenshot` and `Transcribe`
  fields are identifiers. Three `Transcribe` occurrences are the imperative
  verb in a prompt sent to Gemini, not a tool name.
- The full Go scan first looks like it still finds 446 hits. Nearly all of them
  are identifiers (`DefaultBashTimeout`, `execGlance`, `encodeScreenshot`,
  `TestExecGround…`) and the setting keys under `internal/settings`, which keep
  their names because an operator's stored configuration depends on them.

## What was left undone

Documents left as historical records, and why:

- `docs/OBSERVED.md` quotes `TaskCreate` and `WebFetch` in measured findings.
  Those record runs made under the old names, and measured findings are left
  alone.
- `docs/reviews/`, `docs/sources/`, `docs/plans/` and `third_party/` are
  historical by instruction.
- `docs/VISION-TOOLKIT.md` sections 1 to 6 are the assessment that led to the
  port, and the document says so. Section 6's `Locate` and `Crop` are the
  proposal as it stood then. Sections 7 and 8 describe the shipped tools and
  are updated.
- `docs/DEEPSEEK-VISION.md` section 5 quotes session ids and `Read` calls from
  runs made before the rename. The surrounding description of what those runs
  showed is unchanged.
- `docs/KIMI-INTEGRATION.md`'s phase records stay as records. Their tool names
  and counts are current.
- `docs/GEMINI-INTEGRATION.md` is written as an integration plan with phases.
  Its tool names are current. Its plan-shaped claims about what is built were
  not audited.
- `.claude/skills/playwright-cli/` is generated and untouched.

## Bugs found and not fixed

**`docs/DEEPSEEK-RESPONSES.md` section 1 has a broken sentence.** It reads
"`harness stdio-session` posts to `/responses`. `/chat/completions`, and §5
says why that is a decision rather than an oversight." The middle of the
sentence is missing. It has nothing to do with the vocabulary and is left
alone.

**`docs/KIMI-INTEGRATION.md` section 4.5 opens with a stale claim.** "Today no
image reaches any model but Gemini's" was true when the Kimi plan was written.
`deepseek-flash` and Kimi both read images natively now, and the same section
describes Kimi's own multimodal path in its next paragraph. Left alone for the
same reason as the sentence above: it is an accuracy issue outside the
vocabulary sweep.

**`TestJobOutputConcurrentPollsDoNotPanicOrDuplicate` is still flaky under
load.** Phase 3 replaced its joined-text assertion with a byte sum, and it
passes alone: 20 runs here, one after another. In the first fully uncached gate
run of this phase, with every package's tests in flight at once, all 400
concurrent polls returned before the shell had written anything, so the call
ended with 0 of 1692 bytes. Nothing in the test makes the polls wait for the
first line. The gate passed on the next attempt. It is a phase-1 test and its
fix is not part of this phase.

**Two contradictions in `docs/TOOLS.md` were fixed rather than reported.** The
read-only list denied `crop`, which the code allows and the `crop` section
explains. And the execution rules named `tools.MutationTarget`, which phase 2
renamed to `tools.MutationTargets`. Both are in "What was built".

## Mechanical friction

Lowercase tool names are hard to search for. "list" is a verb and sits inside
"job_list", "read", "write", "task", "ground" and "complete" are English words,
and "ground" sits inside "run_in_background". Every hit needed a judgement, so
the backtick-quoted form was the only reliable mechanical replace and the bare
names were read one at a time.

`docs/TOOLS.md` is more than a thousand lines of prose where every section names
tools in passing. The work was a full read and a series of edits. Renaming the
headings then made every cross-reference ("docs/TOOLS.md, `"Bash"`") a second
pass, in this repository and in the three stdio documents.

The gate runs gofmt, vet, the suite and six cross-compiles. It is long enough
that one run at the end is the only practical shape, which means a doc edit
made afterwards cannot be covered by it. There is no docs linter, so nothing
checks a heading reference or a tool name in prose.

The first comment sweep was too narrow, and the review found it. Its greps
looked for the names with no English meaning (`BashOutput`, `WebFetch` and the
rest), so the names that double as words (`Bash`, `Screenshot`, `Glance`,
`Ground`, `Detect`, `Crop`, `Transcribe`) were never searched for in Go at all.
The same narrowness cut the other way in `internal/settings`: the file's
*identifiers* contain those names (`KeyToolBashTimeout`), so a grep that was
meant to find descriptions found keys instead and the count said 39 when the
prose had fewer. A name that is also a word needs a search that separates
identifiers from prose, which is what the second pass did.

## Memory suggestions

This is the last phase, so most of what the feature needed is already in the
memory. Two additions for the next feature:

- `internal/tools/definitions.go` is where an array's membership is decided and
  where a document should point. A count in prose goes stale at the next tool.
- `internal/settings`' description strings are operator-facing text that names
  tools. A rename has to reach them too, and no test watches them.

One correction to the memory's "Files that matter" table for whoever sweeps a
vocabulary next: the sweep reaches `docs/VALIDATION.md`, the repository's own
`CLAUDE.md` and the settings descriptions, none of which the plan listed.
