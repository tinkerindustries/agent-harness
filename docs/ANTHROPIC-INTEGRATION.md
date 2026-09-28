# Anthropic: `internal/anthropic`

`internal/anthropic` is a client for Anthropic's Messages API
(`POST https://api.anthropic.com/v1/messages`), hand-rolled the same way
`internal/gemini` and `internal/deepseek` are: request and response bodies
are Go structs, never `map[string]any`, so identical values always
serialise to identical bytes — the byte-stability contract the prompt
cache depends on (`docs/DESIGN.md` §3.2). It implements
`internal/session.Client`, the narrow seam `internal/deepseek`,
`internal/kimi` and `internal/gemini` also implement, so the agent loop
never learns Anthropic's request shape.

It hosts three models: `claude-opus-5-5`, `claude-sonnet-5-5` and
`claude-fable-5-1` (`modelinfo.go`). `internal/provider.Anthropic` routes
them, `internal/tools.DefinitionsFor` gives them the vision-capable tool
array without `WebFetch` (Anthropic's own `web_search` and `web_fetch`
server tools ride the request instead, appended after the client-declared
array by `intent.go`'s `toolsFromWire`), and `harness stdio-session` hosts
all three behind `ANTHROPIC_API_KEY` (`cmd/harness/stdiosession.go`,
`docs/STDIO-PROTOCOL.md`).

## The raw-block replay unit

Anthropic's own preserved-thinking check requires every `thinking` and
`redacted_thinking` block from the most recent assistant turn to come back
exactly as it was received, in the same order, alongside whatever other
blocks — `text`, `tool_use`, `server_tool_use`, a `*_tool_result` — that
turn carried. Reconstructing that shape from the harness's own decomposed
vocabulary (reasoning text, content text, a list of tool calls) cannot
reproduce it: there is no field in `wire.Item` for a `server_tool_use` block
or a `*_tool_result` block, and reassembling in a different order or
dropping an empty `thinking` field both trip the check.

So this client does not reconstruct a replayed turn. It captures the whole
response's `content` array once, verbatim, as one `wire.EventProviderBlocks`
event (`wire.Event.ProviderBlocks`), and carries it on the sub-turn's
reasoning item (`wire.Item.ProviderBlocks`, `json:"-"` — never serialised
into another provider's request) the same way Gemini's thought signature
rides on the item. `internal/store.ReasoningDeltaPayload.ProviderBlocks`
persists it with the sub-turn; `internal/fold.Fold` sets it on the
reasoning item it emits, committing the item whenever reasoning text, a
signature, or provider blocks is non-empty — the common case here is
provider blocks present with no signature and no separate reasoning text at
all, since Anthropic's own `thinking` block already carries its own
signature and this client never needs `wire.Item.ThoughtSignature`.

When `requestFromIntent` (`intent.go`) meets a reasoning item carrying
`ProviderBlocks`, it renders that sub-turn's assistant message from those
bytes directly and swallows the `ItemMessage`(assistant) and
`ItemFunctionCall` items `fold.Fold` still emits for the same sub-turn —
they exist for the executor and the transcript, not for this client's
request. Only `internal/anthropic` ever reads `ProviderBlocks`; DeepSeek,
Gemini and Kimi never set it, so it is empty and omitted on every request
and event they produce, and the existing goldens (`TestPromptGolden`,
`TestGoldenFrames`) pin that unchanged.

A sub-turn with no captured blocks — none exist yet for a session this
client has run its whole life — falls back to reconstructing a `thinking`
block from the item's reasoning text and (a Gemini-style) signature field,
best-effort. This path is not expected to be exercised in production; it
exists so the request builder degrades rather than panics if it ever is.

## Request

`requestFromIntent` renders `wire.ChatIntent` into a `MessagesRequest`
(`types.go`). The leading system item (`wire.SystemPromptOf`) becomes the
top-level `system` field, one text block. Every other item becomes a
message:

- `tool_use` and `tool_result` blocks render from `ItemFunctionCall` and
  `ItemFunctionCallOutput`. All of one sub-turn's tool results collapse into
  one `user` message carrying one `tool_result` block per call — the shape
  the API expects for a turn's outputs — rather than the one-message-per-
  result shape the OpenAI-format dialect sends.
- An image in a tool result or a user message becomes an `image` content
  block, decoded from the data URI `wire.ItemPart.ImageURL` already
  carries.
- A mid-conversation `wire.RoleSystem` item — a steer or a reminder the loop
  injects — renders as a `system`-role message on all three models. A model
  `systemMessageModels` in `intent.go` does not name gets a user text block
  instead.

Tool schemas render as `input_schema` tools (`ToolDefinition`), carried
through as the same `json.RawMessage` `wire.ToolFunction.Parameters`
already holds. `web_search_20260209` and `web_fetch_20260209`
(`ServerToolDefinition`) are appended after the frozen array, in that fixed
order, on every request — this client's tool array is never shorter than
those two entries.

`max_tokens` is required, and zero is a 400. An intent that names no ceiling
gets `defaultStreamMaxTokens` (64,000) on a streamed request and
`defaultUnaryMaxTokens` (16,000) on a unary one (`modelinfo.go`). This is
every sub-turn a parent runs without `max_output_tokens`. DeepSeek and
Gemini read zero as their own default, so their clients leave the field out
instead.

## Thinking and effort

Every request sends adaptive thinking unconditionally:

```json
"thinking": {
  "type": "adaptive",
  "display": "summarized",
  "block_binding": {"prefix_mismatch_behavior": "error"}
}
```

`block_binding` needs the `thinking-binding-controls-2026-08-01` beta
header, sent on every request via `Transport.SetAuth` (`client.go`) —
Anthropic's live docs record the header as required on Fable 5.1 for orgs
created on or after 2026-08-31 and optional elsewhere, but this client sends
it everywhere rather than branching on org age or model, since the loop's
append-only history (`internal/CLAUDE.md`'s invariant) is exactly what
`prefix_mismatch_behavior: "error"` is meant to enforce: a signature that no
longer matches its conversation prefix is a bug in this client's replay,
not something to paper over with `"drop_block"`.

`intent.Effort` maps straight onto `output_config.effort` when non-empty;
empty omits the field, leaving the API's own per-model default in force —
`"medium"` on Opus 5.5, `"high"` on Sonnet 5.5 and Fable 5.1. A session never
reaches that branch: `internal/stdiosession`'s `effortFrom` resolves a
create naming no effort to `"high"` before the intent is built.
`intent.Thinking` is not read — there is no on/off toggle to spell on this
surface, matching `internal/kimi`'s and `internal/gemini`'s own reasoning
for ignoring it.

## Caching

Up to three of the API's four available breakpoints are placed on every
request (`applyCacheBreakpoints`, `intent.go`): the last client-declared
tool, the system block, and the last content block of the last message —
provided that message is one this client built itself rather than a
raw-replayed assistant turn (a request's items never end there; see "The
raw-block replay unit" above). This is the ordinary "cache everything up to
now" strategy for a growing, append-only conversation: the API's lookback
(at most 20 blocks behind a breakpoint) finds the previous request's cache
automatically as the array grows by append, without this client tracking
which prefix was already cached.

`CacheSlack()` — the churn detector's tolerance for this provider
(`internal/session.Client`'s own doc comment) — is measured, not guessed:
see `docs/OBSERVED.md`, "Claude Messages API — Phase 2 live check". It is
a single short session's measurement, not the multi-sub-turn
incrementally-growing one `internal/gemini.CacheSlack`'s own doc comment
describes; a later phase driving a real multi-sub-turn session should
revisit it the way Gemini's own figure was revisited.

`wire.Usage.CacheWriteTokens` carries `cache_creation_input_tokens`,
billed at `pricing.ModelPrices.InputCacheWritePerMillionUSD` — a rate
distinct from an ordinary cache miss's. `Client.UsageSplit` subtracts it out
of `cacheMiss` rather than folding it in, so `internal/pricing.Table.Cost`
never double-counts a cache write as a miss.

## Streaming

`stream.go`'s `readSSE` is this client's own SSE decoder — the frame
vocabulary (named events: `message_start`, `content_block_start`,
`content_block_delta`, `content_block_stop`, `message_delta`,
`message_stop`, `ping`, `error`) has no counterpart in
`internal/providerhttp.PumpStream`, which decodes the OpenAI-format chunk
shape, the same reason `internal/gemini`'s `pumpChatEvents` is its own
reader.

`thinking_delta` becomes `wire.EventReasoningDelta`, `text_delta` becomes
`wire.EventContentDelta`, and `input_json_delta` on a `tool_use` block
becomes `wire.EventToolCallDelta` — `server_tool_use` blocks stream the
same `input_json_delta` shape but are never forwarded as a tool-call delta,
since the harness's own executor does not dispatch a server tool call.
`signature_delta` is captured onto the block being assembled but produces
no wire event of its own; it rides out on the raw block, not as a delta to
accumulate.

Every content block, known or not, is accumulated into a `blockState`
(`stream.go`) and rendered into the raw block array once the stream ends at
`message_stop`: the five kinds this client reassembles from deltas
(`text`, `thinking`, `redacted_thinking`, `tool_use`, `server_tool_use`)
render from their accumulated fields, and anything else — chiefly a
`*_tool_result` block, which arrives whole at `content_block_start` with no
deltas at all, per the streaming guide's own examples — is kept verbatim
from that frame. One `wire.EventProviderBlocks` event carries the complete
array, emitted after the last delta and before `EventUsage` and
`EventFinish`.

A `json.RawMessage` is compacted (insignificant whitespace stripped)
whenever `encoding/json` marshals it, so the raw blocks this client
re-emits are not byte-identical to the original HTTP response — a `tool_use`
input's `{"a": 1}` becomes `{"a":1}` — but every string value, `thinking`
text and `signature` included, is untouched, since compaction only removes
whitespace outside string literals. Anthropic's preserved-thinking check
reads the `signature` field's string value, not the surrounding bytes, so
this is inert.

## `pause_turn`

A response ending `stop_reason: "pause_turn"` means Claude wants another
round of server-tool use within what the loop sees as a single request.
Both `StreamChatCompletion` and `CreateChatCompletion` resume internally,
bounded by `maxPauseTurnResumes` (5): the just-received response's raw
blocks are appended as a new `assistant`-role message with no new `user`
turn, and the request is resent. The loop is given one logical response
regardless of how many resumes it took: the raw blocks from every resumed
request concatenate, in order, into a single `wire.EventProviderBlocks`,
and the usage from each resumed request sums into one `wire.EventUsage`.

## Errors and retry

`Transport.Retryable` (`errors.go`'s `isRetryableStatus`) retries 429, the
5xx server faults (500, 502, 503, 504) and 529 (`overloaded_error`) with
the transport's existing backoff — the set `docs/api/errors.md` names,
wider than `internal/gemini`'s (429/500/503 alone) because Anthropic's own
docs are explicit about the wider set, unlike the Interactions API's silent
`openapi.json`.

A `stop_reason: "refusal"` response — HTTP 200, Claude having declined to
continue — ends the turn with a `*RefusalError` carrying the response's
`stop_details` verbatim, on both the streaming and unary paths.

## Mid-stream retry

A connection that dies after a 200 but before `message_stop` — a reset, a
timeout on `readSSE`'s own idle watchdog (`ErrIdleTimeout`) — is reopened
with the same request body and re-pumped, on
`providerhttp.Transport.RetryStream`'s own backoff schedule
(`docs/DESIGN.md` §4.3, "Retrying a stream that dies mid-flight"), the same
mechanism `internal/deepseek`, `internal/kimi` and `internal/gemini` wire
onto their own streams. `client.go`'s `streamOneRound` is the seam:
`readSSE` is unmodified and wrapped in a `providerhttp.StreamOpener`/
`StreamPump` pair per call, because the retry unit is one HTTP response's
whole SSE body and `pause_turn` can mean several of those within a single
sub-turn the loop sees as one request. The retry applies per round rather
than around the whole `pause_turn` loop: a round that already streamed
output before dying still ends the sub-turn on that output, exactly like
every other provider (`RetryStream`'s own "has this stream spoken yet"
gate) — reopening would resend a request the API may already be mid-way
through answering, and there is no way to tell the caller's already-forwarded
deltas apart from a second, independent completion's.

## The rest of the seam

`IsReasoningStarved` and `RepairArguments` are both no-ops. Neither
DeepSeek pathology applies here: `max_tokens` caps a whole response
(thinking and text together) rather than exposing a reasoning-only budget
to starve, and tool input arrives as a genuine JSON object this client
accumulates from an already-valid `partial_json` stream, never a string the
model could mis-punctuate.

## Live check

`internal/anthropic/live_test.go`'s `TestLiveMessagesAPI` drives the real
API by hand — gated on `RUN_ANTHROPIC_LIVE_TEST`, which `scripts/test.sh`
and `scripts/build.sh` never set. Run it with the key from
`~/.config/agent-harness/anthropic.env`:

```sh
set -a; source ~/.config/agent-harness/anthropic.env; set +a
RUN_ANTHROPIC_LIVE_TEST=1 go test ./internal/anthropic -run TestLiveMessagesAPI -v
```

Findings are in `docs/OBSERVED.md`, "Claude Messages API — Phase 2 live
check".
