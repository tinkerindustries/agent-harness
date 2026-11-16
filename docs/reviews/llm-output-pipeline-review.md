# Hosted LLM → harness output pipeline review

Date: 2026-11-17

Revision reviewed: `f90b6e59d42c1773a36642d878eafe36eb5593f0`

## Summary

Found six information-integrity issues: incomplete answers can be reported as successful; failed requests lose usage; Claude cache writes disappear from token totals; retry reasoning contaminates the final transcript; returned snapshots share mutable live data; and Claude citations never reach the parent.

Five findings were reproduced with temporary Go probes against the actual session and translator implementations. Citation loss was confirmed by tracing the source. No production code was changed.

| Priority | Finding | Principal affected path |
| --- | --- | --- |
| High | Truncated output is reported as successful completion | Provider finish reason → agent loop → all dialects |
| Medium | Usage received before an error is discarded | Provider stream → session accounting |
| Medium | Cache-write tokens disappear from reported totals | Claude usage → Responses / Managed Agents |
| Medium | Discarded retry reasoning becomes final output | DeepSeek retry → live text → Responses |
| High | Snapshot data remains mutable after its lock is released | Responses get → concurrent JSON serialization |
| Medium | Citation and server-tool metadata stop at the event log | Claude provider blocks → parent translators |

“High” means a correctness issue that can mislead a caller about completion or compromises concurrent output consistency. These priorities do not imply observed production incidents.

## Scope and approach

This repository is a Go harness, despite the generic game-mode instructions. The relevant path is provider SSE decoding → `wire.Event` → `session.stream` and `runSubTurn` → SQLite events plus ephemeral hub deltas → `stdiosession` translators → JSON-RPC stdout.

Reviewed the architecture and local protocol contracts, provider stream handling, session accumulation and retry behavior, hub/pump delivery, parent-facing translators, and JSON serialization. The concrete reproduction work focused on the shared loop and Responses/Managed Agents output. This was not an exhaustive review of every tool, Kimi integration, resume scenario, or transport failure. No live provider calls or external parent application were used.

## 1. Truncated output is reported as successful completion

**Evidence:** [turn.go](../../internal/session/turn.go), especially line 465; [lifecycle.go](../../internal/session/lifecycle.go), lines 400–403; [translate.go](../../internal/stdiosession/translate.go), `KindTurnFinished` handling.

The provider's finish reason is retained in `TurnFinishedPayload`, but is not returned in `subTurnOutcome`. Once there are no tool calls and no deferred input, the loop finishes with `StatusOK` and reason `no_tool_calls`. The translators do not expose the stored finish reason.

The reasoning-starved retry does not fix this: it addresses an exhausted budget with no answer text. A response with partial text and finish reason `length` takes the ordinary success path. DeepSeek's Responses adapter explicitly maps `response.incomplete` to `length`, so this is reachable through the real adapter, not just a theoretical interface input.

**Reproduction:** A fake provider emitted `unfinished sentence`, then `EventFinish{FinishReason: "length"}`. The real Runner returned:

```text
status=ok reason=no_tool_calls text="unfinished sentence"
```

**Impact:** A parent receives a completed run without knowing that the provider cut the answer short. It cannot reliably distinguish a complete answer from an interrupted sentence or partial implementation report. The same no-tool branch also does not distinguish a content-filter finish.

**Recommendation:** Carry the finish reason through the run outcome and define explicit handling for truncation/filtering. Preserve the reason on the parent resource even if policy chooses to end rather than continue. Add an end-to-end test with nonempty text plus a length finish.

## 2. Usage received before a provider error is discarded

**Evidence:** [responsesstream.go](../../internal/deepseek/responsesstream.go), line 85 onward; [turn.go](../../internal/session/turn.go), lines 716–724 and the early error returns in `runSubTurn`.

DeepSeek's `response.failed` decoder intentionally emits any supplied usage before emitting the error. `session.stream` collects that usage, but when `streamErr != nil` it returns nil usage along with the error. `runSubTurn` then returns before appending its usage batch.

**Reproduction:** Feed the real `Runner.stream` a usage event containing 150 input and 10 output tokens, followed by a provider error. It returns the error and **nil usage**.

There is a second manifestation: the first reasoning-starved attempt's usage is held in memory until the retry succeeds. If the retry fails, the first request's known usage is never committed either.

**Impact:** Failed requests can have known token spend that is absent from both the persisted accounting and parent-facing totals. This is distinct from a transport failure where the provider never supplied usage.

**Recommendation:** Return received usage alongside stream errors and commit accounting independently of whether the assistant turn is valid for replay. Retain failed-attempt metadata without inserting incomplete tool calls into conversation history. Test both a failed response carrying usage and a successful first attempt followed by a failed starvation retry.

## 3. Claude cache-write tokens disappear from parent token totals

