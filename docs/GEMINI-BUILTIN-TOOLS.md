# Gemini's built-in tools: should this harness use any of them?

## 1. What this is

[GEMINI-INTEGRATION.md](GEMINI-INTEGRATION.md) §8 put Gemini's built-in tools
out of scope with one line: "Interesting, orthogonal, and each one is its own
argument about what a coding session should be allowed to reach." This is that
argument, made once, so the question is decided rather than re-opened from
scratch each time someone reads the tool list.

**Recommendation up front: adopt `google_search`, and only `google_search`,
behind an off-by-default setting — and only after the four costs in §5 are
accepted, because two of them are not obvious and one of them is a terms-of-
service question rather than an engineering one. Everything else is either
already covered by a tool the harness gates, or executes somewhere that makes
it useless here.**

Nothing in this document is implemented. Sources are `third_party/gemini-docs/`
and `openapi.json` within it, vendored 2026-08-21.

## 2. The nine tools, and the one fact that decides most of it

`openapi.json`'s `Tool` schema has nine variants. What separates them is not
capability but **where they execute**
(`third_party/gemini-docs/interactions/tool-combination.md`, "Supported
tools"):

| Tool | Executes | Config | Already covered by |
| --- | --- | --- | --- |
| `google_search` | Server-side | `search_types`: `web_search`, `image_search`, `enterprise_web_search` | **nothing** |
| `url_context` | Server-side | none | `WebFetch` |
| `code_execution` | Server-side | none | `Bash` |
| `file_search` | Server-side | `file_search_store_names`, `metadata_filter`, `top_k` | — |
| `retrieval` | Server-side | `rag_store_config`, `exa_ai_search_config`, … | — |
| `google_maps` | Server-side | `latitude`, `longitude`, `enable_widget` | — |
| `mcp_server` | Server-side | `name`, `url`, `headers`, `allowed_tools` | `internal/mcpclient` |
| `computer_use` | **Client-side** | `environment`, safety policy fields | `.claude/skills/playwright-cli` |
| `function` | **Client-side** | the harness's own tools | — |

"Server-side" means Google runs it. The harness never issues the call, never
sees it before it happens, and cannot refuse it.

## 3. The permission gate is the real question

The harness has one rule about tool execution, and `docs/MCP.md` states it for
the case that most resembles this one:

> A call goes out through the same permission gate as everything else. MCP
> tools reach outside the workspace by definition, so `readonly` sessions do
> not get to call them unless the server itself is marked `allow_readonly`.
> The decision is made in Go at the moment of the call, from a map frozen on
> the policy at run start — never from a live read of the table.

A server-side built-in tool cannot honour that. `readonly` permission mode
means "Read, Glob, Grep, List, WebFetch only", but a Gemini session with
`url_context` enabled reaches arbitrary URLs regardless of mode, and no
`tool_call` event ever reaches `internal/tools` for the policy to judge.

This is not total blindness. The steps come back in the response — "these
intermediate tool steps are now visible and returned to you, they are part of
the conversation history" — so a `google_search_call` step and its queries are
observable, loggable, and would appear in the trace. But observation after the
fact is not a gate. The harness would be reporting what Google already did.

**So adopting any server-side tool means writing down an explicit exception to
the rule, not quietly discovering one later.** That is the decision this
document exists to force, and it is why the recommendation is one tool rather
than a category.

Two things make the exception narrower than it first sounds:

- **It is per-tool and per-request.** The harness builds the `tools` array; a
  built-in tool the array omits cannot be called. So the gate moves from
  per-call to per-session, decided when the request head is frozen. That is a
  weaker guarantee than the existing one, but it is a real one, and it is the
  same shape as the MCP `allow_readonly` flag — a decision frozen at run start.
- **`readonly` can still exclude it.** Nothing forces a `readonly` session's
  array to carry `google_search`. Gating the tool on permission mode at
  array-construction time restores most of the property, one level up.

## 4. Tool by tool

**`google_search` — the only one with a real case.** The harness has `WebFetch`
but no search. A session that knows a URL can read it; a session that does not
know the URL has nothing. That is a genuine gap for coding work — API changes,
error messages, library versions. `search_types` also offers `image_search`,
which returns image bytes and would land in a session that can now see images.

**`url_context` — no.** It fetches URLs, which `WebFetch` already does through
the permission gate and the output cap. Adopting it adds an ungated egress path
in exchange for nothing the harness cannot already do.

**`code_execution` — no, and it would actively mislead.** It runs in Google's
sandbox. The harness's entire model is a workspace with repositories cloned
into it at a path that is identical on both sides of the container mount
(`docs/WORKTREES.md`, "Path parity"). Code executing on Google's infrastructure
cannot see those repositories, cannot edit a file, and produces nothing that
reaches the deliverable. Sitting in the array beside `Bash`, it is a trap: two
tools that look like "run code", one of which silently cannot do the job.

**`computer_use` — no, but for a different reason.** It is *client-side*, so it
would go through the permission gate normally — the architectural objection
does not apply. The objection is duplication and size: browser automation
already exists as the `playwright-cli` skill, and adopting `computer_use` means
implementing a driver for Google's predefined function set, in a session that
already has a working answer.

**`file_search` and `retrieval` — no.** RAG over uploaded stores. A session's
context is its workspace, its repositories' `CLAUDE.md` files, and its skills
catalogue. There is no corpus to point these at.

**`mcp_server` — no, and it would move MCP out of the harness's hands.** Google
would call the MCP server directly. `internal/mcpclient` exists precisely so
the harness holds the connection, the frozen tool snapshot, the per-server
`allow_readonly` flag and the call gate (`docs/MCP.md`). Handing that to the
provider gives up all four. `interactions/function-calling.md` also records
that remote MCP does not support Gemini 3 yet, so the question is moot today.

**`google_maps` — no.** No coding use.

## 5. Four costs, two of them not obvious

**Enabling any built-in tool alongside our own forces `validated` tool
choice.** From `tool-combination.md`, "Limitations": "Default to `validated`
mode (`auto` mode is not supported) when tool context circulation is enabled."
The harness sends no `tool_choice` today and relies on `auto` being the
default. This would put a value in `generation_config` on every Gemini
request — a change to the frozen request head, in a mode nothing has measured.
The whole combination feature is also **Preview and Gemini-3-only**.

**Built-in tool steps carry their own `id` and `signature`, and stateless mode
must replay both.** From `tool-combination.md`: "you must ensure that you pass
both the `id` and the `signature` fields back to the model in subsequent
requests to validate authenticity and maintain context." The fold handles
`thought` steps only (`GEMINI-INTEGRATION.md` §5.2, and it is true today
precisely because signatures "never appear on … standard function calls").
Built-in tools are the exception that reopens it — `internal/fold`,
`ReasoningDeltaPayload` and `wire.Message` would all need to carry a second
kind of signature, keyed per step rather than per sub-turn.

**Search bills per query, outside the cost model.** $14 per 1,000 requests
after 5,000 free per month, shared across all Gemini 3.x models
(`third_party/gemini-docs/pricing.md`). `pricing.Table.Cost` is a three-rate
per-token model; this is a fourth axis, the same shape of hole as the cache
storage charge `gemini.Usage.TokenSplit`'s comment already records. The data is
available — `usage.grounding_tool_count` reports `{type, count}` per grounding
tool — so it is fixable, but **every cost figure in the UI under-reports until
it is**, and the settings registry already warns what silent zero-cost does to
a spend figure. Note Search is exempt from the token double-charge that the
other built-in tools incur on `prompt_token_count`, because it prices at the
query level instead.

**Grounded output has terms-of-service display requirements.** A
`google_search_result` step carries `search_suggestions`, "an HTML snippet for
rendering search suggestions in your UI. Full usage requirements are detailed
in the Terms of Service"
(`third_party/gemini-docs/interactions/google-search.md`), and the answer text
carries inline `url_citation` annotations binding spans of the response to
source URLs. This harness has a UI — the transcript viewer — so "we are
headless" is not a clean answer. **This is a question for a person, not an
engineering trade-off**, and it should be read directly at
<https://ai.google.dev/gemini-api/terms#grounding-with-google-search> before
anything ships. Mechanically it also means `wire.Content` would need to model
`annotations`, which it does not today.

## 6. The cost nobody asks about: provider divergence

The tool array already differs by provider — DeepSeek gets one array, the
vision-capable providers another (`internal/tools.DefinitionsForProvider`). So
divergence is not new. What is new is divergence in *what the session can
reach*, rather than in how it is told to reach it.

`internal/evals` compares two arms of the same suite. If a Gemini arm can
search the web and a DeepSeek arm cannot, "the same task on two models" is no
longer the same task, and a score difference has an extra explanation nobody
controlled for. That does not block adoption, but an eval run spanning
providers should either disable the tool or record that it was on.

## 7. If it is built

Sketch only; this is not a phased plan.

1. **A settings key, off by default.** `tools.gemini_google_search` or similar,
   with a description that names the permission-gate exception explicitly
   rather than burying it.
2. **Excluded from `readonly` at array-construction time**, restoring the gate
   at session granularity (§3).
3. **`grounding_tool_count` into the cost model in the same change**, not
   after. An unmetered tool is how spend silently vanishes.
4. **`validated` tool choice measured before it is sent**, live, the way Phase 2
   measured everything else — including what it does to ordinary tool-calling,
   since it applies to the whole request and not just the built-in tool.
5. **The ToS question answered by a person**, and the answer recorded here.
6. **Built-in step signatures through the fold**, with a resume test, since
   this reopens what §5.2 closed.

Steps 4 and 5 gate the rest. Neither is expensive; both are the kind of thing
that is much cheaper before the code than after.

## 8. What this rules out

`url_context`, `code_execution`, `file_search`, `retrieval`, `google_maps`,
`mcp_server` and `computer_use` are assessed and declined above, each for a
stated reason. If one is revisited, the reason to revisit it should be that its
reason here has changed — `computer_use` if the playwright skill proves
inadequate, `mcp_server` if Google's remote MCP gains Gemini 3 support *and*
the harness stops needing to gate MCP calls, which is unlikely.
