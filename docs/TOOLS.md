# Tool specification

## Why these names

DeepSeek-V4 shipped with dedicated agent optimisations and names Claude Code,
OpenClaw, and OpenCode as its integration targets. DeepSeek runs its own
in-house agentic coding on it.

That is the observation. The inference this document acts on: the tool
vocabularies those harnesses use — names, argument names, result shapes — are
the ones V4 handles most reliably. The inference is not stated anywhere in
DeepSeek's docs, but matching a vocabulary costs nothing and diverging from one
costs accuracy, so the asymmetry decides it.

Claude Code and OpenCode converge on the same set. Where they differ in casing,
this spec follows Claude Code, which DeepSeek names first and supports with
explicit model mapping and a documented environment configuration.

Function names must match `[a-zA-Z0-9_-]{1,64}` and a request may carry at most
128 of them. Both vocabularies fit.

One caveat on the evidence, recorded in full in [VALIDATION.md](VALIDATION.md).
The vendored docs establish that DeepSeek optimised V4 for those harnesses. They
never state what those harnesses call their tools. The specific names below come
from outside this repository, which makes them the weakest load-bearing
assumption in the design — and also a cheap one to be wrong about, since a
rename is the whole fix.

The one concrete coding tool name DeepSeek does document is `apply_patch`:
`{"type": "custom", "name": "apply_patch"}` is accepted on the Responses API and
any other custom name returns 400. That is direct evidence of tuning against
Codex's patch format. It is flash-only and Responses-API-only, so it does not
reach our endpoint, but it suggests patch-shaped editing is trained in and worth
trying against `Edit` if exact-match replacement underperforms.

## The v1 set

| Tool | Arguments | Purpose |
| --- | --- | --- |
| `Read` | `file_path`, `offset?`, `limit?` | Read a file, line-numbered |
| `Write` | `file_path`, `content` | Create or overwrite a file |
| `Edit` | `file_path`, `old_string`, `new_string`, `replace_all?` | Exact-match replacement |
| `Bash` | `command`, `timeout?`, `description?` | Run a shell command |
| `Glob` | `pattern`, `path?` | Path matching by pattern |
| `Grep` | `pattern`, `path?`, `glob?`, `output_mode?` | Content search, ripgrep-backed |
| `List` | `path`, `ignore?` | Directory listing |
| `TaskCreate` | `tasks[]` | Append tasks to the working plan |
| `TaskGet` | `taskId` | Fetch one task by taskId |
| `TaskList` | `status?` | List the working plan (optionally one status) |
| `TaskUpdate` | `taskId`, `status?`, `subject?`, `description?`, `activeForm?` | Patch one task by taskId, or remove it with `status: "deleted"` |
| `Task` | `description`, `prompt`, `subagent_type` | Delegate to a flash-backed subagent |
| `WebFetch` | `url`, `prompt` | Fetch a URL and extract against a question |
| `ReviewScreenshot` | `image_paths[]`, `question`, `spec?` | Send screenshots to Gemini's vision model and return its diagnosis |
| `Complete` | `summary`, `result?`, `status?` | Emit the run's machine-readable result |

Fifteen tools. The first thirteen have a trained-in analogue in at least two of
the three named harnesses. `ReviewScreenshot` and `Complete` do not. The former
exists because DeepSeek cannot see images, so vision is a Gemini call this
harness builds itself; the trained-vocabulary argument above says nothing
about either, and each is named for what it does.

## Per-tool notes

### Read

Returns line-numbered content, `cat -n` style, because that is the shape the
target harnesses return and the model reads offsets out of it.

This creates a known friction with `Edit`: the line-number prefix is display
only and must not appear in `old_string`. Detect a leading line-number pattern
in `old_string` and return a targeted error naming the problem rather than a
generic no-match. The model corrects on a specific error and flounders on a
vague one.

### Write

Overwriting an existing file requires that the session has already read it. The
gate forces the model to work from real bytes rather than remembered ones.
Creating a new file needs no prior read.

### Edit

The highest-risk tool, and the one where harnesses diverge most in quality.

- `old_string` must match exactly once unless `replace_all` is set. On multiple
  matches, return the count and refuse. On zero matches, return the nearest
  candidate line range if one exists.
- A prior `Read` of the file is required.
- The result carries the applied diff, so the model sees what actually landed
  rather than assuming its intent was realised.

### Bash