**Evidence:** [anthropic/client.go](../../internal/anthropic/client.go), `UsageSplit` at line 514; [translate.go](../../internal/stdiosession/translate.go), `usageFrom` at line 507; [managedagentstranslate.go](../../internal/stdiosession/managedagentstranslate.go), `maUsageFrom` at line 418.

The Anthropic adapter correctly separates input into cache reads, uncached input, and cache writes. Its cache-miss value deliberately excludes writes because they have separate pricing. The stored usage retains `PromptCacheWriteTokens` and the full `PromptTokens`.

The output translators reconstruct input as **cache hit + cache miss**, omitting writes. Their total-token fields consequently omit writes as well. The Interactions conversion repeats this formula, although its public entry point currently hosts Gemini only.

**Reproduction:** Stored usage of 150 input tokens comprising 20 cache reads, 30 ordinary input, and 100 cache writes becomes **50 input tokens** in both Responses and Managed Agents. With 10 output tokens, their total is 60 instead of 160.

**Impact:** Claude runs under-report token consumption precisely when populating a prompt cache. Cost calculation uses a separate cache-write argument, so this finding does **not** establish that successful-request dollar totals omit the same writes.

**Recommendation:** Use the full stored prompt count for aggregate input, with appropriate fallback for historical records. Preserve the three-way breakdown where the output dialect supports it. For Managed Agents, settle the field semantics explicitly instead of carrying over the Responses formula. Add a cache-write fixture to the parent-facing golden tests.

## 4. Discarded retry reasoning contaminates the final Responses transcript

**Evidence:** [turn.go](../../internal/session/turn.go), lines 355–376; [translate.go](../../internal/stdiosession/translate.go), `Live` and `KindReasoningDelta` at line 267.

The reasoning-starved retry reuses the same live sink and sub-turn identity. The first attempt's reasoning has already streamed; the retry appends more reasoning to the same open output item. Only the accepted attempt's reasoning is persisted.

When the committed reasoning event arrives, the Responses translator trusts the existing live item and closes it without reconciling its content with the authoritative event.

**Reproduction:** Deliver live reasoning `discarded attempt;`, then `accepted attempt`, followed by a committed reasoning event containing only `accepted attempt`. The final assembled output contains:

```text
discarded attempt;accepted attempt
```

**Impact:** The parent transcript differs from the event log and the reasoning replayed to the model. There is no attempt boundary identifying the discarded text. This is primarily a reasoning-integrity issue: the starvation condition requires no answer text in the discarded attempt.

**Recommendation:** Give retry attempts distinct live identities and explicitly mark discarded attempts, or reconcile the final item from authoritative data with a documented replacement event. Simply allocating a new sink is insufficient if the translator still sees the same open item. Extend the starvation fixture to emit actual reasoning and assert the final transcript; the existing fixture tests accounting without emitting first-attempt reasoning.

## 5. Returned Responses snapshots share mutable live content

**Evidence:** [translate.go](../../internal/stdiosession/translate.go), `snapshot` at lines 474–479, `appendText`, and `addUsage`; [jsonrpc.go](../../internal/stdiosession/jsonrpc.go), `dispatch` serializes returned results after the handler returns.

`snapshot()` copies the outer `[]OutputItem`, but each copied item still points at the original `Content` slice. A subsequent text delta modifies its last `ContentPart.Text` in place. The copied usage also retains the shared `Harness` pointer. The lock protects the copy operation, not the later JSON encoding of these references.

**Reproduction:** Stream `before`, obtain a snapshot, then stream `after`. The previously returned snapshot now reads `beforeafter`, without any mutation by its caller.

**Impact:** A `responses.get` result is not a stable snapshot. JSON serialization on the request goroutine can read the same fields the streaming goroutine writes, constituting an unsynchronized access path. The deterministic aliasing probe confirmed shared mutation; a race-detector run was not performed.

The Interactions snapshot uses the same shallow-copy pattern and should be audited with equivalent probes. This report's executed proof is for Responses; it does not claim every nested field in every dialect is mutable.

**Recommendation:** Deep-copy mutable slices and pointer-backed usage metadata under the translator lock, or produce an immutable serialized resource while holding it. Add a snapshot immutability test and a concurrent get/stream test under `go test -race`.

## 6. Claude citations and server-tool metadata never reach the parent

**Evidence:** [anthropic/stream.go](../../internal/anthropic/stream.go), `citations_delta` at line 399 and block rendering; [turn.go](../../internal/session/turn.go), provider-block persistence; [translate.go](../../internal/stdiosession/translate.go) and [managedagentstranslate.go](../../internal/stdiosession/managedagentstranslate.go), `KindReasoningDelta` handlers; [wiretypes.go](../../internal/stdiosession/wiretypes.go), `ContentPart`.

The Anthropic adapter preserves citation objects and server-tool blocks in `ProviderBlocks`. The loop stores those blocks for provider replay. Neither relevant parent translator reads `ProviderBlocks`; they render flattened reasoning and answer text instead. Responses content parts have only type, text, and image URL fields, with no citation representation.

