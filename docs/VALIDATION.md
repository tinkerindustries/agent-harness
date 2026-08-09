# Validation against the vendored docs

Every decision in DESIGN.md, TOOLS.md, and MODELS.md checked against
`vendor/docs/deepseek/`. Mirror fetched 2026-08-09.

The 26 pages cited below are pinned at
[`sources/2026-08-09/`](sources/2026-08-09/MANIFEST.md), because the mirror is
regenerated on refresh and several of these facts were already marked as due to
change. Cite the snapshot when the wording matters.

Three categories: confirmed, corrected, and unvalidated. The last one matters
most — it lists what we are relying on that the local docs do not establish.

## Confirmed

| Claim | Source |
| --- | --- |
| Anthropic endpoint ignores `cache_control`, `anthropic-beta`, `anthropic-version`, `top_k`, `thinking.budget_tokens` | `guides/anthropic_api.md` |
| Anthropic endpoint does not support image, document, or `redacted_thinking` blocks | `guides/anthropic_api.md` |
| Web search is server-side and Anthropic-format only; Chat Completions accepts `type: "function"` and nothing else | `guides/anthropic_api.md`, `agent_integrations/claude_code.md`, `api/create-chat-completion.md` |
| `reasoning_content` must round-trip when `tools` is present, or the API returns 400 | `guides/thinking_mode.md` |
| Cache-hit against cache-miss input pricing: 50× on flash, 120× on pro | `quick_start/pricing.md`, arithmetic |
| Cache prefix units persist at end of user input, end of model output, and fixed intervals | `guides/kv_cache.md` |
| Cache construction takes seconds and is best-effort | `guides/kv_cache.md` |
| 768K compaction window | `agent_integrations/claude_code.md` (`786432` = 768 × 1024) |
| Up to 10 minutes before inference starts; keep-alive comments when streaming, blank lines when not | `quick_start/rate_limit.md` |
| `stream_options.include_usage`, `data: [DONE]` terminator | `api/create-chat-completion.md` |
| Streaming delta schema documents only `content`, `reasoning_content`, `role` | `api/create-chat-completion.md` |
| Concurrency 500 pro / 2500 flash, account-wide regardless of key | `quick_start/rate_limit.md`, `quick_start/pricing.md` |
| Error code set and meanings | `quick_start/error_codes.md` |
| Pricing is about to rise significantly | `quick_start/pricing.md` footnote 2 |
| V4 carries dedicated agent optimisations, naming Claude Code, OpenClaw, OpenCode | `news/news260424.md` |
| Function names match `[a-zA-Z0-9_-]{1,64}`, max 128 per request | `api/create-chat-completion.md` |
| Strict mode schema subset, Beta, requires `/beta` | `guides/tool_calls.md` |
| Effort mapping is not identity | `guides/thinking_mode.md`, consistent with `api/create-chat-completion.md` |
| `temperature` and `top_p` have no effect in thinking mode | `guides/thinking_mode.md`, `guides/responses_api.md` |
| `GET /models` and `GET /user/balance` shapes | `api/list-models.md`, `api/get-user-balance.md` |
| Stateless API; the client concatenates the full history every request | `guides/multi_round_chat.md` |
| 1M context on both models | `quick_start/pricing.md`, `agent_integrations/codex.md` (`1048576`) |

One inference got promoted to confirmed. Parallel tool calls are always enabled
and cannot be disabled — stated outright in `guides/responses_api.md`
("`parallel_tool_calls` | Ignored (parallel tool calling is always enabled)")
and again as `"supports_parallel_tool_calls": true` for both models in the Codex
`models.json`. TOOLS.md previously derived this from the Anthropic table alone.

## Corrected

### Base URL defaults to plain, not `/beta`

DESIGN.md §2 defaulted to `https://api.deepseek.com/beta` so strict tool mode
would be available. TOOLS.md then decided against strict mode by default. The
two documents contradicted each other and the `/beta` default lost its reason.

Every integration configuration DeepSeek publishes uses plain
`https://api.deepseek.com`. Default there and reach for `/beta` only when a beta
feature is actually in use.

`/v1` is accepted as a path prefix — `agent_integrations/workbuddy.md` and
`nanobot.md` both use it — though `oh_my_pi.md` advises against it and the API
reference documents the bare path. Send the bare path.

### Assistant messages carrying tool calls need non-null content

`agent_integrations/oh_my_pi.md` lists `requiresAssistantContentForToolCalls`
among three fields that are "essential — without them, DeepSeek V4 will return
400 errors when using tools in thinking mode." Its note: "Ensures tool-call
messages have non-null `content`."

Nothing in the API reference says this. The reference types assistant `content`
as nullable. The thinking-mode sample output shows `content=''` on a tool-call
turn, which is an empty string rather than null.

The fold from event log to `messages` emits `""` for a tool-call assistant
message with no text, never `null`.

### `tool_choice` is rejected in thinking mode

Same source: `supportsToolChoice: false`, annotated "DeepSeek V4 thinking mode
rejects the `tool_choice` parameter."

Measurement on 2026-08-09 shows that claim is too broad. `auto` and `none` are
both accepted; `required` and named-tool forcing return 400 `Thinking mode does
not support this tool_choice`. Details in [OBSERVED.md](OBSERVED.md).

Never sending `tool_choice` remains the rule, since `auto` is the default when
tools are present. The finding that matters is the capability limit: no tool can
be forced while thinking is on.

### Two request-shape details

