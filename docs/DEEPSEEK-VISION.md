# Can `deepseek-v4-flash-vision-exp` replace the Gemini vision seam?

Assessed 2026-08-27 against the docs mirror refreshed the same day
(`third_party/deepseek-docs/`). Nothing here has been run against the live API.

Short answer: **not on the API surface this harness speaks.** The model reads
images and prices them at a fraction of a Gemini call, so the prize is real. The
obstacle is where the image has to go. This harness delivers images to the model
in tool results, and DeepSeek's Chat Completions endpoint gives a tool message a
plain string for content. The Responses API is the surface that accepts an image
in tool output, and the harness does not speak it.

---

## 1. What the model is

`deepseek-v4-flash-vision-exp`, model version DeepSeek-V4-Flash-Vision-Exp,
released 2026-08-21 (`news/news260821.md`). DeepSeek calls it experimental and
says it matches `deepseek-v4-flash` on text.

It matches flash on the specifications this harness cares about, and on price
exactly (`quick_start/pricing.md`):

| | `deepseek-v4-flash` | `deepseek-v4-flash-vision-exp` |
| --- | --- | --- |
| Context / max output | 1M / 384K | 1M / 384K |
| Thinking mode | both | both |
| Tool calls | ✓ | ✓ |
| Responses API | ✓ | ✓ |
| Anthropic API | ✓ | ✓ |
| FIM completion | non-thinking only | not supported |
| Concurrency | 2500 | 2500 |
| 1M input, cache miss | $0.22 / $0.44 | $0.22 / $0.44 |
| 1M output | $0.66 / $1.32 | $0.66 / $1.32 |

Off-peak first, peak second. FIM is the only capability it loses, and no tool
here uses FIM.

Images cost at most **384 tokens each** (`guides/vision.md`, "Token Usage").
Every image is resized to roughly 800×800 before inference, so a 5000px capture
and a 2000px one bill the same. Up to 600 images ride in one request.

