# DeepSeek API documentation (vendored)

Local mirror of <https://api-docs.deepseek.com/>, converted to Markdown.

- Fetched: 2026-08-27
- Pages: 71 (every URL in the site's `sitemap.xml`)
- Each file carries `source:` frontmatter pointing at the page it came from.

On the 2026-08-27 refresh every page was re-fetched and re-converted by script.
The converter was checked by re-converting pages that had not changed upstream
and requiring byte-identical output against the copy already here, so a diff on
any page is an upstream edit rather than a conversion artefact. Fifteen pages
changed and eight are new; the rest moved only their `fetched:` date. That date
is the date of the last comparison, not of the last edit.

The refresh added the vision model, `deepseek-v4-flash-vision-exp`: `guides/vision.md`,
`guides/files_api.md` and the four `api/*-file*.md` endpoints are new, and it also
appears in `quick_start/pricing.md`, `quick_start/rate_limit.md`, `index.md`, and
the request schemas of `api/create-chat-completion.md` and `api/create-response.md`.

DeepSeek publishes no OpenAPI spec and no docs source repository, so this is
converted from the rendered site. Internal links are rewritten to relative
`.md` paths; links off-site stay absolute.

The schemas under `api/` are served in the HTML, inside collapsible widgets whose
markup does not flatten into readable Markdown. They are serialised into a nested
bullet list — `` `field` (type) **required** — description ``, with `oneOf` variants
as their own branch. The serialiser reproduces `api/get-user-balance.md` and
`api/list-models.md` byte for byte, which is how it was checked; on the
2026-08-27 refresh it replaced the flattened widget text that
`api/create-chat-completion.md`, `api/create-completion.md` and
`api/create-response.md` had been carrying.

`faq.md` carries no content of its own — it redirects to a separate app on
`static.deepseek.com`, which is not mirrored here. It is hand-written and is not
re-converted on a refresh. The image token calculator at the foot of
`quick_start/token_usage.md` is interactive and likewise has no static content.

The prompt library renders from `/data/prompts.json` rather than from page
markup. All 13 prompts are captured in `prompt-library.md`, with the raw JSON
kept at `_data/prompts.json`. They are published in Chinese only; the site
serves the same file on both locales.

Files with an `.en.` in the name are our English translations, not upstream
content: `prompt-library.en.md` and `_data/prompts.en.json`. The translated JSON
keeps the upstream structure, so it is a drop-in substitute.

## Top level

- [Your First API Call](index.md)
- [Change Log](updates.md)
- [FAQ](faq.md)
- [Prompt Library](prompt-library.md)

## Quick start

- [Models & Pricing](quick_start/pricing.md)
- [Rate Limit & Isolation](quick_start/rate_limit.md)
- [Error Codes](quick_start/error_codes.md)
- [Token & Token Usage](quick_start/token_usage.md)

## Guides

- [Thinking Mode](guides/thinking_mode.md)
- [Tool Calls](guides/tool_calls.md)
- [Multi-round Conversation](guides/multi_round_chat.md)
- [Context Caching](guides/kv_cache.md)
- [JSON Output](guides/json_mode.md)
- [Chat Prefix Completion (Beta)](guides/chat_prefix_completion.md)
- [FIM Completion (Beta)](guides/fim_completion.md)
- [Using the Responses API](guides/responses_api.md)
- [Using the Anthropic API](guides/anthropic_api.md)
- [Vision](guides/vision.md)
- [Files API](guides/files_api.md)
- [Integrate with AI Tools](guides/coding_agents.md)

## API reference

- [DeepSeek API](api/deepseek-api.md)
- [Chat Completions API](api/create-chat-completion.md)
- [Responses API](api/create-response.md)
- [FIM Completion API (Beta)](api/create-completion.md)
- [Lists Models](api/list-models.md)
- [Upload File](api/create-file.md)
- [List Files](api/list-files.md)
- [Retrieve File](api/retrieve-file.md)
- [Delete File](api/delete-file.md)
- [Get User Balance](api/get-user-balance.md)

## API samples

- [chat_curl](api_samples/chat_curl.md)
- [chat_nodejs](api_samples/chat_nodejs.md)
- [chat_python](api_samples/chat_python.md)
- [thinking_mode_api_example_non_streaming](api_samples/thinking_mode_api_example_non_streaming.md)
- [thinking_mode_api_example_streaming](api_samples/thinking_mode_api_example_streaming.md)
- [thinking_mode_api_example_tool_call](api_samples/thinking_mode_api_example_tool_call.md)
- [thinking_mode_api_example_tool_call_output](api_samples/thinking_mode_api_example_tool_call_output.md)

## Agent integrations

- [Claude Code](quick_start/agent_integrations/claude_code.md)
- [Codex](quick_start/agent_integrations/codex.md)
- [GitHub Copilot](quick_start/agent_integrations/github_copilot.md)
- [GitHub Copilot CLI](quick_start/agent_integrations/copilot_cli.md)
- [OpenCode](quick_start/agent_integrations/opencode.md)
- [OpenClaw](quick_start/agent_integrations/openclaw.md)
- [Crush](quick_start/agent_integrations/crush.md)
- [Kilo Code](quick_start/agent_integrations/kilo_code.md)
- [AstrBot](quick_start/agent_integrations/astrbot.md)
- [Deep Code](quick_start/agent_integrations/deepcode.md)
- [Hermes Agent](quick_start/agent_integrations/hermes.md)
- [Langcli](quick_start/agent_integrations/langcli.md)
- [nanobot](quick_start/agent_integrations/nanobot.md)
- [Oh My Pi](quick_start/agent_integrations/oh_my_pi.md)
- [Pi](quick_start/agent_integrations/pi_mono.md)
- [Reasonix](quick_start/agent_integrations/reasonix.md)
- [WorkBuddy/CodeBuddy](quick_start/agent_integrations/workbuddy.md)

## News / release notes

- [DeepSeek-V4-Flash-Vision-Exp Release](news/news260821.md)
- [DeepSeek-V4-Pro GA Release](news/news260813.md)
- [DeepSeek V4 Preview Release](news/news260424.md)
- [DeepSeek-V3.2 Release](news/news251201.md)
- [Introducing DeepSeek-V3.2-Exp](news/news250929.md)
- [DeepSeek-V3.1-Terminus](news/news250922.md)
- [DeepSeek-V3.1 Release](news/news250821.md)
- [DeepSeek-R1-0528 Release](news/news250528.md)
- [DeepSeek-V3-0324 Release](news/news250325.md)
- [Your First API Call](news/news250120.md)
- [Introducing DeepSeek App](news/news250115.md)
- [Introducing DeepSeek-V3](news/news1226.md)
- [DeepSeek V2.5: The Grand Finale](news/news1210.md)
- [DeepSeek-R1-Lite-Preview](news/news1120.md)
- [DeepSeek-V2.5](news/news0905.md)
- [Context Caching on Disk](news/news0802.md)
- [DeepSeek API Upgrade](news/news0725.md)