The tool the permission policy exists for. Wall-clock timeout and an output byte
cap on every invocation, with truncation labelled in the result so the model
knows it saw a fragment. Both are defaults, not fixed values: an operator
changes them with `harness config set tools.bash_timeout` (and
`tools.bash_timeout_max`, the ceiling a request's own timeout is clamped to)
and `tools.output_cap` — the whole `tools.` group of settings — and the next
tool call picks the change up without a restart.

Foreground only in v1. Background shells with separate output-polling and kill
tools are a named follow-up, and they matter for dev servers and test watchers.

A command that backgrounds a process without redirecting its output —
`node server.js &`, inheriting the captured pipe — leaves that pipe open after
the shell exits, which used to wedge the call forever: `Wait` blocked on the
copy goroutines past the tool timeout, past a cancelled context. That wait is
now bounded by `tools.bash_wait_delay` (default 2s). When it fires, the harness
stops waiting, kills the process group the command ran in, and returns an error
naming the mistake and the fix — the model is the one who has to route around
it:

> the command exited but left a process holding its output open; the harness
> stopped waiting after 2s and killed the process group. Redirect output and
> detach (`cmd >/tmp/x.log 2>&1 &`) if you meant to leave something running.

The kill reaches the whole group, not just the `/bin/sh` child, so the orphan
that held the pipe dies with the call instead of outliving it and keeping
whatever port it bound. A command that detaches with its output redirected is
untouched: the pipes close with the shell, `Wait` returns normally, and nothing
is killed.

### Grep and Glob

Backed by ripgrep where available, with a Go fallback. `Grep` defaults to
returning matching file paths; content and count modes are selected by
`output_mode`. Keeping the default cheap matters because the model uses search
to orient and would otherwise pull large content into a context that gets
re-sent every sub-turn.

### TaskCreate, TaskGet, TaskList, TaskUpdate

The target harnesses all carry a todo tool, so V4 will reach for one. Where
they write the whole plan array on every change, this harness splits that
into a CRUD family: `TaskCreate` appends tasks (ids minted in call order and
stable for each task's whole life), `TaskUpdate` patches or removes one task
by taskId, and `TaskGet`/`TaskList` read one task or the whole plan back.
The field names match Claude Code's own Task tools — each item carries
`subject` ("a brief, actionable title"), `description` ("what needs to be
done"), and `activeForm`, and the task identifier is `taskId` — with the
deliberate deviations that `TaskCreate` keeps its batch-array shape
(`{"tasks": [...]}`) so seeding a whole plan is one call, and `TaskList`
keeps its optional `status` filter. No dependencies, `owner`, or `metadata`:
a harness session is a single agent, not a team with tasks to claim.

The split exists so a per-item update is cheap enough that an agent does not
skip it mid-run. Rewriting the whole plan on every status change costs the
model the full checklist to produce and the session a bigger message to
append; naming one taskId in a short `TaskUpdate` call makes keeping the
plan current the obvious move. A terminal harness renders the list as a
checklist that scrolls away. A web UI can pin it as a live plan panel beside
the transcript, which is one of the clearer wins the browser buys us.

`TaskUpdate` patches only the fields present. `status: "deleted"` removes the
task — it is only ever an input trigger, never a stored status, so a
"deleted" combined with a patch, an update with nothing to set, an unknown
taskId, or an invalid status are all errors and mutate nothing — the same
validation the event-log replay applies (`internal/store/status.go`), so a
session's recovered plan can never drift from what the loop actually did.
`TaskGet`'s single-item result shows the task's `description` too, which the
compact checklist line deliberately leaves out.

### Task

Delegates to a subagent on `deepseek-v4-flash`. DeepSeek's own recommended Claude
Code configuration sets `CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash`, so this
is both the trained-in pattern and the cheap one.

The subagent runs in its own conversation. Its transcript never joins the parent
message array — only its final result does. That keeps the parent prefix stable
and avoids mixing caches across models.

### WebFetch

Ours, not DeepSeek's. See the endpoint trade-off below. Fetch, extract to text,
then have flash answer the caller's `prompt` against the extracted content, so
the parent context receives an answer rather than a page. The fetch size cap,
the extraction cap, and the call's timeout are defaults under the `tools.`
group of settings (`tools.webfetch_max_body`, `tools.webfetch_max_extract`,
`tools.webfetch_timeout`).

### ReviewScreenshot

DeepSeek is text-only, so this is the harness's vision path: the agent captures
a screenshot itself (a browser tool, a headless-browser script) and this tool
sends it to Google Gemini for a diagnosis. It accepts PNG, JPEG, or WebP files,
workspace-confined like every other path-taking tool. The image count and the
per-file size cap are defaults, not fixed values
(`tools.reviewscreenshot_max_images`, `tools.reviewscreenshot_max_bytes`), so
the tool description quotes no numbers — it is part of the frozen request head
(docs/CACHE.md) and a number there would make the head vary per installation.
A call that exceeds a bound is refused with an error stating the actual limit,
which is how the model discovers it. The first image is sent at `high`
resolution and the rest at `medium`, per the prompting notes' advice that only
the image needing scrutiny should be high — the model is told to put the
screenshot it cares about first
([`docs/gemini-3.5-flash-ui-review-prompting.md`](gemini-3.5-flash-ui-review-prompting.md)
has the request-shape rationale: no temperature/top_p/top_k, `thinking_level`,
"data first, question last").

The model comes from the `google.vision_model` setting (default
`gemini-3.5-flash`) and the key from `google.api_key`, both read through the
settings table on every call, so either can change without a restart. The call
has its own timeout (default 60s, `tools.reviewscreenshot_timeout`) rather than
the 30-second tool default.

Known limitation: Gemini calls do not appear in a session's cost accounting —
`configs/prices.json` and `internal/pricing` cover DeepSeek only, and phase 2
deliberately does not extend them.

### Complete

The seam between an agent run and the queue that asked for it. `summary` is
prose for a human reading the transcript. `result` is the payload the requester
receives in `harness.work.result.<id>.final`. `status` distinguishes a finished
job from one the model gave up on, which is a different thing from a harness
error.

A call to `Complete` ends the run. Parallel tool calls mean it can arrive
alongside others, so the rule is that the whole batch executes in `tool_calls`
order and the run ends after it.

Two consequences fall out of constraints stated elsewhere.

It cannot be forced. Thinking mode rejects `tool_choice: required` and named
tool forcing ([OBSERVED.md](OBSERVED.md)), so there is no way to make the model
call this before it stops. The system prompt asks for it, and a run that ends
without it returns its final assistant text with a null `result`. No error path
is needed for the omission.

Its schema never varies. When a work request supplies a `result_schema`, that
schema goes in the opening user message and validation happens in Go against the
stored copy. Putting it in the tool definition would give every request a
different tool array and cost the whole shared prefix ([CACHE.md](CACHE.md)). A
payload that fails validation returns the errors through the tool result channel
and the run continues, so the model gets to correct it — but only three times.
Three consecutive rejections carrying the *same* message end the run:
`run_finished` reason `complete_rejected`, session status `failed`, queue result
`failed` with code `complete_rejected`, and an `error` event carrying the
validation message. A correction opportunity the model is not taking is a budget
leak, and one live run spent eleven sub-turns and 7% of its cost re-sending a
payload whose shape it never varied. A rejection whose message *differs* from
the last one is progress and restarts the count; the sub-turn limit remains the
backstop for a model cycling between several wrong shapes.

Because the schema lives in the opening message rather than the tool definition,
that message also carries a worked example of the call built from the schema's
own field names — `Complete(summary="…", status="done", result={"branch": …})`.
The schema alone does not say where its fields go, and the failure it invites is
sending them as top-level arguments beside `status`, which produces `result:
expected object, got null` — an accurate message about what is missing that says
nothing about what was sent. `execComplete` detects that specific case and names
the stray arguments.

`Complete` ships in every session including CLI ones that will never call it.
One tool array across every caller is what keeps the stable head shared.

## Two decisions this changes

### Strict mode is now per-tool, not global

`DESIGN.md` §4.6 said author every schema for strict mode from day one. That was
decided before the trained-vocabulary evidence.

Strict mode requires every object property to appear in `required` and
`additionalProperties` to be `false`. The trained-in schemas have genuinely
optional arguments — `Read`'s `offset` and `limit`, `Grep`'s `path` and `glob`.
Under strict those become `anyOf` null-variants that are still listed as
required, which is a different shape from the one the model was tuned against.

So: match the trained shape, non-strict, and validate arguments in Go. A bad
argument returns a clear error through the tool result channel and the loop
recovers — that channel exists for exactly this. Reserve strict mode for tools
where a malformed argument is dangerous rather than merely wrong. Strict is also
still Beta.

### Web search is only on the endpoint we are not using

DeepSeek serves a native, server-side web search tool, and Claude Code gets it
for free. The Anthropic compatibility table supports `server_tool_use` and
`web_search_tool_result` content blocks. The OpenAI-format Chat Completions API
supports `type: "function"` and nothing else.

So the choice is:

- Stay on the native endpoint (`DESIGN.md` §2) and keep strict mode, FIM, prefix
  completion, and native cache accounting — but build `WebFetch` ourselves and
  go without trained-in web search.
- Move to `/anthropic` for server-side search and lose all four.

Recommendation: stay native. Search is one tool among fifteen, our own `WebFetch`
covers the documentation-lookup case that a coding harness actually needs, and
DeepSeek's own note says its web search bills extra tokens for summarisation
anyway. The decision is reversible per-session if it proves wrong, since the
endpoint is already config.

## Execution rules

These hold for every tool and live in Go, not in prompt text.

- Paths resolve against the workspace root and are checked for escape after
  symlink resolution. Escapes are rejected, not sanitised.
- Every tool has a wall-clock timeout and an output byte cap, with truncation
  labelled in the result. Both are settings (`tools.` group) with the defaults
  listed above; the values are resolved from the settings table on each call,
  so a limit changed with `harness config set` applies without a restart.
- Tool results are appended in `tool_calls` array order, never in completion
  order. Parallel tool calling is always on and cannot be disabled: the
  Responses API guide states it outright, the Codex model catalogue declares
  `"supports_parallel_tool_calls": true` for both models, and the Anthropic
  table ignores `disable_parallel_tool_use`. Executing concurrently is fine;
  appending out of order is not, because it churns the prefix and costs the
  cache (`DESIGN.md` §3.2).
- Never send `tool_choice`. Thinking mode accepts `auto` and `none` but rejects
  `required` and named-tool forcing, so no tool can be forced while thinking is
  on (`DESIGN.md` §4.4, [OBSERVED.md](OBSERVED.md)).

## Permissions

Permission is a policy the session is given at creation, not a question it asks
later. The browser is read-only, so there is nobody there to ask.

Two modes, fixed for the life of a session and required on every request:

- Read-only. `Read`, `Glob`, `Grep`, `List`, `WebFetch`, `ReviewScreenshot`,
  `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`, and `Complete` run.
  `Write`, `Edit`, `Bash`, and `Task` are denied.
- Full access. Everything runs, as root, inside the workspace mount.

There is no third mode between them and no default. Every ingress — a work
request, the MCP launch tool, `harness run` — rejects a request that does not
name one, so no configuration value decides a session's permissions on a
caller's behalf.

A middle mode existed until it was removed. It gated `Bash` behind an
executable allowlist that included `go`, `npm`, `make`, and `python`, each of
which runs arbitrary code, so the boundary it drew was narrower than it
appeared. Read-only against full access is the distinction the harness can
actually enforce.

`WebFetch` runs in read-only mode. It reaches the network, so read-only bounds
what a session can change on disk rather than what it can send.
`ReviewScreenshot` is the same: it reads a file and sends it over the network,
changing nothing on disk.

A work request may add `deny` patterns on top of its mode. They only ever
subtract; a request cannot widen the mode it asked for.

Modes gate execution, never availability. All fifteen tools are sent on every
request in every mode, and a call the mode disallows is refused at execution
with an error result the model can read and route around. Removing tools per
mode would give each mode a different prefix and make every mode switch a cold
cache ([CACHE.md](CACHE.md)).

A denial is not an error. It returns as a tool result naming the rule that
refused it, which is the shape the model recovers from — it picks a different
approach rather than retrying the same call. Denials are recorded as their own
event kind so they are findable after the fact.

The CLI registers an interactive resolver against the same decision point, so a
terminal user is prompted for calls the policy would otherwise deny. Nothing
else registers one. Whether a session can ask a human is a property of its
caller, and every other part of the loop is identical.

## Context and compaction

DeepSeek's recommended Claude Code configuration sets
`CLAUDE_CODE_AUTO_COMPACT_WINDOW=786432` — compact at 768K of the 1M window.
Take that number as their guidance rather than deriving our own.

Compaction rewrites history and therefore resets the prompt cache (§3.2). Both
things are true and they reconcile as: compaction is a session boundary, not an
edit. At 768K the harness opens a new session seeded with a summary, links it to
the parent, and shows the boundary in the UI. The cache reset is deliberate and
visible rather than an unexplained cost spike.

## Deferred

Background shells with output polling and kill. Notebook editing. MCP tool
import. FIM-backed inline completion. Edit checkpointing and rollback. Each is
additive against this set.
