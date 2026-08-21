# Gemini API documentation (vendored) — Interactions surface

Local mirror of <https://ai.google.dev/gemini-api/docs/>, the Gemini API docs
from Google, scoped to the **Interactions API** — the surface
`docs/GEMINI-INTEGRATION.md` targets for `gemini-3.7-flash`.

- Fetched: 2026-08-21
- Pages: 19, plus one OpenAPI spec
- Each file carries `source:` frontmatter pointing at the page it came from.

Google serves a raw Markdown rendering of every docs page at `<url>.md.txt`,
so these files are the vendor's own Markdown rather than a conversion of
rendered HTML — same deal as the Kimi mirror. Internal links are rewritten to
relative `.md` paths for pages that are mirrored here; links to anything
outside this mirror (the legacy `generateContent` tree, model family pages,
Google Cloud docs, SDK reference, etc.) are left as absolute
`ai.google.dev` / other URLs. A couple of Google's own links use a path that
404s (`.../interactions/interactions-overview`,
`.../interactions/thinking`, both missing or misplacing the `interactions/`
segment); those were resolved to the correct page rather than mirrored
literally.

**Interactions vs. `generateContent`.** Google now has two generation
surfaces. `generateContent` (`POST /v1beta/models/{model}:generateContent`)
is the original, long-stable API and is what most existing Gemini
documentation and SDK examples still use — it is **not** mirrored here.
The **Interactions API** (`POST /v1beta/interactions`) is the newer,
step-based, tool-and-agent-oriented surface `interactions-overview.md` calls
"Generally Available and recommended for all new projects" as of June 2026.
This mirror covers Interactions only, plus the two pages (`models.md`,
`pricing.md`) that aren't surface-specific.

Confusingly, ten of the nineteen pages here still carry Google's boilerplate
"This version of the page covers the new Interactions API, which is currently
in Beta" note, while `interactions-overview.md` itself says GA — the two
claims sit side by side on the live site as fetched. Take the GA claim from
the overview page as authoritative; the Beta banner reads like a stale
template fragment that individual guide pages haven't been updated to drop.

`interactions/thought-signatures.md` is a redirect stub — the real content
lives on [`thinking.md`](thinking.md#signatures) under "Thought signatures".
It is kept here (one line) so the URL from
[GEMINI-INTEGRATION.md](../../docs/GEMINI-INTEGRATION.md) resolves to
something, the same way `deepseek-docs/faq.md` records a redirect rather than
silently dropping the page.

Google also publishes an OpenAPI 3.0.3 description of the (beta, `v1beta`)
Interactions surface, kept here as [`openapi.json`](openapi.json): 14 paths
and 181 schemas. There is a separate "stable v1" flavour of the API reference
page (`api/interactions-api-v1`) with a slightly different model list, but it
documents the exact same `v1beta` endpoint under the hood — it reads as a
doc-only variant, not a second wire format, and was not mirrored.

## Interactions API

- [Interactions API overview](interactions-overview.md) — what it is, why it
  exists next to `generateContent`, stateful vs. stateless, data retention.
- [Interactions API reference](api/interactions-api.md) — full resource and
  field reference: `Interaction`, `Step` (all its typed variants), `Tool`,
  `Content`, request/response bodies for create/get/cancel/delete.
- [Function calling](interactions/function-calling.md) — declarations,
  parallel and compositional calling, the `function_call` /
  `function_result` step shapes.
- [Streaming](interactions/streaming.md) — the SSE event vocabulary:
  `interaction.created`, `step.start`, `step.delta`, `step.stop`,
  `interaction.completed`, `error`, `done`, and every delta type.
- [Combine built-in tools and function calling](interactions/tool-combination.md)
  — mixing `google_search`/other built-in tools with custom function tools in
  one interaction.
- [Thinking (and thought signatures)](thinking.md) — thinking levels, thought
  summaries, and the "Thought signatures" section that
  `interactions/thought-signatures.md` redirects to: replay rules for
  stateless mode, and why stateful mode needs none of this.
- [Thought signatures (redirect stub)](interactions/thought-signatures.md)
- [Structured outputs](interactions/structured-output.md) — `response_format`
  / JSON Schema-constrained output on Interactions.
- [Understand and count tokens](interactions/tokens.md)
- [Code execution](interactions/code-execution.md) — built-in
  `code_execution` tool, its call/result step shapes.
- [Computer use](interactions/computer-use.md) — built-in `computer_use`
  tool, safety policies, environments.
- [File search](interactions/file-search.md) — built-in `file_search` tool
  over a file-search store.
- [Grounding with Google Search](interactions/google-search.md)
- [Grounding with Google Maps](interactions/maps-grounding.md)
- [Deep Research agent](interactions/deep-research.md) — the `agent` form of
  `input`/`model`, distinct from calling a plain Gemini model.
- [URL context](interactions/url-context.md)
- [Migrate from `generateContent` to the Interactions API](migrate-to-interactions.md)
  — side-by-side request/response shapes for both surfaces; useful as a
  second source for the OpenAI-format-vs-Interactions diff in
  [GEMINI-INTEGRATION.md §3](../../docs/GEMINI-INTEGRATION.md).

## Models and pricing

Not Interactions-specific, but load-bearing for picking and costing a model:

- [Gemini models](models.md) — the full model list; `gemini-3.7-flash` is
  under "Gemini 3 / Stable".
- [Pricing](pricing.md) — per-model rate cards, including
  `gemini-3.7-flash`'s introductory pricing, the 2027-01-01 rate doubling, and
  the context-caching per-hour storage charge, all of which
  `docs/GEMINI-INTEGRATION.md` §4 depends on.

## API reference

- [Interactions API reference](api/interactions-api.md)
- [`openapi.json`](openapi.json) — OpenAPI 3.0.3, `v1beta`, 14 paths, 181
  schemas.
