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

### The tool-message shape — hand-built request, not through the harness

§2's four-request table is what covers the shape actually shipped: an image
part inside the tool message that reports it, with no separate message
after it. Those requests were built by hand against the live API, outside
the harness, to isolate exactly one variable — where the image part sits —
against a control that removes it. The tool-message row returned 200 with
the correct answer at 295 prompt tokens.

No end-to-end harness run has exercised this shape yet. That is a real gap:
the hand-built requests prove the API accepts and reads the image in this
position, not that a real session's fold, tool dispatch, and streaming path
produce and send it correctly end to end.

### Two harness runs — a shape no longer shipped

Two sessions were run through this harness on 2026-08-28 against
`deepseek-v4-flash-vision-exp`, before this revert: `sess-9f4a7bd66718ba16a19fcd8ed8b079e2`,
one `Read` call on a terminal-UI screenshot, and
`sess-ebb915785a4e697b2d346c4383d59fe8`, two parallel `Read` calls in one
assistant turn. Both returned HTTP 200. Both got every asked-for detail
right — a version string, a tip line's wording, which list entry was
highlighted, two images kept distinct in the answer.

Both runs exercised the separate-user-message shape this revert removes,
not the tool-message shape the harness now sends. They are real evidence
that the model reads images correctly when driven through this harness's
actual tool-dispatch and fold path. They are not evidence about the shape
currently shipped — they confirm the model can be trusted to look at an
image reached through this harness, but say nothing about whether the
tool-message shape survives that same path uncorrupted. An end-to-end
harness run against the tool-message shape is the next thing that should
happen, and has not happened yet.

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

## 6. The routes not taken

Two other surfaces were considered. This section records why Chat
Completions is what shipped instead of either one.

The Responses API is the first. DeepSeek documents images in
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
