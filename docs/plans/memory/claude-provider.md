# Claude provider and claude-session — feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

| Where | Addition | Meaning |
|---|---|---|
| `wire.EventType` | a raw-block event, beside `EventThoughtSignatureDelta` | One complete value per response: the assistant `content` array exactly as Anthropic returned it |
| `wire.Event` | `ProviderBlocks json.RawMessage` | The payload of that event |
| `wire.Item` | `ProviderBlocks json.RawMessage` tagged `json:"-"` | Carried on the sub-turn's reasoning item, the way `ThoughtSignature` is. Never serialised into another provider's body |
| `store.ReasoningDeltaPayload` | `provider_blocks`, omitempty | Persists the blocks with the sub-turn. The sub-turn commits when reasoning, signature or blocks is non-empty |
| `wire.Usage` | a cache-write token count, omitempty | Anthropic's `cache_creation_input_tokens` |
| `pricing.ModelPrices` | `input_cache_write_per_million_usd`, optional | Zero means cache writes are priced at the miss rate |
| `provider.Name` | `Anthropic` | Serves `claude-opus-5`, `claude-sonnet-5`, `claude-fable-5-1` |

Only `internal/anthropic` reads `ProviderBlocks`. When an assistant turn has them, the client
renders that turn from them verbatim and does not rebuild it from the text and call items.

## Invariants

- The request head (system prompt, then tool array) is frozen for a session's life. Adding the web tools must be deterministic: same position, same bytes, every request.
- DeepSeek, Gemini and Kimi request bodies, `TestPromptGolden` and `TestGoldenFrames` never change. Re-recording a golden needs the user's say-so.
- Nothing but protocol frames reaches stdout. `ANTHROPIC_API_KEY` is never written to the settings table, and it is stripped from `Bash` and MCP children.
- The loop only ever appends. Compaction starts a new session. Anthropic's preserved-thinking check depends on this, and the client sends `prefix_mismatch_behavior: "error"`.
- Thinking, redacted thinking and server-tool result blocks go back byte-for-byte, in their original order.
- Each vendor vocabulary hosts only that vendor's models. `stdio-session` hosts everything.

## Files that matter

| Path | Why an agent needs it |
|---|---|
| `internal/session/client.go` | The `Client` seam the new client implements |
| `internal/gemini/` | The closest existing provider: its own SSE vocabulary, opaque replay, hand-rolled structs |
| `internal/providerhttp/transport.go` | Retry with backoff and `SetAuth`, the transport to reuse |
| `internal/session/turn.go`, `internal/fold/fold.go` | Where a sub-turn's reasoning and signature are captured and replayed. Raw blocks follow the same path |
| `internal/stdiosession/dialect.go`, `translator.go`, `interactions*.go` | The dialect seam and the last dialect added through it |
| `cmd/harness/stdiosession.go` | Keys, client dispatch, hosted models, dialect choice |
| `internal/stdiosession/modelinfo.go` | The one place the protocol's model vocabulary meets a provider's |
| `docs/STDIO-INTERACTIONS.md` | The model for `docs/STDIO-MANAGED-AGENTS.md` |

## External facts

Read from Anthropic's docs as bundled with Claude Code 2.1.274 (model table cached 2026-06-24). Check a live page before building on any figure.

| Fact | Source |
|---|---|
| `POST /v1/messages`. Headers `x-api-key`, `anthropic-version: 2023-06-01`, `content-type: application/json`. Beta features go in a comma-separated `anthropic-beta` header | <https://platform.claude.com/docs/en/api/messages> |
| All three models: 1M context, 128K max output. Adaptive thinking only, and `budget_tokens`, `temperature`, `top_p`, `top_k` and assistant prefill all return 400. Fable 5.1 400s on an explicit `thinking: disabled` and on forced `tool_choice` (`any`/`tool`) | <https://platform.claude.com/docs/en/about-claude/models/overview.md> |
| `output_config.effort` takes `low`, `medium`, `high`, `xhigh`, `max`. The default is `high`. Opus 5 accepts `thinking: disabled` only at `high` or below. Lower effort in preference to disabling thinking | <https://platform.claude.com/docs/en/build-with-claude/effort.md> |
| `thinking.display` defaults to `"omitted"` on these models, which sends thinking blocks with empty text. `"summarized"` returns readable summaries. Replay blocks unchanged on the same model | <https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking.md> |
| Preserved thinking. A signature binds its conversation prefix (system, tools, earlier messages). Editing history invalidates later blocks. `thinking.block_binding.prefix_mismatch_behavior` (`error`/`drop_block`) needs beta `thinking-binding-controls-2026-08-01`. Enforced on Fable 5.1 for orgs created on or after 2026-08-31 | <https://platform.claude.com/docs/en/build-with-claude/extended-thinking.md> |
| SSE events: `message_start`, `content_block_start`, `content_block_delta` (`text_delta`, `thinking_delta`, `signature_delta`, `input_json_delta`), `content_block_stop`, `message_delta` (stop reason, usage), `message_stop`, `ping`, `error` | <https://platform.claude.com/docs/en/build-with-claude/streaming.md> |
| Caching. Render order is tools → system → messages, with at most 4 breakpoints. A top-level `cache_control: {type: "ephemeral"}` auto-places one. Minimum cacheable prefix is model-dependent (512–4096 tokens). Usage reports `input_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens` | <https://platform.claude.com/docs/en/build-with-claude/prompt-caching.md> |
| Server tools `web_search_20260209` and `web_fetch_20260209` need no beta header. Their results arrive as `server_tool_use` plus `*_tool_result` blocks, which must be replayed. `stop_reason: "pause_turn"` means resend the same history with the assistant content and no new user turn | <https://platform.claude.com/docs/en/agents-and-tools/tool-use/overview.md> |
| Mid-conversation `role: "system"` messages work on Opus 5 and Fable 5.1, not Sonnet 5. They must follow a user message | <https://platform.claude.com/docs/en/build-with-claude/prompt-caching.md> |
| Status 529 is `overloaded_error` and is retryable, like 429 and 5xx. `stop_reason: "refusal"` arrives as HTTP 200 with `stop_details` | <https://platform.claude.com/docs/en/api/errors.md> |
| Pricing (verify live). Opus 5 $5/$25, Sonnet 5 $2/$10, Fable 5.1 $10/$50 per MTok in/out | <https://platform.claude.com/docs/en/pricing.md> |
| Managed Agents: sessions (`POST /v1/sessions` with `initial_events`, get, delete), events (`POST /v1/sessions/{id}/events`: `user.message`, `user.interrupt`, `user.custom_tool_result`), and the SSE event types | <https://platform.claude.com/docs/en/managed-agents/sessions.md>, <https://platform.claude.com/docs/en/managed-agents/events-and-streaming.md> |

## Live checks

The user's Anthropic key is in `~/.config/agent-harness/anthropic.env`, as one
`ANTHROPIC_API_KEY=` line. Pass that path to `-env`, or source it for a hand-driven client
test. Never copy the key into the repository, a report, a commit or a log.

## Vocabulary

- **Raw blocks**: one response's assistant `content` array as Anthropic sent it. The replay unit.
- **Managed Agents vocabulary**: the session and event shapes `claude-session` puts on the pipe. None of Anthropic's hosted agent runtime is involved.
