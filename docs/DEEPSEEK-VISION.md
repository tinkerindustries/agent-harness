# `deepseek-v4-flash-vision-exp` native vision

Assessed 2026-08-27 against the docs mirror refreshed the same day
(`third_party/deepseek-docs/`). Implemented and verified against the live API
2026-08-28 — see §7.

The model reads images and prices them at a fraction of a Gemini call. The
obstacle the first assessment found was real: a Chat Completions tool message
cannot carry an image. The fix is not a new transport. The image rides a
second message instead of the tool message, placed after the whole batch of
tool results it belongs to. `internal/fold/fold.go` builds that message, and
the vision model is registered and turned on.

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

## 2. The tool-message blocker, and the option it missed

A Chat Completions tool message's `content` is `Text content (string)`, with
no array variant (`api/create-chat-completion.md`), and `guides/vision.md`
states that images are supported in `user` messages only. Both facts were
correct in the original assessment of this document. The harness posts to
`/chat/completions` (`internal/deepseek/stream.go`, `internal/deepseek/client.go`),
so a tool message built for this endpoint really cannot carry an image part.

What the original assessment concluded from that was wrong. It read the
blocker as ruling out native vision on this surface entirely, because every
image the harness produces is the result of a tool call, and a tool call's
result goes in a tool message. The image does not have to live inside the
tool message that reports it, though. It can ride in a separate `user`
message that follows the tool message — the one role `guides/vision.md`
names as accepting images. That needs no new endpoint, no new dialect, and
no new streaming envelope. It needs a second message in the array the fold
already builds.

`wire.Content` already carries either a string or an array of parts, and
`wire.PartTypeImageURL` already exists as a part type, built for Kimi and
reused for Gemini. The DeepSeek shape reuses the same vocabulary.

## 3. The sidecar design

`internal/fold/fold.go` builds the message array from the event log. For a
DeepSeek model (`deepSeekSidecarShape`, which checks `provider.ModelFor(model)
== provider.DeepSeek` — see §4 for why it checks the provider and not the
capability), a tool-result event's text goes into an ordinary tool message,
exactly as before. If the event also carries an image, that image is not
attached to the tool message. It is buffered, and flushed as one `user`
message once the whole batch of tool calls the current assistant message
opened has been answered.

That "once the whole batch" rule is the part that has to be exact, and the
reason is the Chat Completions contract itself: an assistant message that
carries `N` tool_calls must be answered by exactly `N` tool messages before
anything else appears. A `user` message wedged between the second and third
of five tool messages answering the same assistant turn is not documented as
legal, and the plan that shaped this design treats it as a likely 400. So the
fold cannot flush an image the moment its own tool result arrives — it has to
wait until every tool result in the batch has arrived, emit all of the tool
messages first, and only then emit the one sidecar `user` message carrying
every image the batch produced. `internal/fold/fold_test.go`'s
`TestFoldDeepSeekSidecarParallelImages` and
`assertNoInterleavedUserMessage` exist to catch a regression on exactly this
point: they scan the built message array for a tool message immediately
followed by a user message immediately followed by another tool message, the
literal shape of the interleaving this rule forbids.

The sidecar message opens with a fixed lead-in sentence,
`"The harness placed these images here, from the tool results above, because
a tool message cannot carry them directly."`, before the per-image parts.
Without it, a sidecar message is structurally indistinguishable from one a
person actually sent — `wire.RoleUser` carrying prose and an image is the
same shape a real steer or continuation takes when someone pastes an image
into the composer. The lead-in tells the model which case it is looking at.
Each image is preceded by its own text part carrying the label the tool
already produced (`"Image: <path>"`), so a batch with more than one image
still lets the model tell them apart.

A batch with no image-bearing tool result flushes nothing — there is no
sidecar message when there is nothing to put in it. A batch mixing an
image-bearing result with a plain one, or with a permission denial, still
flushes exactly one sidecar carrying only the images that exist; a denial
carries no `ImageURL` at all, since a denied call never ran, but it still
counts toward the batch's size, or a sidecar for the batch's other,
image-bearing result would never flush.

## 4. Why the fold discriminates on the provider, not on `SeesImages`

The vision split elsewhere in the harness — which tool array a session sends,
whether `Read` returns an image part — is decided by `provider.SeesImages`,
a table keyed by model rather than by provider, because this model is the
first whose vision capability disagrees with the rest of its provider's.