`developer` is rejected as a role; send `system`. DeepSeek uses `max_tokens`,
not OpenAI's `max_completion_tokens`. Both from `oh_my_pi.md`.

### Set `max_tokens` explicitly

Max output is 384K. No default is documented for V4 anywhere in the mirror.
`api/create-chat-completion.md` points at the pricing page for the default, and
the pricing page gives only the maximum. The one default that does appear —
4096 — is from `news/news0725.md` in 2024, describing the era when 8K was a beta
feature, and is stale.

Send `max_tokens` on every request rather than inheriting an undocumented
default.

### The exact 400 text is known

`agent_integrations/copilot_cli.md` quotes it: `The reasoning_content in the
thinking mode must be passed back to the API.` Useful for error mapping and for
asserting in tests.

That page also notes the Anthropic endpoint sidesteps the whole class of error,
because the shim handles thinking-block replay itself. A fair point in the
shim's favour, and it does not change our choice — we handle the replay
explicitly either way.

### Input is text only

`input_modalities: ["text"]` in the Codex `models.json` for both models. The
Anthropic table marks image and document blocks unsupported. The Responses API
replaces `input_image` parts with placeholder text.

No screenshots, no image paste, no visual diffing. Worth stating in scope rather
than discovering later.

## Contested — the pro default

MODELS.md defaults the main loop to `deepseek-v4-pro`, following DeepSeek's
recommended Claude Code configuration. The change log complicates that.

`updates.md` for 2026-07-31 announces V4-Flash-0731 with "significantly enhanced
agent capabilities, with benchmark results far exceeding V4-Pro-Preview":
Terminal Bench 2.1 at 82.7, NL2Repo 54.2, DeepSWE 54.4, Toolathlon verified
70.3. The same entry says the update "only upgrades the DeepSeek-V4-Flash API.
The DeepSeek-V4-Pro API and the APP/WEB models are unchanged", and that "the
official release of DeepSeek-V4-Pro will follow soon."

Read plainly: as of 2026-07-31, the pro endpoint still served the preview-era
model, and the current flash beat it on agentic coding benchmarks while costing
roughly a third as much with five times the concurrency.

Against that, `quick_start/pricing.md` lists pro's version as "DeepSeek-V4-Pro"
rather than "-Preview", and DeepSeek's Claude Code configuration still names pro
as the main model. Those two are not dated, so we cannot tell whether they
predate the flash update.

This is unresolved from local docs alone. The harness lets you switch, so the
cost of the wrong default is one click. MODELS.md now records the tension and
keeps pro as the default while flagging flash as the benchmark-backed
alternative for agentic coding.

## DeepSeek's own effort advice is inconsistent

- Claude Code: `CLAUDE_CODE_EFFORT_LEVEL=max`
- Codex: `model_reasoning_effort = "high"`
- Deep Code: `reasoningEffort` documented as `"max"` or `"high"`
- Oh My Pi: selector locked to high and xhigh, with xhigh mapped to max
- Their own benchmark runs: max effort

Four harnesses, two answers. Max is the more common recommendation and the one
used for published benchmarks, so it stays our default.

The benchmark note adds something the API docs contradict: the runs used
"topp=0.95, and temperature=1.0" alongside max effort, while
`guides/thinking_mode.md` says both parameters have no effect in thinking mode.
The reading that reconciles them is that those are the fixed internal sampling
values, which is why user-supplied ones are ignored. Nothing actionable, but it
explains the apparent conflict.

## Unvalidated — what we are relying on that the docs do not establish

This section is the point of the exercise.

**The specific tool names.** TOOLS.md builds on Claude Code's vocabulary being
`Read`, `Write`, `Edit`, `Bash`, `Glob`, `Grep`, `TodoWrite`, `Task`,
`WebFetch`. Those names appear nowhere in the vendored docs. What the docs
establish is that DeepSeek optimised V4 for Claude Code, OpenClaw, and OpenCode
— not what those harnesses call their tools. The vocabulary comes from outside
this repo and is the weakest load-bearing assumption in the design.

It is also cheap to be wrong about. Names are a rename away, and the argument
shapes are conventional.

One adjacent fact the docs do supply: DeepSeek accepts
`{"type": "custom", "name": "apply_patch"}` on the Responses API and returns 400
for any other custom tool name. That is direct evidence of tuning against
Codex's patch-application format, and it is the only concrete coding tool name
DeepSeek documents. It is flash-only and Responses-API-only, so it does not
apply to our endpoint, but it is a signal that patch-format editing is
trained-in.

**Line-numbered `Read` output.** Same gap, same source, same cheap fix.

**Effort changes not disturbing the cache.** Not documented. Reasoning from
effort being a request parameter rather than prompt content.

**Incremental tool-call deltas on Chat Completions.** Settled by measurement on
2026-08-09, not by the docs. They do arrive incrementally, in OpenAI's indexed
form. See [OBSERVED.md](OBSERVED.md).

## Two things worth knowing that changed nothing

`updates.md` mentions "the DeepSeek Harness minimal mode (to be released soon)"
as the framework used for their published V4-Flash benchmarks. DeepSeek is
shipping their own harness. Worth watching, given the name of this repository.

`oh_my_pi.md` warns that `reasoning_content` replay behaviour varies across
unofficial providers — DeepInfra, KiloCode, NVIDIA NIM, Zenmux — and advises the
official endpoint. Independent support for targeting `api.deepseek.com`
directly, which CLAUDE.md already requires.
