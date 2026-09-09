# `deepseek-v4-flash-vision-exp` native vision

Assessed 2026-08-27 against the docs mirror refreshed the same day
(`third_party/deepseek-docs/`). Implemented and verified against the live API
2026-08-28 — see §5.

The model reads images and prices them at a fraction of a Gemini call. The
first assessment believed a Chat Completions tool message could not carry an
image, on the strength of the vendored docs. A live measurement on
2026-08-28 shows the API accepts one and attends to it correctly (§2). The
harness sends the image inside the tool message that reports it, the same
shape it already used for Kimi and Gemini. `internal/fold/fold.go` builds
that message with no DeepSeek-specific branch, and the vision model is
registered and turned on.

---

## 1. What the model is

`deepseek-v4-flash-vision-exp`, model version DeepSeek-V4-Flash-Vision-Exp,
released 2026-08-21 (`news/news260821.md`). DeepSeek calls it experimental and
says it matches `deepseek-v4-flash` on text. DeepSeek's own Codex model
catalogue (`quick_start/agent_integrations/codex.md`) declares
`input_modalities: ["text", "image"]` for this model against `["text"]` for
`deepseek-v4-pro` and `deepseek-v4-flash` — a second, independent confirmation
from the vendored mirror that this model reads images, alongside
`guides/vision.md`.

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
here uses FIM. `configs/prices.json` carries it as an exact copy of flash's
entry in all three homes a rate can live in: the flat `Models` map and both
`rate_schedule` tiers. A model absent from that table still runs, and its
spend reads as zero in every figure that reports it, silently. Copying
flash's entry avoids that. `internal/pricing` pins the two entries equal by
test.

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

## 2. The tool-message blocker, and what the API actually does

A Chat Completions tool message's `content` is `Text content (string)`, with
no array variant (`api/create-chat-completion.md`), and `guides/vision.md`
states that images are supported in `user` messages only. The original
assessment of this document read those two statements as forbidding an
image inside a tool message, and built a separate `user` message to carry
the image around that restriction instead.

A live measurement against `deepseek-v4-flash-vision-exp` on 2026-08-28
contradicts the documented schema. Four consecutive requests, same setup —
a solid `rgb(0,153,102)` image and a question asking for its colour:

| shape | HTTP | prompt_tokens | answer |
| --- | --- | --- | --- |
| image inside the tool message | 200 | 295 | correct |
| image in a separate user message after the tool message | 200 | 319 | correct |
| no image, control | 200 | 177 | states plainly it cannot see one |

The control is what makes the other two rows readable: without an image the
model says so rather than guessing, so a correct colour in the other two
rows means the image reached the model in both shapes. The tool-message
shape works, and costs 24 fewer prompt tokens than the separate-message
shape — the difference is the lead-in sentence and the duplicated label the
separate message needed.

The harness now relies on behaviour the vendor's own documentation
contradicts. The exposure is concrete: if DeepSeek starts enforcing the
schema its docs already publish, a tool message carrying an image part
starts returning 400 on every vision run. Nothing measured here rules that
out — the four requests above establish only what the API does today, not
what it is contracted to keep doing. The project accepted this risk
knowingly on 2026-08-28, and this document is the record of that decision.

`wire.Content` already carries either a string or an array of parts, and
`wire.PartTypeImageURL` already exists as a part type, built for Kimi and
reused for Gemini. The DeepSeek image reuses the same vocabulary and the
same code path — see §3.

## 3. The message shape today

`internal/fold/fold.go` builds every tool-result message the same way
regardless of provider: `toolResultMessage` puts the label text and, when
the event carries one, the image part, both inside the one tool message
that answers the call. DeepSeek, Kimi, and Gemini all take this shape.
There is no per-provider branch in the fold, no batching state, and no
message queued for later — a tool result's message is complete the moment
its event is folded.

One fact from the investigation stays on record even though nothing in the
shipped shape depends on it: an interleaved `assistant(tool_calls: A, B) ->
tool(A) -> user(...) -> tool(B)` sequence is rejected by the live API with a
400, whose own message says an assistant message carrying `tool_calls` must
be followed by tool messages answering each `tool_call_id`. That finding is
why a future change should not reintroduce a separate `user` message into
the middle of a tool-result batch without checking this again first.

## 4. The resolution trade

DeepSeek resizes every image to roughly 800×800 and caps it at 384 tokens
regardless of the source resolution. A Gemini `Glance` was measured at
1,121–1,195 input tokens per image
([`VISION-TOOLKIT.md`](VISION-TOOLKIT.md) §7). Native DeepSeek vision costs
about $0.000084 per look against roughly $0.013 for the equivalent Gemini
call, and gives roughly a third of the visual resolution in return. It is the
cheap option. It is not strictly the better one.

Small text on a full-page screenshot will be less legible at 384 tokens than
it is to a Gemini call spending three times that. `Transcribe` and `Crop`
existed specifically to compensate for a model that could not resolve fine
detail on its own — `Transcribe` chunks a tall page into pieces small enough
to read, and `Crop` cuts a region out so `Glance` can look at it at full
resolution. Turning the vision capability on for this model drops both of
them, along with `Screenshot`, `Glance`, `Ground`, and `Detect`: the model
reuses `definitionsVisionCapable`, the same fourteen-tool array Kimi and
Gemini already share, instead of a third array carrying some subset of the
six. The user decided this explicitly on 2026-08-28, aware of the
consequence: a session on this model has no compensating tool to fall back
on when a screenshot's detail exceeds what 384 tokens can carry. It works at
native resolution or it does not work at all.

