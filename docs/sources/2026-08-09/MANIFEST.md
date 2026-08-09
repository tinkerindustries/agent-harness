# Source snapshot — 2026-08-09

Pinned copies of the DeepSeek documentation pages the design decisions rest on,
taken from `third_party/deepseek-docs/` on 2026-08-09.

The mirror is generated and meant to be re-scraped, so a refresh replaces the
exact text cited in [VALIDATION.md](../../VALIDATION.md). This directory is not
generated. Do not refresh it — copy to a new dated directory instead and diff
the two.

Several facts recorded here were flagged at capture time as due to change within
days: pro's effort mapping, the pro model release, and pricing. Those are the
ones the diff will be worth reading.

Checksums are the first 16 hex characters of SHA-256. Regenerate with:

```sh
find . -name '*.md' ! -name MANIFEST.md | sed 's|^\./||' | sort | \
  while read -r f; do printf "%s  %s\n" "$(shasum -a 256 "$f" | cut -c1-16)" "$f"; done
```

## What each page establishes

| Checksum | Page | Load-bearing for |
| --- | --- | --- |
| `436fc82be398e975` | `guides/kv_cache.md` | The whole of CACHE.md. Prefix units, full-match rule, Example 2, persistence triggers |
| `667f7097eb73cdba` | `guides/thinking_mode.md` | reasoning_content round-trip (§3.1), effort mapping table, temp/top_p ignored |
| `1c38d4b7f92c8fb0` | `quick_start/pricing.md` | Cache 50×/120× ratios, 1M context, 384K max output, concurrency, the pricing-rise warning |
| `cb17a283fe351295` | `api/create-chat-completion.md` | Request and response schema, streaming delta shape, `stream_options`, tool limits, effort values |
| `17c6ff866f251933` | `quick_start/agent_integrations/oh_my_pi.md` | The three undocumented 400-causers: tool_choice rejected, non-null tool-call content, reasoning replay. Also `max_tokens` vs `max_completion_tokens`, `developer` role rejected |
| `0e4191e269641615` | `quick_start/agent_integrations/claude_code.md` | DeepSeek's own recommended settings: pro, effort max, flash subagents, 768K compaction. Server-side web search |
| `82aac91876f1bebf` | `updates.md` | Flash-0731 agent benchmarks beating V4-Pro-Preview, pro unchanged, benchmark sampling values, "DeepSeek Harness" mention |
| `2b62a09d487cde61` | `guides/responses_api.md` | Parallel tool calls always enabled, `apply_patch` custom tool, incremental function-call argument streaming |
| `a1324d1e3003e302` | `guides/anthropic_api.md` | What the shim ignores, web search availability, no image/document support |
| `1056f77bdff4e509` | `guides/tool_calls.md` | Strict mode schema subset and its Beta status |
| `78de6d13c0f489ba` | `quick_start/rate_limit.md` | Concurrency limits, 10-minute hold, keep-alive behaviour, user_id KVCache isolation |
| `fb98038016b4d011` | `quick_start/agent_integrations/codex.md` | Model catalogue: `supports_parallel_tool_calls`, 1M context, text-only input, declared effort levels |
| `61bb58c57045a45b` | `quick_start/agent_integrations/copilot_cli.md` | The exact 400 text for missing reasoning_content |
| `1733f446fc5a5d28` | `news/news260424.md` | V4 agent optimisation naming Claude Code, OpenClaw, OpenCode — the basis for the tool vocabulary inference |
| `12d8b59f78ee2781` | `quick_start/error_codes.md` | Retry classification |
| `704474fd1620bc49` | `api/list-models.md` | Model list endpoint |
| `c00fbcfe2c5ef26b` | `api/get-user-balance.md` | Balance endpoint and 402 handling |
| `c549b450cf3d5e9a` | `guides/multi_round_chat.md` | Stateless API, full history concatenation |
| `4ef585d0c2871a51` | `quick_start/agent_integrations/deepcode.md` | A third effort recommendation, plain base URL |
| `fe6a6574e1d02834` | `guides/coding_agents.md` | Integration targets |
| `76d09a004fa8f6e5` | `guides/json_mode.md` | JSON output caveats |
| `5972125a8a4b4f31` | `guides/fim_completion.md` | FIM limits, deferred feature |
| `097c3650a2a7dded` | `guides/chat_prefix_completion.md` | Prefix completion, deferred feature |
| `c753c8f623cc8f84` | `quick_start/token_usage.md` | Offline tokenizer availability |
| `593f933f0c71cd8c` | `api/deepseek-api.md` | Bearer auth |
| `8f8fe862905680af` | `README.md` | Mirror provenance and method |

## Not captured

The other 37 pages of the mirror, and `_img/`. Nothing in the design cites them.
The full mirror stays at `third_party/deepseek-docs/`.

`faq.md` carries no content upstream — it redirects to a separate app that the
mirror does not cover.
