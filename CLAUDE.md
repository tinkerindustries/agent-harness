# deepseek-harness

A harness for running DeepSeek specifically. Target DeepSeek's own API rather
than a provider-agnostic abstraction; where a choice arises, prefer the option
that exercises DeepSeek's behaviour directly.

## Shape

Work arrives on a NATS JetStream queue, runs as one of several concurrent agent
sessions in a single Go process, and returns a result to a results stream. The
web UI is read-only: `GET` and `HEAD` only, and no endpoint starts or steers a
run. `docs/DESIGN.md` is the reference; read it before changing the request
path, the tool array, or the system prompt, all of which are cache-critical.

## Vendored documentation

`vendor/docs/deepseek/` mirrors <https://api-docs.deepseek.com/> as Markdown.
Start at `vendor/docs/deepseek/README.md` for the index. Consult it before
answering questions about DeepSeek's API surface.

The mirror is generated, not authored — do not hand-edit the files. To refresh,
re-scrape the site; response schemas under `api/` render client-side and need a
headless browser to capture.

Files with an `.en.` in the name are our English translations rather than
upstream content. DeepSeek's prompt library is published in Chinese only;
`_data/prompts.en.json` mirrors its structure and works as a drop-in substitute.

## API facts

- Base URL, OpenAI format: `https://api.deepseek.com`
- Base URL, Anthropic format: `https://api.deepseek.com/anthropic`
- Models: `deepseek-v4-flash` and `deepseek-v4-pro`. Both default to thinking
  mode and support non-thinking mode.
- The Responses API supports `deepseek-v4-flash` only.

Pricing, rate limits, and context/output limits change; read
`vendor/docs/deepseek/quick_start/pricing.md` rather than quoting numbers from
memory.