## 5. Verified against the live API

Two different things have been measured, on two different surfaces, and they
cover different shapes. Neither substitutes for the other.

### The tool-message shape, hand-built

§2's four-request table covers the shape shipped: an image part inside the
tool message that reports it, with no separate message after it. Those
requests were built by hand against the live API, outside the harness, to
isolate one variable — where the image part sits — against a control that
removes it. The tool-message row returned 200 with the correct answer at 295
prompt tokens.

### The tool-message shape, end to end — `sess-11d91e194a3174ef60e621cb0e2d5544`

The same shape driven through the harness, after the revert. Two images
attached, the run told to issue both `Read` calls in one assistant turn.
Both images were read correctly and kept distinct: each dialog's title, which
entry carried the highlight in each, and the two keyboard shortcuts printed
along the bottom of the second.

The request the harness sent, HTTP 200, read from its own provider trace:

    system     string
    user       string
    assistant  string, tool_calls=2
    tool       parts: label, image_url
    tool       parts: label, image_url

Five messages where the removed shape sent six. No message follows the tool
run. This is byte-for-byte the arrangement Kimi and Gemini already get, which
is the point of the revert.

### Two earlier harness runs — a shape no longer shipped

Two sessions ran before the revert: `sess-9f4a7bd66718ba16a19fcd8ed8b079e2`,
one `Read` call on a terminal-UI screenshot, and
`sess-ebb915785a4e697b2d346c4383d59fe8`, two parallel `Read` calls in one
assistant turn. Both returned HTTP 200 and got every asked-for detail right.

Both exercised the separate-user-message shape the revert removes. They are
kept on record because they are what established that the model reads images
correctly when driven through this harness at all. The run above is what
covers the shape now shipped.

### Still unverified

- Whether the model's image understanding holds up across the range of work
  `Glance`, `Ground`, and `Detect` did through Gemini. A handful of images
  read correctly is a handful of data points, all on large, high-contrast
  UI text.
- Whether the 384-token ceiling makes screenshot-driven work materially worse
  in practice. §4 states the trade in principle. The probe images were around
  2,950 pixels wide and their small text was read correctly, which is
  encouraging and is not a measurement.
- Everything about `Ground` and `Detect`'s pixel-box convention. None of the
  measurements above asked the model for coordinates.

## 6. The one hosted model over stdio

`harness stdio-session` hosts this model and neither of the other two
DeepSeek models, and §4 is the whole reason. That process hosts one session
for a parent application over a pipe and takes its credentials from the
environment the parent spawned it with
([`STDIO-PROTOCOL.md`](STDIO-PROTOCOL.md)). A session on
`deepseek-v4-flash` or `deepseek-v4-pro` there would be given the six tools
that exist because a model cannot see — `Screenshot`, `Glance`, `Ground`,
`Detect`, `Transcribe`, `Crop` — and four of those send their images to
Google. The parent would have had to supply a Google API key to make a
DeepSeek session work, or watch four tools fail whenever the model reached
for one. Turning this model's vision capability on is what removes that: it
is offered none of the six, `Read` hands it the image directly, and one
credential is enough.

Nothing about `harness serve` changes. It routes all three models as it
always has, and the vision tools there have a Google key configured
alongside.

## 7. The routes not taken

Two other surfaces were considered. This section records why Chat
Completions is what shipped instead of either one.

The Responses API is the first — and it is no longer a route not taken:
`internal/deepseek` speaks it, and `harness stdio-session` posts to it
([`DEEPSEEK-RESPONSES.md`](DEEPSEEK-RESPONSES.md)). A live probe on
2026-09-10 read an image out of a `function_call_output` correctly
([`OBSERVED.md`](OBSERVED.md)), which is this document's §2 risk answered
rather than accepted. What follows is the assessment as it stood when Chat
Completions was chosen, kept because `harness serve` still posts there and
the reasoning still applies to it.

DeepSeek documents images in
`function_call_output` and `custom_tool_call_output` output directly, with a
`take_screenshot` example (`guides/responses_api.md`, "Image Input") — an
image as part of a tool's own output, rather than a message that follows it.
All three models support the Responses API, so adopting it is not specific
to the vision model. It costs a whole second dialect for
`internal/deepseek`: a different request body, a different item vocabulary
for the message array, a different streaming envelope than the
`wire.ChatCompletionChunk` frames `internal/providerhttp.PumpStream`
decodes, and a fresh frozen prefix for every session that adopts it
([`DESIGN.md`](DESIGN.md) §3.2).

The Anthropic-format endpoint is the second. There, a `tool_result` block
rides inside a `user` message rather than arriving as a message of its own.
`guides/anthropic_api.md`'s compatibility table now lists `type="image"` as
Supported (it was Not Supported before the 2026-08-27 mirror refresh) and
`tool_result` with its `content` field as Fully Supported. The table states
what each shape supports on its own. It does not state whether an image
block is accepted nested inside a `tool_result`, and that is the one
question that decides whether this route works at all.