The fold's choice of *shape* cannot use that same table. Kimi and Gemini both
have `SeesImages(model) == true`, and both want the parts-in-tool-message
shape that `toolResultMessage` already builds, unchanged. Now that
`deepseek-v4-flash-vision-exp` also has `SeesImages(model) == true`, "sees
images" no longer picks out one shape: two of the three models it is true
for want one shape, and the third wants the other. The shape follows a fact
about the provider's endpoint — whether its Chat Completions schema allows a
parts array inside a tool message. `fold.go`'s `deepSeekSidecarShape`
checks `provider.ModelFor(model) == provider.DeepSeek` directly, independent
of `SeesImages`.

## 5. The append-only constraint that shaped the flush

[`DESIGN.md`](DESIGN.md) §4.1 requires that folding a prefix of a session's
event log never disagree with folding the full log: a message the fold has
already emitted for a shorter prefix must still be there, byte for byte, once
more events arrive. `Fold` is called on every prefix the harness ever asks
about, not only complete logs, so this is not a hypothetical concern for the
DeepSeek sidecar.

That is why the sidecar counts tool results against the batch's own declared
size — the number of `tool_calls` the assistant message that opened the batch
carried — rather than flushing at "the next event that is not a tool result"
or "the end of whatever slice of events was passed in." A prefix cut between
the first and second tool result of a two-image batch is real input to
`Fold`. Flushing there would emit a one-image sidecar that a longer fold, once
the second result lands, has no way to agree with, because a message already
emitted may never be revisited. Counting against the batch's own known size
means a shorter fold has either already emitted the exact sidecar a longer
fold would, or has emitted none yet. `TestFoldDeepSeekSidecarAppendOnly`
folds every prefix of a two-image-batch event log and checks each one
against the full fold to pin this directly.

`internal/session/turn.go` commits a whole sub-turn's tool results, and any
denials mixed into the same batch, as one `store.AppendEvents` call. A
session's real event log therefore never holds a partially-recorded batch.
The prefix-agreement requirement still governs `Fold`'s own contract, for
the arbitrary events slice a caller such as `internal/session/resume.go`'s
`primeDetector` or a `Fold` call over a compaction boundary may pass it.

## 6. The resolution trade

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

## 7. Verified against the live API

Both assumptions the design rested on are confirmed. One run, session
`sess-9f4a7bd66718ba16a19fcd8ed8b079e2`, on `deepseek-v4-flash-vision-exp`
through this harness on 2026-08-28.

The two questions were whether a `user` message may directly follow a run of
`tool` messages answering the same assistant turn, and whether images in that
position are attended to rather than read as an unrelated aside. The mirror
documents neither as allowed nor as forbidden in this exact position.

The run attached a screenshot of a terminal UI, asked the model to `Read` it
and to report three details that appear nowhere except in those pixels: a
version string in the corner, the wording of a tip line, and which list entry
carried the highlight. It was told to say it could not see the image rather
than describe what the image probably showed, so a bluff would be legible as
one. All three answers were correct against the file.

The API accepted the request. The run finished in two sub-turns with no
error and no retry. The fold could only have built the sidecar shape, because
`deepSeekSidecarShape` answers true for every model the DeepSeek provider
serves, and the parts-in-tool-message shape would have been rejected — a tool
message's `content` takes a string and nothing else.

The image cost is close to the documented ceiling. Prompt tokens went from
3,273 in the first sub-turn to 3,927 in the second, and that delta of 654
covers the assistant's tool call, the tool message, the lead-in, the label
and the image together. The whole run cost $0.0011.

Still unverified:

- Whether the model's image understanding holds up across the range of work
  `Glance`, `Ground`, and `Detect` did through Gemini. One screenshot read
  correctly is one data point, on a task with large, high-contrast text.
- Whether the 384-token ceiling makes screenshot-driven work materially
  worse in practice. §6 states the trade in principle. The probe image was
  2,966 pixels wide and its small text was read correctly, which is
  encouraging and is not a measurement.
- Everything about `Ground` and `Detect`'s pixel-box convention. Nothing here
  asked the model for coordinates.

## 8. The routes not taken

Two other surfaces were considered. This section records why Chat
Completions, with the sidecar message, is what shipped instead of either one.

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
rides inside a `user` message rather than arriving as a message of its own,
which would sidestep the batch-ordering question §3 and §5 work through.
`guides/anthropic_api.md`'s compatibility table now lists `type="image"` as
Supported (it was Not Supported before the 2026-08-27 mirror refresh) and
`tool_result` with its `content` field as Fully Supported. The table states
what each shape supports on its own. It does not state whether an image
block is accepted nested inside a `tool_result`, and that is the one
question that decides whether this route works at all.