**Trigger:** Claude produces a text block with structured citations after a server-side search/fetch. The text reaches the parent, while the citation objects and their association with that text do not. Server-side tool activity is likewise absent from the normal tool-call output path.

**Impact:** A parent cannot reliably display the source attribution that the provider supplied or explain server-tool activity from the protocol alone. URLs written directly into prose can survive; the finding concerns structured metadata. Provider replay remains intact, so this is output observability loss rather than conversation corruption.

**Recommendation:** Project safe citation metadata into an explicit parent-facing schema and expose server-tool activity where useful. Preserve block-to-text associations. Do not dump encrypted replay blobs or raw thinking signatures into public output merely to make metadata visible. Test an answer with citations through the real Anthropic adapter and the final stdio translator.

## Validation and limitations

Executed five temporary probes against the current implementation:

- `cache_write_omitted`: 150 stored input tokens become 50 in both parent vocabularies.
- `snapshot_aliases_live_content`: a previously returned snapshot changes after another live delta.
- `retry_reasoning_not_reconciled`: accepted and discarded reasoning remain concatenated.
- `length_is_success`: the real Runner returns `ok/no_tool_calls` for partial text plus `length`.
- `usage_before_error_discarded`: received usage becomes nil on the error return.

These probes asserted the observed defects, so their passing results confirm reproduction; they were not tests asserting correct behavior. Temporary probe files were removed after execution.

Also ran:

```text
go test ./internal/stdiosession ./internal/session -run 'TestPipelineReviewProbes|TestGoldenFrames|TestManagedAgentsGoldenFrames|TestStarvedRetry' -count=1
```

Both packages passed. Local Go build/module cache locations were used because the default user cache directories were inaccessible. No full suite, race detector, or live-provider validation was run. Existing golden tests passing does not disprove these findings: their fixtures do not exercise the reproduced inputs.

Cleanup of `.review-go-cache/` and `.review-go-mod/` was rejected by the approval system, so those temporary cache directories remain at the repository root. The only intended deliverable is this report.

## Suggested repair order

1. Preserve incomplete/filtered finish reasons and correct snapshot ownership.
2. Preserve failure-path usage and include Claude cache writes in output accounting.
3. Make retry attempts explicit and reconcile authoritative text.
4. Add citation/server-tool projections and cross-boundary fixtures.

The recurring gap is that individual layers retain information, but the next boundary either drops it or assumes a simpler shape. Tests that begin with provider events and assert the final parent resource are the most useful protection here.

## Resolution

All six findings were addressed on the branch that follows this review. The
tests start from provider events or committed events and assert what the
parent receives (`internal/session/finishusage_test.go`,
`internal/stdiosession/pipeline_test.go`).

1. **Truncation.** `subTurnOutcome` now carries the finish reason. A run whose
   last response made no tool calls ends with reason `truncated` (finish reason
   `length`) or `content_filter` instead of `no_tool_calls`. The Responses and
   Interactions dialects report status `incomplete` for both, and Managed Agents
   reports `stop_reason.type` `truncated` / `content_filter`. The run's own
   session status stays `ok`, since the loop ended cleanly, and the partial text
   still comes back in `harness.text`.
2. **Usage on failure.** `Runner.stream` returns the usage it collected along
   with a stream error, and `commitFailedUsage` writes it as usage events, for
   both a failed first request and a failed starvation retry, without writing
   any reasoning, content or `turn_finished`. `primeDetector` ignores usage from
   a sub-turn that never finished, so a resume's churn baseline is unchanged.
3. **Cache writes.** All three dialects build input from hit + miss + write.
   Responses reports `harness.cache_write_tokens`, Managed Agents reports
   `cache_creation_input_tokens` (and its `input_tokens` semantics are now
   documented), and Interactions reports `harness.cache_write_tokens`. Providers
   that record no writes are unchanged. Input is rebuilt from the three parts
   rather than taken from the stored prompt total, so that providers whose
   stored total is not their hit + miss sum report exactly what they did.
4. **Retry reasoning.** Rather than invent a retraction frame, the committed
   event now overwrites the streamed text of the open item (Responses,
   Interactions), so `output_item.done` and `get` match the log. Managed Agents
   already built its final event from the committed text. A client that
   assembles from deltas replaces the item with the `.done` one.
5. **Snapshots.** The three translators' `snapshot` now deep-copy items, steps
   and events, their argument bytes and usage blocks.
6. **Citations.** `providerblocks.go` projects the stored provider blocks.
   Responses gets `annotations` on `output_text` and a `server_tool_call` item;
   Managed Agents gets per-block `citations` and `agent.tool_use` /
   `agent.tool_result` pairs marked `harness.server_tool`. Encrypted search
   content, citation indices and thinking signatures are not forwarded.
   Interactions hosts Gemini alone and is not changed for this.

Not done: a race-detector run and a citation test through the real Anthropic stream decoder, which is
unexported from `internal/anthropic`. The citation tests use the block shape
`internal/anthropic/stream.go` renders.
