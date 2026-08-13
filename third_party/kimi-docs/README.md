# Kimi API documentation (vendored)

Local mirror of <https://platform.kimi.ai/docs>, the Kimi API Platform docs from
Moonshot AI.

- Fetched: 2026-08-13
- Pages: 74
- Each file carries `source:` frontmatter pointing at the page it came from.

The site publishes a Markdown rendering of every page at `<url>.md`, so these
files are the vendor's own Markdown rather than a conversion of rendered HTML.
Internal links are rewritten to relative `.md` paths; links off-site stay
absolute.

`sitemap.xml` and `llms.txt` both list the same 72 pages, and both omit
[Tool Use](api/tool-use.md) and [Partial Mode](api/partial.md). Those two were
found by following links out of the listed pages. A refresh should crawl links
to closure rather than trusting either index.

Moonshot publishes an OpenAPI 3.1.0 description of the API, kept here as
[`openapi.json`](openapi.json): 10 paths and 25 schemas, served from
`https://api.moonshot.ai`. The `api/` pages embed the same operations as inline
YAML.

Pages are MDX, and the components are left as they arrive. Ten pages render
their tables through a `DocTable` element, so prices and limits appear in a JSX
`rows` array rather than a Markdown table — the values are all there, in
`pricing/` and `api/models-overview.md`. `<Note>` and `<Warning>` blocks are
likewise kept verbatim.

The Chinese site, <https://platform.kimi.com/docs>, carries five pages absent
from the English one: two legal agreements and three research changelogs
(`changelog/changelog/{changelog,moba,moonlight}`). They are not mirrored here.

## Top level

- [Quickstart](overview.md)
- [Main Concepts](introduction.md)
- [Model List](models.md)
- [Platform Changelog](platform-changelog.md)

## Models

- [Kimi K3](guide/kimi-k3-quickstart.md)
- [Kimi K2.7 Code](guide/kimi-k2-7-code-quickstart.md)
- [Kimi K2.6](guide/kimi-k2-6-quickstart.md)

## Guides

Request shape and generation:

- [Thinking Models](guide/use-thinking-models.md)
- [Reasoning Effort](guide/use-reasoning-effort.md)
- [Set Parameters for Multi-turn Chat](guide/engage-in-multi-turn-conversations-using-kimi-api.md)
- [Streaming Output](guide/utilize-the-streaming-output-feature-of-kimi-api.md)
- [Automatic Reconnection on Disconnect](guide/auto-reconnect.md)
- [Context Caching](guide/use-context-caching-feature-of-kimi-api.md)
- [JSON Mode](guide/use-json-mode-feature-of-kimi-api.md)
- [response_format and Structured Output](guide/response_format.md)
- [Partial Mode](guide/use-partial-mode-feature-of-kimi-api.md)
- [Vision Models](guide/use-kimi-vision-model.md)

Tools and agents:

- [Tool Calls](guide/use-kimi-api-to-complete-tool-calls.md)
- [Tool Choice](guide/use-tool-choice.md)
- [Dynamically Loaded Tools](guide/use-dynamic-tool-loading.md)
- [K3 Tool Calling Best Practices](guide/kimi-k3-tool-calling-best-practice.md)
- [Official Tools](guide/use-official-tools.md)
- [Internet Search](guide/use-web-search.md)
- [Build an Agent with Kimi K3](guide/use-kimi-k3-to-setup-agent.md)
- [File-Based Q&A](guide/use-kimi-api-for-file-based-qa.md)

Batch:

- [Batch API](guide/use-batch-api.md)
- [Batch Inference in the Console](guide/use-batch-inference.md)

Integrations and tooling:

- [Claude Code](guide/claude-code-kimi.md)
- [Codex CLI](guide/codex-kimi.md)
- [Kimi Code CLI](guide/kimi-code-cli.md)
- [OpenCode](guide/open-code.md)
- [OpenClaw](guide/use-kimi-in-openclaw.md)
- [Hermes Agent](guide/use-kimi-in-hermes-agent.md)
- [ModelScope MCP Server](guide/configure-the-modelscope-mcp-server.md)
- [MoonPalace debugging tool](guide/use-moonpalace.md)
- [Playground](guide/use-playground-to-debug-the-model.md)

Practice and operations:

- [Prompt Best Practices](guide/prompt-best-practice.md)
- [Benchmarking Best Practices](guide/benchmark-best-practice.md)
- [Troubleshooting](guide/troubleshooting.md)
- [Organization Setup and Verification](guide/org-best-practice.md)
- [Account and Billing](guide/account-and-payments.md)
- [Email and Google Sign-In](guide/account-security-and-sign-in.md)
- [Compare with Other Kimi Products](guide/product-plans.md)

## API reference

- [API Overview](api/overview.md)
- [Model Parameter Reference](api/models-overview.md)
- [Create Chat Completion](api/chat.md)
- [Tool Use](api/tool-use.md)
- [Partial Mode](api/partial.md)
- [List Models](api/list-models.md)
- [Estimate Tokens](api/estimate.md)
- [Check Balance](api/balance.md)
- [Common Error Codes](api/errors.md)
- [Join the Developer Community](api/join-the-community.md)

Files:

- [Files](api/files.md)
- [Upload File](api/files-upload.md)
- [List Files](api/files-list.md)
- [Get File Information](api/files-retrieve.md)
- [Get File Content](api/files-content.md)
- [Delete File](api/files-delete.md)

Batch:

- [Create Batch](api/batch-create.md)
- [List Batches](api/batch-list.md)
- [Retrieve Batch](api/batch-retrieve.md)
- [Cancel Batch](api/batch-cancel.md)

## Pricing

- [Model Inference Pricing Explanation](pricing/chat.md)
- [Kimi K3](pricing/chat-k3.md)
- [Kimi K2.7 Code](pricing/chat-k27-code.md)
- [Kimi K2.6](pricing/chat-k26.md)
- [Kimi K2.5](pricing/chat-k25.md)
- [Moonshot V1](pricing/chat-v1.md)
- [BatchJob](pricing/batch.md)
- [WebSearch](pricing/tools.md)
- [Recharge and Rate Limiting](pricing/limits.md)

## Agreements

- [Terms of Service](agreement/modeluse.md)
- [Privacy Policy](agreement/userprivacy.md)
