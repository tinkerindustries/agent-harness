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
| `TodoWrite` | `todos[]` | The model's working plan |
| `Task` | `description`, `prompt`, `subagent_type` | Delegate to a flash-backed subagent |
| `WebFetch` | `url`, `prompt` | Fetch a URL and extract against a question |

Ten tools. Each has a trained-in analogue in at least two of the three named
harnesses.

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

Approval-gated by default (see Permissions below). Wall-clock timeout and an
output byte cap on every invocation, with truncation labelled in the result so
the model knows it saw a fragment.

Foreground only in v1. Background shells with separate output-polling and kill
tools are a named follow-up, and they matter for dev servers and test watchers.

### Grep and Glob

Backed by ripgrep where available, with a Go fallback. `Grep` defaults to
returning matching file paths; content and count modes are selected by
`output_mode`. Keeping the default cheap matters because the model uses search
to orient and would otherwise pull large content into a context that gets
re-sent every sub-turn.

### TodoWrite

The target harnesses all carry a todo tool, so V4 will reach for one. A terminal
harness renders it as a checklist that scrolls away. A web UI can pin it as a
live plan panel beside the transcript, which is one of the clearer wins the
browser buys us.

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
the parent context receives an answer rather than a page.

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

Recommendation: stay native. Search is one tool among ten, our own `WebFetch`
covers the documentation-lookup case that a coding harness actually needs, and
DeepSeek's own note says its web search bills extra tokens for summarisation
anyway. The decision is reversible per-session if it proves wrong, since the
endpoint is already config.

## Execution rules

These hold for every tool and live in Go, not in prompt text.

- Paths resolve against the workspace root and are checked for escape after
  symlink resolution. Escapes are rejected, not sanitised.
- Every tool has a wall-clock timeout and an output byte cap, with truncation
  labelled in the result.
- Tool results are appended in `tool_calls` array order, never in completion
  order. Parallel tool calling is always on and cannot be disabled: the
  Responses API guide states it outright, the Codex model catalogue declares
  `"supports_parallel_tool_calls": true` for both models, and the Anthropic
  table ignores `disable_parallel_tool_use`. Executing concurrently is fine;
  appending out of order is not, because it churns the prefix and costs the
  cache (`DESIGN.md` §3.2).
- Never send `tool_choice`. Thinking mode rejects it (`DESIGN.md` §4.4).

## Permissions

Three modes, session-scoped:

- Read-only. `Read`, `Glob`, `Grep`, `List`, `WebFetch` run freely; everything
  else prompts.
- Default. Reads run freely, `Write` and `Edit` show a diff and prompt, `Bash`
  prompts against a growable allowlist.
- Full access. Nothing prompts.

Modes gate execution, never availability. All ten tools are sent on every
request in every mode, and a call the mode disallows is refused at execution
with an error result the model can read and route around. Removing tools per
mode would give each mode a different prefix and make every mode switch a cold
cache ([CACHE.md](CACHE.md)).

Approval is an event in the log and the agent loop blocks on it, so an approval
survives a page reload the same way the rest of the run does.

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