That number is the argument. A `Glance` against Gemini measured 1,121–1,195
input tokens per image and, in one case, 3,331 output tokens
([`VISION-TOOLKIT.md`](VISION-TOOLKIT.md) §7). At `gemini-3.7-flash`'s rates
that is around $0.013 for one look at one image. The same image inside a
DeepSeek request costs $0.000084 off-peak. Two sessions measured in
[`reviews/vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md) spent
23% and 39% of their total cost on the vision path.

## 2. Where the image has to go, and why that decides it

The harness already has the whole mechanism for a model that sees images. It
was built for Kimi K3 and reused for Gemini:

- `wire.Content` carries either a string or an array of parts, and
  `wire.PartTypeImageURL` is one of the part types (`internal/wire/content.go`).
- `tools.Result.ImageURL` carries a base64 data URI, set by `Read` and by the
  MCP image path (`internal/tools/registry.go`, `read.go`, `mcpexec.go`).
- The fold rebuilds the parts array from the stored event
  (`internal/fold/fold.go`), so a replay reproduces the image.
- `tools.Executor.SeeImages` gates both halves: whether `Read` returns an image
  part at all, and which tool array the session sends. On a vision provider the
  six DeepSeek-only tools — `Screenshot`, `Glance`, `Transcribe`, `Ground`,
  `Detect`, `Crop` — are not offered (`internal/tools/definitions.go`,
  [`GEMINI-INTEGRATION.md`](GEMINI-INTEGRATION.md) §5.7).

Every one of those images reaches the model as the content of a **tool
message**. That works on Kimi because Kimi's schema allows the parts array on
any role, tool included, and on Gemini because the request builder translates
the part into Gemini's own shape.

DeepSeek's Chat Completions schema does not allow it. A tool message's
`content` is `Text content (string)`, with no array variant
(`api/create-chat-completion.md`), and `guides/vision.md` states outright that
images are supported in `user` messages only. The harness posts to
`/chat/completions` (`internal/deepseek/stream.go:36`,
`internal/deepseek/client.go:111`). So a session on the vision model would build
exactly the request the API rejects.

Turning `seesImages` on for this model, with nothing else changed, drops the six
tools that let a DeepSeek session see anything and replaces them with a 400.

## 3. The surface that does allow it

The Responses API carries images in `input_image` parts, and those parts are
legal in the output of `function_call_output` and `custom_tool_call_output`
items (`guides/responses_api.md`, "Image Input"). DeepSeek documents the case
directly, with a `take_screenshot` tool in the example. That is the shape this
harness needs.

All three models support the Responses API, so the move is not specific to the
vision model. It is a second dialect for `internal/deepseek`: a different
request body, a different item vocabulary for the message array, a different
streaming envelope than the `wire.ChatCompletionChunk` frames
`internal/providerhttp.PumpStream` decodes, and a fresh frozen prefix for every
session that adopts it ([`DESIGN.md`](DESIGN.md) §3.2).

The Anthropic-format endpoint is the other candidate and is cheaper to reach
from here, because a `tool_result` block rides inside a `user` message rather
than a message of its own. `guides/anthropic_api.md` lists image blocks as
supported and `tool_result` with its `content` field as fully supported. It does
not say whether an image block is accepted *inside* a `tool_result`, which is
the only question that matters. One request against the live API settles it, and
that request is by far the cheapest next step available.

## 4. The per-provider assumption this model breaks

`seesImages()` resolves a model to a provider and answers on the provider
(`internal/session/runner.go`). `DefinitionsForProvider` switches on the
provider too, and its comment records that Kimi and Gemini share one array
because they drop the identical six tools (`internal/tools/definitions.go`).

`deepseek-v4-flash-vision-exp` would be the first model whose vision capability
disagrees with its provider's. Both functions have to become model-scoped before
it can be added, whichever transport wins. The tool array is frozen for a
session's life and stored on the session row, so this has to be right at
creation and on resume ([`CACHE.md`](CACHE.md)).

## 5. What to do

1. **Send one request** to `https://api.deepseek.com/anthropic/messages` with
   `deepseek-v4-flash-vision-exp`, carrying an image block inside a
   `tool_result` block. If it is accepted, the Anthropic path gives native
   vision for the cost of a dialect the client already has a place for, and the
   answer belongs in [`OBSERVED.md`](OBSERVED.md). If it is rejected, the
   Responses API is the only route and it is a much larger piece of work.
2. **Do not turn `seesImages` on for this model** until one of those two
   transports is in place. It would remove the tools that work and put nothing
   usable in their place.
3. **Add the model to `configs/prices.json` in the same change that adds it to
   `internal/provider`.** A model with no entry still runs. `Table.Cost`
   returns an error, and both callers that price a turn drop it and keep zero
   (`internal/session/turn.go:497`, `internal/tools/vision.go`), so the run's
   spend disappears from every figure in the UI without anything failing. The
   settings registry records the same trap for the vision model setting
   (`internal/settings/registry.go:218`). The model is absent from the table
   today, and its rates are `deepseek-v4-flash`'s exactly, so the entry is a
   copy — including the peak and off-peak maps under `rate_schedule`.

## 6. Not verified

- Nothing here was run against the live API. Section 2's blocker rests on the
  request schema in `api/create-chat-completion.md` and the restriction in
  `guides/vision.md`, which agree with each other; section 3's Anthropic
  question rests on an absence from the compatibility table and is genuinely
  open.
- Whether the model's own image understanding is good enough to replace
  `Glance`, `Ground` and `Detect` is untested. `Ground` and `Detect` return
  pixel boxes on a 0-1000 grid, which is Gemini's own convention
  ([`VISION-TOOLKIT.md`](VISION-TOOLKIT.md) §6). Nothing says DeepSeek honours
  that convention, and nothing here has measured its boxes.
- The 384-token ceiling caps what any single look can resolve, so the tall-page
  problem `Transcribe` exists for does not disappear on a vision model. It gets
  cheaper to chunk, not unnecessary.
- Files API images (`guides/files_api.md`) were not assessed. They raise the
  per-image ceiling to 64 MiB and avoid re-uploading, and the harness has no
  file-upload path today.
