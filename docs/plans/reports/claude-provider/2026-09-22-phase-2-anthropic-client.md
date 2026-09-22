# Phase 2 — the Anthropic client and the loop's replay plumbing

## What was built

**Replay plumbing.** `wire.EventProviderBlocks` and `wire.Event.ProviderBlocks`
carry one Anthropic response's assistant `content` array verbatim.
`wire.Item.ProviderBlocks` (`json:"-"`) carries it on the sub-turn's
reasoning item, the way `ThoughtSignature` does. `store.ReasoningDeltaPayload`
gained `ProviderBlocks`; `internal/session/turn.go`'s `stream` and `runSubTurn`
thread it through and commit the sub-turn whenever reasoning text, a
signature, or provider blocks is non-empty; `internal/fold.Fold` sets it on
the reasoning item it emits and swallows nothing — the request-building
decision to skip the sub-turn's other items belongs to the consumer
(`internal/anthropic`), not to the fold. `wire.Usage.CacheWriteTokens` and
`pricing.ModelPrices.InputCacheWritePerMillionUSD` carry Anthropic's
`cache_creation_input_tokens` and its distinct billing rate;
`pricing.Table.Cost` gained a `cacheWriteTokens` parameter (every existing
caller updated to pass 0) and `store.UsagePayload.PromptCacheWriteTokens`
records it. DeepSeek, Gemini and Kimi never set any of these fields.

**`internal/anthropic`.** A new package implementing `session.Client`
against `POST https://api.anthropic.com/v1/messages`, hand-rolled on
`internal/providerhttp.Transport` with `SetAuth`, following `internal/gemini`'s
shape:

- `types.go` — the request/response Go structs.
- `intent.go` — `requestFromIntent`: system prompt → top-level `system`;
  tool_use/tool_result rendering, with one sub-turn's tool results collapsed
  into one user message; mid-conversation system items rendered per model
  (`systemMessageModels`); the raw-block replay path and its fallback; tool
  array rendering plus the two server tools in fixed order; up to three
  cache breakpoints (`applyCacheBreakpoints`).
- `stream.go` — `readSSE`, this client's own SSE decoder (named events,
  content-block-indexed deltas), accumulating every content block into a
  `blockState` and rendering the complete raw array at `message_stop`;
  `mergeUsage` combines `message_start`'s usage (input/cache figures) with
  `message_delta`'s (the authoritative cumulative output figure).
- `client.go` — `Client`, `NewClient`, `StreamChatCompletion` and
  `CreateChatCompletion` (both with the bounded internal `pause_turn` resume
  loop), `UsageSplit`, `CacheSlack`, `IsReasoningStarved`/`RepairArguments`
  (no-ops).
- `errors.go` — `APIError`, `RefusalError`, `parseAPIError`,
  `isRetryableStatus` (429, 500/502/503/504, 529).
- `modelinfo.go` — `DisplayName`, `ContextWindowTokens`, `EffortLevelsFor`/
  `EffortSupported` for `claude-opus-5`, `claude-sonnet-5`,
  `claude-fable-5-1`.

**Tests**, beside the code: `intent_test.go` (request shape from fixed
intents, byte stability across two renders, cache breakpoint placement, the
raw-blocks-swallow-extra-items path, mid-conversation system rendering per
model), `stream_test.go` (SSE fixtures for text/tool_use/thinking-plus-
signature/an unknown verbatim-captured block kind/a mid-stream error, usage
merging), `client_test.go` (an `httptest` server driving
`StreamChatCompletion` and `CreateChatCompletion` end to end, headers, the
`pause_turn` resume loop, a refusal, retry-on-503, `UsageSplit`), `errors_test.go`,
`modelinfo_test.go`, and `fold_replay_test.go` (an event log built the way
`internal/session/turn.go` actually commits one, folded and rendered,
proving the raw-block sub-turn replays verbatim while an adjacent sub-turn
with no raw blocks reconstructs normally).

**`docs/ANTHROPIC-INTEGRATION.md`** — the provider reference, in the manner
of `docs/GEMINI-INTEGRATION.md` but written as current-state reference
rather than a phased narrative, since there was no multi-phase history to
narrate yet.

**The live check** — see below.

