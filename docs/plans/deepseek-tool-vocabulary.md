# Tool vocabulary alignment with the official DeepSeek harness

- **Slug**: deepseek-tool-vocabulary
- **Reports**: [reports/deepseek-tool-vocabulary/](reports/deepseek-tool-vocabulary/)

## What this builds

The built-in tool array speaks the vocabulary of
[`deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness)
(`dsh`), the vendor's own harness:

- Every built-in tool name becomes `lower_snake_case`, which is how `dsh`
  names all of its tools.
- The tools both harnesses have take `dsh`'s names and argument spellings:
  `read`, `write`, `edit`, `bash`, `glob`, `grep`, `job_output`, `job_list`,
  `job_kill`, `web_fetch`.
- `bash` gains `workdir`, and its `timeout` becomes `timeoutMs`.
- `job_output` takes `job_id` and gains `wait`; `job_kill` takes `job_id`;
  `job_list` is new.
- `apply_patch` joins the array, carrying a Codex-format patch in an `input`
  string.
- `list_mcp_resource_templates` joins the four MCP access tools.
- The tools this harness added on top keep their behaviour and adopt the same
  casing: `list`, `task_create`, `task_get`, `task_list`, `task_update`,
  `task`, `complete`, `screenshot`, `glance`, `transcribe`, `ground`,
  `detect`, `crop`.

The reason is evidence. `docs/TOOLS.md` chose Claude Code's `TitleCase` names
because DeepSeek names Claude Code and OpenCode as integration targets. The
vendor's own harness sits closer to the model's training data than that
inference does, and it uses lower case throughout. It also ships
`apply_patch`, the one tool name DeepSeek documents as trained
(`third_party/deepseek-docs/api/create-response.md`, `tools`).

## Not building

- **Schedules**: `schedule_create`, `schedule_list`, `schedule_view`,
  `schedule_edit`.
- **Subagents**: `subagent`, `subagent_fork`, `list_agents`, `send_message`,
  `interrupt_agent`. The existing `task` tool keeps its blocking,
  flash-backed behaviour and its name changes casing only.
- **`web_search`**: `dsh` backs it with a search provider plugin. This process
  has no search backend, and the Responses API ignores built-in tool types
  (`third_party/deepseek-docs/guides/responses_api.md`). Adding it means a new
  provider, credential and billing path, which is its own feature.
- **`read_image`**: `read` already returns image parts on any model
  `provider.SeesImages` names, and a second image path would duplicate it.
- **`ask_user_question`, `present`, `lsp`, `exit_plan_mode`, goals,
  `skill`, `cordis_inspect_*`, `plugin_manager`, `plugin_market`**: each
  needs a host surface this process does not have. Those are an interactive
  parent channel, a deliverables UI, a language-server registry, a goal store,
  a skill loader and a plugin graph respectively.

## Decisions

### The task tools keep their model

`dsh` replaces its whole task list with one `todo_write` call carrying the
complete array. Each item carries `content` and `status` only. This harness
has four tools that mint ids and patch fields, and its items carry `taskId`,
`subject`, `description`, `status` and `activeForm`
(`internal/store/status.go`'s `StatusTodo`).

Adopting `todo_write` drops the id, the description, the present-continuous
`activeForm`, `status: "deleted"`, and `TaskGet`. It rewrites
`internal/session/livestate.go` and `internal/session/tooldispatch.go`, and it
deletes the event-log replay of `TaskCreate`/`TaskUpdate` in
`internal/store/status.go`. The `plan` column itself already holds a todos
array, so it survives with a smaller item shape.

The benefit is one matching tool name. The cost is a smaller capability and a
loop change that has nothing to do with vocabulary. This plan keeps the four
tools and changes their casing.

### `apply_patch` ships enabled

`dsh` ships it disabled behind an `applyPatch` config flag. This harness has no
per-tool configuration plane, so the choice is whether the array carries it at
all.

It ships enabled beside `edit`. `apply_patch` is the one tool name DeepSeek
documents as tuned, and a patch that spans files or separated regions is one
call instead of several. `internal/promptvariant` gains a `no-patch` variant so
the two editing tools can be compared on the same tasks, and a variant is the
way to withdraw it if the array turns out to be better without it.

### `web_search` stays out

Recorded in "Not building" above. Phase 4 corrects the stale section in
`docs/TOOLS.md` that says this harness posts `/chat/completions` and that the
Responses API accepts `web_search` as a tool type. Both are wrong now.

## The mapping

| Now | After | Argument change |
| --- | --- | --- |
| `Read` | `read` | none |
| `Write` | `write` | none |
| `Edit` | `edit` | none |
| `Bash` | `bash` | `timeout` becomes `timeoutMs`; gains `workdir` |
| `BashOutput` | `job_output` | `bash_id` becomes `job_id`; gains `wait`; keeps `filter` |
| `KillBash` | `job_kill` | `shell_id` becomes `job_id` |
| | `job_list` | new |
| `Glob` | `glob` | none |
| `Grep` | `grep` | none |
| `List` | `list` | none |
| `TaskCreate` | `task_create` | none |
| `TaskGet` | `task_get` | none |
| `TaskList` | `task_list` | none |
| `TaskUpdate` | `task_update` | none |
| `Task` | `task` | none |
| `WebFetch` | `web_fetch` | none |
| `Screenshot` | `screenshot` | none |
| `Glance` | `glance` | none |
| `Transcribe` | `transcribe` | none |
| `Ground` | `ground` | none |
| `Detect` | `detect` | none |
| `Crop` | `crop` | none |
| `Complete` | `complete` | none |
| `MCPListResources` | `list_mcp_resources` | none |
| `MCPReadResource` | `read_mcp_resource` | none |
| `MCPListPrompts` | `list_mcp_prompts` | none |
| `MCPGetPrompt` | `get_mcp_prompt` | none |
| | `list_mcp_resource_templates` | new |
| | `apply_patch` | new |

`web_fetch` keeps this harness's behaviour: it fetches a URL and answers a
question about it with the flash model. `dsh`'s returns decoded page text.
The name matches; the description carries the difference.

## Phases

| Phase | Name | Depends on | Runs beside |
| --- | --- | --- | --- |
| 1 | Core tools: files, search, shell and jobs | nothing | 2 |
| 2 | `apply_patch` | 1 | 3 |
| 3 | MCP resource templates | nothing | 2 |
| 4 | Documentation | 1, 2, 3 | nothing |

Phase 1 carries every rename of a tool both harnesses have, because all of
them edit `internal/tools/definitions.go` and `internal/tools/registry.go`.
It also rewrites `internal/session/prompt.go`, because the system prompt's
inventory is built from the array and its wording names the tools.

Phases 2 and 3 add tools and touch different files, so they can run beside
each other. Phase 4 is documentation only.

Each phase regenerates the committed goldens its own change moves. A rename or
an added tool always moves the three tool arrays. It moves the two prompt
goldens whenever the array's membership or a fragment's wording changes, which
is every phase here except phase 3.

### Phase 1: core tools

**What it builds**

- `read`, `write`, `edit`, `glob`, `grep`, `list` and the four task tools take
  their new names.
- The whole mapping table is this phase's rename list. Besides the tools above,
  that is `WebFetch` → `web_fetch`, `Task` → `task`, `Complete` → `complete`,
  the six vision tools' casing (`Screenshot`, `Glance`, `Transcribe`, `Ground`,
  `Detect`, `Crop`), and the four MCP access tools' names
  (`list_mcp_resources`, `read_mcp_resource`, `list_mcp_prompts`,
  `get_mcp_prompt`). No later phase renames a tool.
- Every table keyed by tool name follows: `internal/tools/registry.go`'s
  dispatch map, `policy.go`'s `alwaysAllowed`, `descriptor.go`'s
  `descriptorFor`, `mutation.go`'s `Edit`/`Write` case, the task comparisons
  in `internal/session/livestate.go`, `tooldispatch.go` and `turn.go`, and
  `internal/store/status.go`'s task replay, which accepts both the old and
  the new names so a resumed session's plan still reconstructs from events
  written before the rename.
- `bash` replaces `Bash`, gains `workdir`, and its `timeout` becomes
  `timeoutMs`. `execBash` resolves `workdir` against the workspace with the
  same confinement check every path argument goes through.
- `job_output` replaces `BashOutput` and takes `job_id` plus `wait`. `wait`
  blocks until the job finishes or the call's timeout expires, and a timed-out
  wait leaves the job running. `filter` stays.
- `job_kill` replaces `KillBash` and takes `job_id`.
- `job_list` is new: every live job id with its command, start time and
  running or exited state, read from `tools.Shells`.
- `internal/tools/bashbackground.go` grows the listing and wait methods.
- `internal/session/tooldispatch.go`'s `Bash` live-state case.
- `internal/session/prompt.go`: `toolOrder` and every `toolFragments` entry.
  The fragment text names tools in prose and builds words on them
  ("Write-ing", "Edit-ing", "Bash commands"). Each sentence is rewritten
  around the new names.
- `internal/promptvariant/variants.go`: the `no-bash` drop list and its
  description.
- `internal/tools/testdata/tools_{claude,deepseek,kimi}.golden.json` and
  `internal/session/testdata/prompt_{deepseek,kimi}.golden.txt`, regenerated.

**Proof**

```
go test ./internal/tools ./internal/session ./internal/promptvariant
```

### Phase 2: `apply_patch`

**What it builds**

A new `internal/tools/patch.go`, wired into the definitions, the dispatch map
and the policy.

- The argument is `input`, a string, named as Codex names it.
- The grammar is `*** Begin Patch` to `*** End Patch`, with `*** Add File:`,
  `*** Update File:` plus an optional `*** Move to:`, `*** Delete File:`, and
  hunks of `@@` context and space, `+` and `-` lines. `*** End of File`
  anchors the last hunk.
- Paths resolve against the workspace root and go through the same escape
  check as `write` and `edit`.
- The patch is parsed and every hunk applied to an in-memory copy before any
  file is written. A hunk that does not apply fails the whole call and leaves
  the workspace untouched.
- The tool is mutating, so it stays out of `alwaysAllowed`, the way `write`
  and `edit` do, and read-only mode denies it.
- `internal/promptvariant`: a `no-patch` variant that drops it, with its
  description.
- `internal/session/prompt.go`: the `apply_patch` entry in `toolOrder`.
- The three tool goldens and the two prompt goldens, regenerated.

**Proof**

```
go test ./internal/tools -run ApplyPatch
scripts/build.sh
```

The test table covers add, update, delete, move, multi-file, a hunk that
misses, a path that escapes the workspace, and a malformed header.

### Phase 3: MCP resource templates

**What it builds**

`list_mcp_resource_templates` joins the MCP access tools. Its listing reads
the stored snapshot through `MCPProvider.Resources` and filters `Template`,
the same way `list_mcp_resources` reads it. No live call: listings read the
snapshot so an unreachable server still lists what it had, and the probe
already snapshots `resources/templates/list` (`internal/mcpclient/refresh.go`).

- `internal/tools/mcpresources.go`: the fifth access tool and its executor.
- `internal/tools/definitions.go`: its schema and `MCPAccessToolCount`.
- `internal/tools/registry.go`'s `toolFuncs`, `policy.go`'s `alwaysAllowed`
  (a listing reads the stored snapshot, so it is always allowed), and
  `descriptor.go`'s case, as its four siblings have.
- `docs/MCP.md`: the tool list and the resources table.

The MCP access tools join an array only when servers are configured, so this
phase moves no golden file.

**Proof**

```
go test ./internal/mcpclient ./internal/tools
```

### Phase 4: documentation

**What it builds**

- `docs/TOOLS.md`: the catalogue, the per-tool headings and the execution
  rules. The "Web search is not on the endpoint we use" section is corrected
  to describe the Responses surface and the fact that built-in tool types are
  ignored there.
- `internal/CLAUDE.md`, `ARCHITECTURE.md` and `docs/DESIGN.md` wherever a tool
  is named.
- Wherever a comment or a document restates the size of an array ("twenty-two",
  "sixteen", "twenty"), replace the number with a pointer to
  `internal/tools/definitions.go`. Two of those counts are already stale, and
  a new number would be stale again at the next change.

**Proof**

```
scripts/build.sh
```

The build is the gate: it runs gofmt, vet, the suite and every release build.
`TestToolArrayGolden` and `TestPromptGolden` fail until the goldens are
regenerated, which is the point of them.

## Risks and one-time costs

- **The cache prefix changes once for every live session.** Renaming a tool
  moves the serialised head, so the first request after the change re-reads
  the whole conversation at full price. Sessions resume normally; the event
  log is untouched.
- **Operator deny patterns stop matching.** `descriptorFor` builds the string
  a pattern matches against, and that string starts with the tool name. A
  stored `-deny Bash` no longer matches `bash`. Operators with stored patterns
  need to rename them, and the release note has to say so.
- **A resumed session's history carries the old names.** `fold` renders the
  completed `function_call` items from the log verbatim, so an old session's
  history offers `Read` while its array offers `read`. The loop dispatches new
  calls by the new names, so the session keeps working. A name map in `fold`
  would rewrite history and break the byte-for-byte replay the cache depends
  on; leave the old names in old logs.
- **Three tool goldens and two prompt goldens change.** That churn is expected
  and is the evidence the change reached every surface.
- **`apply_patch` widens the editing surface.** It is a second mutating file
  tool with its own failure modes, and the model may choose it where `edit`
  was more reliable. The `no-patch` variant is how that gets measured.