## What was verified, and how

`scripts/build.sh`'s tail:

```
=== gofmt
clean

=== go vet
clean

=== tests
ok  	.../internal/anthropic	(cached)
[... every other package ok ...]

=== cross-compile
darwin/arm64
darwin/amd64
linux/amd64
linux/arm64
android/arm64
windows/amd64

=== go build
bin/harness
```

`go test ./internal/anthropic ./internal/session ./internal/fold ./internal/pricing`
passes. `go test ./internal/stdiosession -run TestGoldenFrames` and
`go test ./internal/session -run TestPromptGolden` both pass without
`-update-golden` — the replay-plumbing additions moved no byte of any
existing provider's request.

**The live check.** `internal/anthropic/live_test.go`'s
`TestLiveMessagesAPI`, gated on `RUN_ANTHROPIC_LIVE_TEST` (never set by
`scripts/test.sh` or `scripts/build.sh`), run by hand against all three
models with the key from `~/.config/agent-harness/anthropic.env`, never
copied anywhere. `docs/OBSERVED.md`, "Claude Messages API — Phase 2 live
check", in full; the headline findings:

- A tool call, replayed with its raw blocks (including a real `thinking`
  block plus signature on one Sonnet 5 request), succeeded on the second
  request under `thinking-binding-controls-2026-08-01` and
  `prefix_mismatch_behavior: "error"` — no 400.
- No response across five measured pairs on Opus 5 and Sonnet 5 carried more
  than one `thinking` block (answering the plan's open question, as far as
  this narrow probe goes — every response carried zero or exactly one).
- The genuine cache miss (prompt tokens minus cache-read minus cache-write)
  was exactly 2 tokens on every one of five second-request measurements,
  across both effort levels and all three models.
  `Client.CacheSlack()` is set to 1024 on that strength — generous headroom
  over 2, since this was a single short session rather than the
  multi-sub-turn measurement Gemini's own `CacheSlack` was tuned against.
- Web search on Sonnet 5 round-tripped clean: a `server_tool_use` block and
  a `web_search_tool_result` block, both captured verbatim by the "keep the
  `content_block_start` object as-is for an unrecognised kind" fallback,
  with no special-casing of that block's shape needed.

No disagreement turned up between the feature memory's External facts table
and the live pages fetched for this phase (`api/messages`,
`build-with-claude/streaming`, `build-with-claude/prompt-caching`,
`build-with-claude/thinking-steering-and-cost` (the adaptive-thinking page's
current URL), `build-with-claude/effort`,
`agents-and-tools/tool-use/overview`, `api/errors`). The live
prompt-caching page's minimum-cacheable-tokens table is more granular
per-model than the memory's summarised "512–4096" range, but agrees with
it. The pricing page (`about-claude/pricing`) was not fetched — nothing in
this phase needed a rate, and `configs/prices.json` is phase 3's.

## What was left undone

- **Mid-stream reconnection.** `providerhttp.Transport.RetryStream`, the
  mechanism DeepSeek, Kimi and Gemini all use to reopen a stream that dies
  after a 200 but before producing output, is not wired up here. This
  client only retries the pre-response HTTP request (`Transport.Do`'s
  existing backoff); a connection that drops mid-SSE-read ends the turn.
  The plan's proof did not ask for this reuse explicitly and time did not
  allow adding it defensibly (it would need `readSSE` split into an
  opener/pump pair matching `providerhttp.StreamOpener`/`StreamPump`, and a
  decision about how partial `pause_turn` state interacts with a mid-read
  reconnect). Worth doing before this client sees real session traffic.
- **`CacheSlack`'s measurement is shallow.** One tool round per model/effort
  pair, not the multi-sub-turn incrementally-growing session Gemini's own
  figure was tuned against. 1024 is a defensible conservative number, not a
  confirmed one.
- **Whether more than one `thinking` block can appear in a single response**
  is answered only as far as five short exchanges on simple tool-calling
  prompts go. A harder multi-step agentic task might still produce one.
- **`CacheSlack`, effort defaults and per-model behaviour were not measured
  under real multi-turn session load** — no `harness stdio-session` process
  exists yet to drive one; that is phase 3's.

## Bugs found and not fixed

None found in Anthropic's API during the live check — every measured
behaviour matched the memory's External facts table or the live docs. One
latent issue was found and fixed in this client's own code before it
shipped: `readSSE`'s line-reading goroutine could block forever on an
unbuffered send when `readSSE` returns early (an error event, a decode
error, the idle watchdog) while the body still has unread bytes — the same
class of leak `providerhttp.PumpStreamWith`'s own `stopped` channel exists
to close. Fixed with an identical `stopped` channel before any test ran
against it, so it never reached a committed, uncaught state — noted here
because `internal/gemini/stream.go`'s `pumpChatEvents`, the closest
precedent this package's `readSSE` follows, has the same gap unfixed. A
future session touching that file should consider carrying the fix back.

## Mechanical friction

- Changing `pricing.Table.Cost`'s signature (a required addition for
  `cacheWriteTokens`) touched two production call sites and eight test call
  sites across three files (`internal/session/turn.go`,
  `internal/tools/vision.go`, `internal/pricing/pricing_test.go`,
  `cmd/harness/pricing_coverage_test.go`). Mechanical but not fast to find —
  `grep -rn "\.Cost("` across `internal` and `cmd` was the way to get all of
  them in one pass rather than discovering missed ones via broken builds one
  at a time.
- `json.Marshal` compacts a `json.RawMessage`'s insignificant whitespace.
  This cost one wrong test expectation (`TestReadSSEToolUse` originally
  expected a space `json.Marshal` had already stripped) before the
  behaviour was understood and documented in
  `docs/ANTHROPIC-INTEGRATION.md`'s "Streaming" section. Worth knowing
  before anyone assumes the raw-block replay is byte-identical to the
  original HTTP response rather than semantically identical.
- WebFetch on `build-with-claude/streaming` returned 49.8KB and was
  persisted to a tool-results file rather than inlined; reading it back with
  the `Read` tool worked fine, just an extra round trip worth expecting for
  any future fetch of that same page.
- No blocking friction otherwise: the build, the existing test suite, and
  the live API itself all behaved as documented.

## Memory suggestions

- **The raw-block commit condition matches the existing pattern exactly**:
  `internal/fold.Fold`'s reasoning-item-commit condition
  (`reasoning.Len() > 0 || signature != ""`) needed a third disjunct
  (`|| len(providerBlocks) > 0`) rather than a new branch, and the ordinary
  Anthropic sub-turn hits that third disjunct alone (no reasoning text
  accumulated separately, since `wire.Item.ThoughtSignature` is never used
  by this provider — the signature lives inside the raw blocks instead).
  Worth stating plainly for whoever reads the fold next: an Anthropic
  session's `reasoning_delta` events almost always carry `Text: ""`,
  `ThoughtSignature: ""`, and a non-empty `ProviderBlocks` — that is the
  normal case, not a degenerate one.
- **`json.RawMessage` inside a slice marshalled via `[]any` (the `Tools`
  field's shape, matching `internal/gemini`'s `Input []any` for
  `ChatInteractionRequest`) works exactly as that precedent suggests**: each
  element marshals through its own concrete type's field order, so a
  heterogeneous array stays byte-stable without a custom `MarshalJSON`.
  Nothing to add to the memory here — this confirms rather than revises the
  precedent Gemini's package already set. Worth citing directly the next
  time a package needs a heterogeneous JSON array, rather than re-deriving
  it.
- **`intent.go`'s system-message model table (`systemMessageModels`) is the
  one place in this phase where "the model" has to be known outside
  `modelinfo.go`.** Phase 3's provider table and phase 4's dialect work
  should know this exists before duplicating the Opus-5/Fable-5.1-vs-
  Sonnet-5 distinction somewhere else.
- **Files phase 3 will want that were not obviously named in the plan**:
  `internal/anthropic/modelinfo.go` is the complete answer to "what does
  `internal/stdiosession/modelinfo.go` need to know about each Claude
  model" — display name, context window, effort set — mirroring
  `internal/gemini/thinkinglevels.go`'s role exactly. `docs/OBSERVED.md`'s
  new section carries the `CacheSlack` figure's provenance and caveats;
  phase 3's live check on `stdio-session` should read it before deciding
  whether to re-measure.
