# Adding Gemini 3.7 Flash as a coding model

## 1. What this is

A plan for making `gemini-3.7-flash` a model the agent loop can run on, beside
`deepseek-v4-pro`, `deepseek-v4-flash` and `kimi-k3`. Google describes it as
"our latest and most capable Flash model, built for complex coding, agentic
workflows, and reliable multi-step execution", which is this harness's whole
job description.

This is a bigger cut than [KIMI-INTEGRATION.md](KIMI-INTEGRATION.md) was, and
the reason is structural rather than a matter of effort. Kimi fitted behind a
`Dialect` because Kimi's Chat Completions API is OpenAI-format, as DeepSeek's
is: the client, the SSE reading, the tool-call assembler, the idle watchdog and
the retry classification are all shared, and only "base URL, how reasoning is
requested, how usage maps onto cache hit and miss, how an error body reads, and
which auxiliary endpoints exist" vary (§4.1 there). Gemini shares none of that.
It is a different request shape, a different response shape, a different
streaming envelope, and it carries one concept — the thought signature — that
has no counterpart anywhere in the existing vocabulary.

So Gemini is not a third `Dialect`. It is a second client that produces the
same `wire.Event` stream and satisfies the same `internal/session.Client`
interface.

Two things make this much less daunting than it sounds, and both are accidents
of earlier decisions landing well:

- **`internal/gemini` already speaks the right API.** It was written for the
  vision tools, and it already has the base URL, the auth, the request and
  response structs, the SSE frame vocabulary, the usage accounting and the
  error parsing for `POST /v1beta/interactions`. What it lacks is tools, a
  message history, and an agentic loop — not the transport.
- **`internal/session.Client` is already the seam.** The agent loop states
  intent (`wire.ChatIntent`) and consumes typed events (`wire.Event`). It never
  sees a provider's request shape. Nothing in `internal/session` needs to learn
  the word "Gemini".

## 2. Why the Interactions API, and why not the Go SDK

### The Interactions API is the surface Google now recommends

> "The Interactions API is now generally available. We recommend using this API
> for access to all the latest features and models."
>
> "While it is now considered legacy, the original `generateContent` API
> remains fully supported."
>
> — [Interactions API overview](https://ai.google.dev/gemini-api/docs/interactions-overview)

Google's own documentation now files the `generateContent` pages under "Gemini
Generate Content API (Legacy)". `internal/gemini` targeted `/v1beta/interactions`
already, for unrelated reasons, so this plan inherits the right surface rather
than having to migrate to it.

### Not the official Go SDK

`google.golang.org/genai` is the official SDK and is the obvious thing to
reach for. Three reasons not to, in descending order of weight:

1. **It appears to target `GenerateContent`, not Interactions.** Neither the
   package documentation nor the repository README mentions the Interactions
   API, `interactions.create`, or a steps-based surface; the examples are all
   `Models.GenerateContent()`. Adopting the SDK would mean adopting the legacy
   surface. *(Confidence: medium — this is a documentation absence, not a
   documented absence. Phase 2 confirms it before the decision is locked.)*
2. **Byte-stability is a hard constraint here and an SDK will not promise it.**
   The request head is frozen (ARCHITECTURE.md, "The request head is frozen"):
   two sessions whose serialised arrays differ share no prompt cache, and a
   resumed session whose bytes shifted under it invalidates its own prefix.
   `internal/gemini`'s package comment is explicit that bodies are Go structs
   "never `map[string]any`, so identical values always serialise to identical
   bytes". An SDK that reserialises through its own types, or that reorders a
   map, silently costs money on every sub-turn.
3. **`internal/httplog` needs the transport.** Every provider call is captured
   by wrapping the `http.RoundTripper` (`WithTransportWrapper`). That capture is
   the only record of what the model was actually shown, and the prod
   diagnostics workflow depends on it. An SDK that does not accept an injected
   transport would put Gemini traffic outside the one record that answers "what
   did it actually get sent".

The repo's stated preference settles it: "Prefer the option that exercises a
provider's real behaviour over a provider-agnostic abstraction" (CLAUDE.md).

The SDK is still worth reading as a reference implementation — particularly for
thought-signature handling, which it does automatically and which we must do by
hand.

## 3. How far apart the surfaces are

| Concern | OpenAI-format (DeepSeek, Kimi) | Gemini Interactions |
| --- | --- | --- |
| History | `messages: [{role, content, tool_calls}]` | `input: [Step]`, steps typed by kind |
| Roles | `system` / `user` / `assistant` / `tool` | `system_instruction` is its own field; steps are `user_input`, `model_output`, `thought`, `function_call`, `function_result` |
| Tool schema | `{type:"function", function:{name, description, parameters}}` | `{type:"function", name, description, parameters}` — flattened, no nested `function` object |
| Tool call | `tool_calls[].function.arguments` (a JSON **string**) | `function_call` step with `arguments` (a JSON **object**) |
| Tool result | `{role:"tool", tool_call_id, content}` | `{type:"function_result", name, call_id, result:[Content]}` — result is an array of typed content blocks, so it can carry images |
| Reasoning replay | `reasoning_content` string | `thought` step carrying an opaque `signature` |
| Streaming | `chat.completion.chunk` with `choices[].delta` | `step.start` / `step.delta` / `step.stop` framed as named SSE events |
| Finish | `finish_reason` on a choice | interaction `status` (`completed`, `requires_action`, …) |
| Tool choice | omitted deliberately | `generation_config.tool_choice`: `auto` \| `any` \| `none` \| `validated`, or an `allowed_tools` object. **Nested, not top-level** — sending it top-level is a 400 (`docs/OBSERVED.md`). Still omitted here. |

Three differences are load-bearing and are the ones to get right.

**Tool arguments are an object, not a string.** `wire.ToolCallFunc.Arguments`
is a `string` because that is what OpenAI-format returns and what the executor
parses. Gemini returns an object. The translation must marshal it to a string
on the way in and unmarshal on the way out, and it must do so byte-stably —
which means `json.RawMessage` carried through, not a `map[string]any`
round-trip that would reorder keys.

**Function results are multimodal.** `result` is an array of content blocks,
each with a `type`. That is a direct fit for the harness's existing image
handling: `Read` and the MCP image path already produce an image part when
`SeeImages` is true, and Gemini is a native-vision model, so it should be true
for it (§5.7).

**Thought signatures.** This is the one with no precedent, and §5.2 is about
nothing else.

## 4. Cost

From `configs/prices.json`:

| Model | Cache-hit in | Cache-miss in | Output |
| --- | --- | --- | --- |
| `deepseek-flash` (peak) | 0.006 | 0.3 | 1.2 |
| `deepseek-flash` (off-peak) | 0.003 | 0.15 | 0.6 |
| `kimi-k3` | 0.30 | 3.00 | 15.00 |
| `gemini-3.7-flash` | 0.075 | 0.75 | 3.75 |

Per million tokens, USD. This table predates DeepSeek's 2026-09-10 rate cut
and this harness dropping `deepseek-v4-pro`; re-read `configs/prices.json`
rather than trusting these figures.

Gemini 3.7 Flash lands well under Kimi K3 but is **not** a cost win against
`deepseek-flash`, which is now roughly 3–6× cheaper on output depending on
the hour — a bigger gap than when this was written against DeepSeek Pro's
discounted tier. The argument for Gemini is capability — native vision in
the main loop, and a model Google is positioning specifically at agentic
coding — not price.

Two cost facts that must not be lost:

- **The rates are introductory and double on 2027-01-01** (0.15 / 1.5 / 7.5).
  `configs/prices.json` already records this and notes that nothing in
  `internal/pricing` watches the date. A run costed after that date on the
  current table will be understated by half.
- **Gemini's context caching also carries a per-hour storage charge** with no
  counterpart in the three-rate table — $0.50 per 1M tokens per hour, doubling
  to $1.00 on the same date (`third_party/gemini-docs/pricing.md`).
  `Usage.TokenSplit`'s doc comment already says the shape cannot express it. A
  Gemini cost figure covers cached reads, not cached storage.

## 5. The seams

### 5.1 The `Client` interface is the whole boundary

`internal/session.Client` requires:

```go
StreamChatCompletion(ctx, wire.ChatIntent) (<-chan wire.Event, error)
CreateChatCompletion(ctx, wire.ChatIntent) (*wire.ChatCompletionResponse, error)
UsageSplit(*wire.Usage) (cacheHit, cacheMiss int)
CacheSlack() int
IsReasoningStarved(finishReason, content string) bool
RepairArguments(finishReason, args string) (string, bool)
```

A Gemini client satisfies all six. `CreateChatCompletion` is used only for the
compaction summary, so it can be the unary form of the same translation.
`IsReasoningStarved` and `RepairArguments` are OpenAI-format pathologies —
Gemini streams complete arguments per step and reports status rather than
`finish_reason`, so both are expected to be trivial implementations returning
false / no-repair. **Phase 2 confirms that rather than assuming it.**

### 5.2 Thought signatures are the hard part

On the Interactions surface a thought signature is a field on a `thought` step:

```json
{"type": "thought", "signature": "EpoGCpcGAXLI2nx/..."}
```

([Thinking guide](https://ai.google.dev/gemini-api/docs/thinking#signatures))

The rules, from `third_party/gemini-docs/thinking.md` (Phase 1 mirrored it;
this supersedes an earlier draft of this section that leaned on
`generateContent`-era material found by search):

- **Signatures live in exactly two places, and standard function calls are not
  one of them.** "In the Interactions API, thoughts are a first-class
  representation as dedicated `thought` steps. Because of this signatures are
  limited exclusively to two known locations, `thought` steps, or built-in tool
  steps (like `google_search_call`/`google_search_result`). **They never appear
  on user inputs, model outputs, or standard function calls.**"

  This is a large simplification. The `generateContent` surface attaches
  signatures to arbitrary parts, which is where the widely-reported
  parallel-function-call rule ("only the first `functionCall` part carries
  one") and the 400-on-missing-signature reports come from. **Neither applies
  here.** We use no built-in tools (§8), so for this harness the rule reduces
  to: capture and replay the signature on `thought` steps, and nothing else.
- **They must be replayed verbatim in stateless mode.** "You **MUST** always
  resend all `thought` blocks exactly as they were received from the model."
  You "should **NOT** remove or modify thought blocks from the history, as they
  contain the signatures required for the model to continue its reasoning."
- **A signature is always present; a summary often is not.** The reference
  marks `signature` "Always present, even when the model performs minimal
  reasoning", while a thought block "may contain only a signature with no
  summary" — on simple requests, with `thinking_summaries: "none"`, or for
  non-text thought content. "Your code should always handle thought blocks
  where `summary` is empty or absent." So the signature, not the summary, is
  the thing that must survive the fold.
- **Model switching is explicitly supported.** "When switching models within a
  session, you should still resend the previous model's thought blocks. The
  backend manages compatibility." Worth knowing before anyone proposes
  stripping them on a model change.
- **What the Interactions docs do NOT say.** They state no error code for
  omitting a signature, and they document no bypass values — the
  `context_engineering_is_the_way_to_go` and `skip_thought_signature_validator`
  escape hatches appear only in `generateContent`-era material and forum
  reports. Whether either concept exists on this surface is **unverified** and
  is a Phase 2 measurement, because compaction depends on the answer.

Google's own summary: "The Interactions API makes handling thought signatures
much simpler than the `generateContent` API."

Why this is architecturally awkward rather than merely fiddly: the harness's
authoritative record is its append-only event log, and the messages array is
*derived* from it by `internal/fold`. Anything the model must see again has to
survive that round trip. `wire.Message` has `ReasoningContent` for DeepSeek and
Kimi, but a signature is a different animal — opaque, mandatory, and attached
to a step rather than to a message's prose.

Three consequences, each of which is a phase:

1. The signature must be **captured** from the stream. It arrives as the final
   delta of a thought step, `{"type": "thought_signature", "signature": "..."}`,
   just before `step.stop`.
2. It must be **stored** in the event log, so a resumed session replays it. The
   existing `reasoning_delta` payload is the natural neighbour, but a signature
   is not a delta and must not be concatenated into the reasoning text.
3. It must be **replayed** by the fold into a form the Gemini request builder
   can turn back into a `thought` step — without changing a single byte of what
   DeepSeek and Kimi send, which `internal/wire/request_golden_test.go` pins.

**Compaction is the sharp edge.** Compaction rewrites history into a summary,
which means the replayed history after a compaction contains prose the model
never actually thought, with no signature to attach. That is exactly what the
bypass values are for — if they work on this surface. If they do not, Gemini
sessions may not be able to compact the way DeepSeek ones do, and that is a
finding worth having early rather than at phase eight. **Phase 2 must test it.**

**Phase 8 found this edge does not actually exist for this harness**, and
corrects the paragraph above rather than deleting it — the reasoning here is
sound in general and is exactly what would bite an implementation that
replayed history across a compaction boundary; this one does not.
`internal/session/compact.go` treats compaction as a session boundary, not a
history edit: the retiring session is summarised in prose, and the new
session starts with that prose in its *system prompt*
(`RenderCompactionSummarySystemPromptFor`) and an ordinary short opening
message — no prior `thought`, `function_call`, or `function_result` step is
ever replayed into the new session's history. There is therefore never a
synthetic thought step with a summary the model never produced, for any
provider, and the bypass values this section and §6 describe are not
something Gemini's compaction path needs. See `docs/OBSERVED.md`, "Compaction
— confirmed working, and the plan's 'sharp edge' does not apply", for the
live confirmation.

### 5.3 `ChatIntent` → `InteractionRequest`

```json
{
  "model": "gemini-3.7-flash",
  "system_instruction": "<the frozen system prompt>",
  "store": false,
  "stream": true,
  "input": [ /* steps, in order */ ],
  "tools": [
    {"type": "function", "name": "Read", "description": "...", "parameters": { /* JSON Schema */ }}
  ],
  "generation_config": {"thinking_level": "high"}
}
```

`tool_choice` is deliberately absent, as it is for DeepSeek: `auto` is the
default and is what the agent loop wants. Note it belongs *inside*
`generation_config` if it is ever needed — top-level is a 400, which cost
Phase 2 a wrong conclusion before it was re-measured.

**`store: false` is a deliberate choice**, and it costs something. The default
is `store: true`, which retains interaction objects on Google's side (55 days
paid, 1 day free) and unlocks `previous_interaction_id` — server-side
conversation state, which Google says gives "cost efficiency through improved
context caching in multi-turn conversations".

We should still choose stateless full replay, for three reasons:

- The event log is the only authoritative record of a run. Server-side state
  would make Google's copy authoritative for part of it.
- Resume must work after a process restart, days later. `Runner.Resume` rebuilds
  from the log.
- Compaction rewrites history. There is no way to rewrite an interaction that
  lives on Google's servers.

**The cost of that choice turned out to be much smaller than feared.** Phase 2
measured a 95.5% implicit cache hit (98,269 of 102,911 input tokens) on a
stable ~103K-token prefix under `store: false` with no
`previous_interaction_id`. Google's own wording agrees: "Implicit caching is
supported in both stateful and stateless modes", with
`previous_interaction_id` only making it "more easily" utilised. So stateless
replay does not forfeit caching, and the main economic objection to this
decision is answered.

One operational caveat falls out of it: **the cache warms asynchronously over
more than one request.** Request 2 of a byte-identical prefix still missed
completely; only request 3 hit. Anything measuring Gemini cache behaviour —
`CacheSlack` in Phase 8 especially — must discard the first two requests or it
will measure the warm-up and report it as the steady state. DeepSeek hits on
request 2, so this is a genuine behavioural difference, not a tuning detail.

**Phase 8 found the warm-up runs longer than this measurement implied**, in a
live, incrementally-growing session rather than a byte-identical prefix
resent unchanged: both of its measured sessions took five to six sub-turns
of complete misses before the first hit, not two. It also found the cache
can drop to a complete miss again mid-session, well after warm-up, on a
request proven byte-identical in its shared prefix to the one before it —
twice in 39 combined sub-turns, with no harness-side cause. See
`docs/OBSERVED.md`, "`CacheSlack` — measured", for the numbers and what they
mean for the constant.

`effort` maps onto `generation_config.thinking_level`
(`minimal` / `low` / `medium` / `high`), not onto `reasoning_effort`. The
existing `ThinkingLevel*` constants in `internal/gemini` already cover it.

**No `temperature`, `top_p` or `top_k`** — `internal/gemini/types.go` already
records that Gemini 3.x must not receive them.

### 5.4 Steps → `wire.Event`

The stream vocabulary, measured and documented
([streaming guide](https://ai.google.dev/gemini-api/docs/interactions/streaming)):

```
event: step.start
data: {"index":0,"step":{"type":"function_call","id":"un6k8t18","name":"get_weather","arguments":{}},"event_type":"step.start"}

event: step.delta
data: {"index":0,"delta":{"type":"arguments_delta","arguments":"{\"location\": \"San Francisco, CA\"}"},"event_type":"step.delta"}

event: step.stop
data: {"index":0,"event_type":"step.stop"}
```

Delta types: `text`, `thought_summary`, `thought_signature`, `arguments_delta`,
`image`.

**Function-call arguments arrive as incremental fragments and must be
concatenated** — "You must accumulate these deltas to get the full arguments".
That is precisely what `wire.ToolCallAssembler` already does for OpenAI-format
deltas, so it should be reusable rather than reimplemented; confirm the index
keying matches.

`internal/gemini/stream.go` already handles `interaction.created`,
`step.start`, `step.delta` (text and thought_summary), `interaction.completed`
and `done`. It must gain `step.stop`, `error`, and the three delta types it
currently ignores — its own comment already admits it skips "tool-call and
image deltas this client never asks for".

### 5.5 Usage and cache accounting

`gemini.Usage.TokenSplit()` already maps onto the harness's three-rate shape
and its reasoning here are documented and sound: thought tokens bill at the
output rate, `total_cached_tokens` is a subset of `total_input_tokens`.

`CacheSlack()` is an empirical bound on a provider's over-prediction (127 for
DeepSeek, 512 for Kimi). Gemini's is **unknown and must be measured**, not
guessed. Until Phase 8 measures it (this plan has eight phases, not nine —
an earlier draft of this paragraph miscounted), a deliberately loose value
with a comment saying it is provisional is the honest placeholder. **Phase 8
has now measured it**; see §7 Phase 8's "Done" note and
`docs/OBSERVED.md`.

Note `step.stop` carries a per-step `step_usage`, which the OpenAI-format
providers have no equivalent of. Not required, but it would make per-tool-call
cost attribution possible for the first time — noted as a future opportunity,
out of scope here.

### 5.6 Byte stability

`internal/wire/request_golden_test.go` and `request_parts_golden_test.go` pin
the serialised request bytes. Every change to `internal/wire` in this plan must
leave DeepSeek's and Kimi's golden files untouched. Any new field is
`omitempty` and unset for them. If a golden file has to change, that is a
signal the change is in the wrong place — put it in the Gemini package instead.

### 5.6a What the Interactions API will accept in a tool's JSON Schema

Measured against the live API, `parameters` is read far more loosely than
Google's `Schema` proto suggests. All of these are accepted: `$schema`,
`$ref` with `$defs`, `additionalProperties`, `const`, `oneOf`, `allOf`,
`anyOf`, `default`, `format`, `examples`, `prefixItems`, a `type` union
including `"null"`, property names that are not identifiers (`-A`, `-i`), and
keywords the API has never heard of.

One construct is refused — JSON Schema's tuple form, where `items` holds an
array of per-position schemas rather than one schema for every element:

```json
{"type": "array", "items": [{"type": "number"}, {"type": "number"}]}
```

The answer is `400 invalid_request: Invalid JSON payload: syntax error in
request body.` It names no tool, no field and no schema, and one offending
declaration invalidates the entire payload — every other tool in the request
goes down with it and the session dies on its first request, before a single
token is generated.

`internal/gemini/schema.go` rewrites the tuple into the single-schema form
and restores the arity as `minItems`/`maxItems`. Members that agree collapse
to the one schema; members that disagree become an `anyOf` over the distinct
ones, which is weaker — it no longer says which member belongs in which
position — but is the closest this surface can express.

Nothing else is stripped. A pass that removed everything outside the `Schema`
proto would rewrite schemas the API accepts, cost their authors' meaning, and
move the frozen prefix for no gain.

The lowering runs once, in `internal/session/lifecycle.go`, where the tool
array is resolved and frozen, so the row, the head and every request all
carry the same bytes. It is not in `toolsFromWire`: that path carries
`parameters` through as raw bytes on purpose (§5.6, docs/DESIGN.md §3.2), and
a per-request rewrite would be the map round trip that rule exists to
prevent.

An MCP server is where a tuple arrives. Its tools are declared by whoever
wrote the server, against no constraint this process imposes. A CAD
session died on one `z.tuple([number, number, number])` in `design_render`,
which took every other tool in the request down with it.

### 5.7 Vision: `seesImages` becomes true

`internal/session/runner.go`'s `seesImages()` is currently true only for Kimi.
Gemini is natively multimodal, so it becomes true for `gemini-3.7-flash` too.
That flips two things automatically, both already built: which tool array the
session sends (`tools.DefinitionsFor`) and whether `Read` and the MCP image
path return an image part (`tools.Executor.SeeImages`).

This is the main capability argument for the whole exercise. Today a DeepSeek
session that screenshots a page has to write the image to disk and ask Glance
to describe it, then reason about the description. A Gemini session sees the
image.

The image part shape differs — `wire.Part`/`ImageURL` is a data URI in
OpenAI-format, where Gemini wants `{"type":"image","mime_type":...,"data":...}`
with an optional `resolution`. The translation lives in the Gemini request
builder.

## 6. Decisions

### Settled

- Interactions API, not `generateContent`. Google calls the latter legacy.
- Hand-rolled client extending `internal/gemini`, not `google.golang.org/genai`.
- `store: false`, stateless full replay. The event log stays authoritative.
- `seesImages()` true for Gemini.
- No `temperature` / `top_p` / `top_k`.
- Thought signatures are stored in the event log, not held in memory. A run
  that cannot be resumed is not a run this harness supports.

### Resolved by Phase 2's measurements

Full write-up in `docs/OBSERVED.md`, "Gemini 3.7 Flash — Interactions API".

- **Go SDK lacks Interactions.** Confirmed by inspecting the exported API:
  one symbol contains "Interaction" (`InteractionStatus`, a bare enum) and
  there is no interactions service or `CreateInteraction`. §2's decision holds.
- **Signatures never ride on function calls.** Three parallel `get_weather`
  calls produced one `thought` step carrying the only signature, then three
  signature-less `function_call` steps. §5.2's reading confirmed.
- **Both bypass values work.** `context_engineering_is_the_way_to_go` and
  `skip_thought_signature_validator` each returned 200 with a valid answer when
  substituted for a real signature. This was read at the time as settling
  "compaction is possible for Gemini sessions" — true as a statement about
  the API, but Phase 8 found this harness's own compaction never needed to
  ask the question: it forks a fresh session with the summary in the system
  prompt rather than replaying a synthetic thought step into history, so no
  code path here ever constructs one of these bypass values. They remain
  correct and interesting facts about the Interactions API; they are not
  wired into anything in this repository.
- **Two distinct signature errors.** A missing or empty signature gives
  "Request contains an invalid argument."; a garbled one gives "Corrupted
  thought signature." Worth distinguishing — the first is a harness bug, the
  second is corruption in the log.
- **`arguments_delta` never fragmented**, from 33 bytes to 69 KB across six
  calls. `wire.ToolCallAssembler` is still the right thing to use (one frame is
  a degenerate case of several) but must not *rely* on fragmentation, and the
  streaming guide's "you must accumulate" wording overstates what happens.
- **Implicit caching works stateless** — 95.5% hit on a 103K prefix — but warms
  over more than one request. See §5.3.
- **Context limit ≥ 1,000,011 input tokens**, measured and billed. The true
  ceiling and any output ceiling remain unknown; no request field was found to
  probe for an output cap.
- **`IsReasoningStarved` / `RepairArguments`** found no work to do: zero
  malformed arguments observed, and no `generation_config` field exists to cap
  output and provoke starvation. Absence of evidence, not proof — implement
  them as honest no-ops with a comment saying so.
- **`CacheSlack`** still deferred to Phase 8, now with a method: discard the
  first two requests.

Two findings contradict the plan and are corrected in place above — the
`tool_choice` nesting (§3, §5.3) and the caching cost (§5.3). Two more change
Phase 4's work and are recorded here:

- **Errors can arrive SSE-framed even when the HTTP status is 400**:
  `event: error\ndata: {"error":{...}}`. `internal/gemini/client.go`'s
  `parseAPIError` expects a bare JSON body and will mis-handle this. Phase 4
  must parse both shapes.
- **`status` is always `"completed"`**, never `"requires_action"`, even
  mid-turn with an unanswered `function_call` pending. §3's table implied
  otherwise. A client must decide "the model wants a tool call" by scanning
  steps for a pending `function_call`, never by branching on `status`.

## 7. Phased plan

Each phase is independently reviewable and leaves the tree building and
`scripts/test.sh` green.

### Phase 1 — Vendor the documentation

Mirror the Gemini Interactions documentation into `third_party/gemini-docs/`
as Markdown, matching how `third_party/deepseek-docs/` and
`third_party/kimi-docs/` are organised, with a `README.md` index. Google serves
raw Markdown at `<page-url>.md.txt`, so this is a fetch-and-organise job.

Pages that matter: interactions overview, the interactions API reference,
function calling, streaming, the thinking guide (signatures), tool combination,
models, pricing.

No Go code. Update `CLAUDE.md`'s "Vendored documentation" section.

**Done.** 19 Markdown pages plus `openapi.json` — the OpenAPI 3.0.3 description
of `v1beta`, 14 paths and 181 schemas. That file is the authoritative reference
for Phase 4's struct definitions; prefer it over the prose pages wherever they
disagree about a field name or type. Phase 1 also corrected §5.2 of this
document, which had been drafted from `generateContent`-era material.

### Phase 2 — Live-API spike, recorded in `docs/OBSERVED.md`

**The point of this phase is that nothing downstream guesses a shape.**

This follows the repo's existing convention rather than inventing one:
"Nothing in either suite calls `api.deepseek.com`. Findings that needed the
live API were measured by hand and written down in `docs/OBSERVED.md` rather
than turned into tests" ([TESTING.md](../TESTING.md)). So the probe itself is
a throwaway under `scripts/` or the scratch directory and is **not committed
as a test**. What gets committed is (a) the findings, in `docs/OBSERVED.md`,
and (b) the captured SSE frame sequences as fixtures under
`internal/gemini/testdata/`, which Phase 4's unit tests then run against.

Exercise against the live API:

1. A tools request with one function, streamed, to a completion.
2. The full `function_call` → `function_result` round trip over two requests
   with `store: false` and manual history replay.
3. A thought step: capture the signature, replay it, confirm success.
4. **Omit the signature and record the exact error.** This is the phase's most
   important single measurement.
5. Try both bypass signature values and record whether they work.
6. Parallel function calls: confirm they arrive as several `function_call`
   steps in one turn, that none carries a signature (§5.2), and record the
   step indices and ordering the assembler depends on.
7. An image in a `function_result`.
8. Capture the raw SSE frame sequence for each.

Record every finding in `docs/OBSERVED.md`, which "overrides the vendored docs
where they disagree", and resolve every item in §6 "Open". Where a finding
contradicts this document, **update this document**.

The dev stack holds `google.api_key` (there is no `GOOGLE_API_KEY` in `.env`).
The probe must read it from that settings store or take it from the
environment, and must never print it — a key pasted into a findings document
outlives the investigation.

**Done.** Every item above resolved, three findings contradicting the plan
(`tool_choice` nesting, caching under `store: false`, `status` never
reporting `requires_action`), all recorded in §6 and in `docs/OBSERVED.md`,
"Gemini 3.7 Flash — Interactions API". SSE fixtures landed under
`internal/gemini/testdata/`.

### Phase 3 — `wire` grows a home for thought signatures

Add what Gemini needs to `internal/wire` without moving a byte for the others:

- A field on `wire.Message` carrying an opaque provider signature, `omitempty`.
- Whatever `wire.Event` needs to carry a signature delta.

Golden tests for DeepSeek and Kimi must pass **unchanged**. Add a golden test
asserting that a message with no signature serialises byte-identically to one
from before this phase.

**Done.** `wire.Message.ThoughtSignature` and `wire.EventThoughtSignatureDelta`
landed following the existing `ReasoningContent`/`EventReasoningDelta`
pattern — carried verbatim, never concatenated. DeepSeek's and Kimi's golden
files are untouched.

### Phase 4 — The Gemini agentic client

`internal/gemini` gains the `session.Client` implementation: `ChatIntent` →
`InteractionRequest`, the streamed steps → `wire.Event`, `UsageSplit`,
`CacheSlack`, `IsReasoningStarved`, `RepairArguments`.

Extend `stream.go` for `step.stop`, `error`, `arguments_delta`,
`thought_signature` and `image` deltas. Reuse `wire.ToolCallAssembler` if
Phase 2 showed it fits.

The existing vision path (`Interact`) must keep working untouched — it is in
production and `internal/tools/vision.go` depends on it.

Unit tests against recorded fixtures from Phase 2, not against live.

**Done.** `internal/gemini` satisfies `session.Client` in full. Two
corrections against Phase 2's live measurements landed on the way:
`max_output_tokens` is real and is now sent (an earlier failed search had
recorded its absence as fact), and a capped interaction's `status:
"incomplete"` is Gemini's spelling of `finish_reason: length`, so
`IsReasoningStarved` matches DeepSeek's semantics rather than being the
no-op the plan expected. Retry-with-backoff stays absent, as §8 already
recorded. The vision path (`Interact`) is untouched.

### Phase 5 — Routing and configuration

- `internal/provider`: `"gemini-3.7-flash": Gemini`.
- `cmd/harness`: construct the client, wire the key and the transport wrapper
  for `internal/httplog`.
- `internal/settings`: whatever key the coding path needs, distinct from
  `google.vision_model`.
- `configs/prices.json`: already has `gemini-3.7-flash`. Verify the shape the
  cost lookup expects matches.
- `harness models` lists it.
- Queue validation accepts it.

**Done.** Provider table entry, client construction with the `httplog`
transport wrapper, queue validation, `harness models` (listing out of the
static provider table, since Interactions has no live models endpoint — see
`docs/MODELS.md`), and a pricing coverage test. One latent bug fixed on the
way: `model.judge` set to `gemini-3.7-flash` would have silently run the
eval judge against DeepSeek's client instead of erroring. **No
Gemini-specific run budget** — a deliberate call, not an oversight: Gemini's
rates sit at or below DeepSeek Pro's standard tier, unlike K3's 7-17×, so the
global defaults were judged to already tolerate it. That reasoning lived
only in this phase's commit message until Phase 8 pulled it into
`docs/OBSERVED.md` and `docs/MODELS.md`, correcting §7 Phase 7's text below,
which still listed a Gemini run budget as in scope two phases later.
`seesImages` stayed false for Gemini until Phase 7.

### Phase 6 — Fold and resume

`internal/fold` replays thought signatures into the messages array; the event
log stores them. `TestAppendOnly` must still hold. Add a test that a session
folded from events produces a request the Gemini builder turns into steps
carrying every signature in the right order.

**Done.** A signature survives capture, storage, fold and replay:
`turn.go` records `EventThoughtSignatureDelta` onto the existing
`ReasoningDeltaPayload` rather than a new event kind (no display content of
its own, so no case needed in either fold), and `Fold` sets it on the
sub-turn's assistant message — recorded whenever present even when the
sub-turn produced no reasoning text, which is the ordinary Gemini case since
a signature is always present but a summary often is not.
`TestFoldToGeminiRequestCarriesSignatures` pins the whole chain. Old rows
without the field still decode; `TestAppendOnly` and the wire goldens hold.

### Phase 7 — Vision split and system prompt

`seesImages()` true for Gemini. Image parts translated into Gemini's image
content shape. A Gemini system prompt if Phase 2 or 9 shows it needs one —
Gemini 3.x wants concise prompts and reacts badly to chain-of-thought
scaffolding written for older models (`docs/gemini-3.5-flash-ui-review-prompting.md`).
~~Run budgets for the model, as `run.compaction_threshold_kimi_k3` does for K3.~~ Already
decided against in Phase 5 (see that phase's "Done" note above) — this line
was stale by the time this phase ran and stays here struck through rather
than silently deleted, since it is what Phase 8 caught and corrected.

**Done.** `seesImages()` true for Gemini; `definitionsKimi` became
`definitionsVisionCapable` since the array is now shared by two providers,
byte-identical, so the goldens are reused rather than duplicated. No Gemini
system prompt: `renderSystemPromptFor` was already a pure function of the
tool array and the `seesImages` capability with no provider baked in, so
Gemini gets a truthful head automatically, with none of the
chain-of-thought scaffolding Gemini 3.x reacts badly to. Tool-result image
resolution is left unset, unlike the vision tools' first-high/rest-medium
policy, since a `Read` or MCP result carries one ad hoc image with no batch
to rank.

### Phase 8 — End to end

Run a real session on `gemini-3.7-flash` through the dev stack. Measure
`CacheSlack` and replace the provisional value. Measure the cache-efficiency
cost of `store: false`. Confirm compaction works or record precisely why it
cannot. Update `docs/OBSERVED.md`, `docs/MODELS.md`, `ARCHITECTURE.md` and
`internal/CLAUDE.md`.

**Done.** Two substantial live sessions against this repository through the
dev stack (`readonly`, real Read/Grep/Glob/List/Task* tool use, multiple
sub-turns, real thought steps with signatures on every one) plus a
throwaway compaction stress probe, all captured end to end in
`internal/httplog` and verified against the trace, not just the unit test.
`CacheSlack` stays 8192 — the number does not move, but its status does,
from a guessed placeholder to a measured one, with two live-session findings
that revise §5.3's warm-up guidance and this section's own "sharp edge"
worry about compaction (see §5.2 and §5.3's corrections above, and
`docs/OBSERVED.md`, "Phase 8 — end to end on the dev stack", for the full
numbers). Compaction confirmed working live, six clean repetitions in one
forced-every-sub-turn stress run, and found not to need the thought-signature
machinery this document worried about at all. `docs/OBSERVED.md`,
`docs/MODELS.md`, `ARCHITECTURE.md`, and `internal/CLAUDE.md` (indirectly,
via the codemap entries already current) are updated; `internal/gemini`'s
`CacheSlack` doc comment carries the measurement.

## 8. What this plan does not cover

- ~~**Retry with backoff on the Gemini agentic path.**~~ Closed.
  `internal/providerhttp.Transport` gained a `SetAuth` field —
  `func(req *http.Request, apiKey string)`, nil meaning "keep sending
  `Authorization: Bearer <key>`" — so a provider whose credential rides on a
  different header can still take `Transport.Do`'s retry-with-backoff without
  DeepSeek or Kimi changing a byte of what they send (pinned by
  `TestSetAuthNilKeepsBearerDefault` and the existing header-assertion tests
  in both packages). `internal/gemini`'s `chatTransport` sets it to
  `x-goog-api-key` plus the `Accept: text/event-stream` header
  `StreamChatCompletion`, `CreateChatCompletion`, and `Interact` all send;
  `Retryable` is `retry.go`'s `isRetryableStatus`, retrying 429/500/503 —
  DeepSeek's own set, since `third_party/gemini-docs/` documents no error
  behaviour at all for `/v1beta/interactions` (`openapi.json` lists only the
  200 response on every operation) and 504 was left out rather than guessed
  at, unlike Kimi's documented 900-second gateway timeout. `PumpStream` is
  still not shared — it decodes `wire.ChatCompletionChunk`, the OpenAI shape,
  and Gemini's step-typed SSE frames have no counterpart in it — so
  `stream.go`'s `pumpChatEvents` stays this package's own reader, now started
  after `Transport.Do` has already retried its way to a response.
  `Interact` moved onto the same `chatTransport` too: its request bytes are
  untouched (`Transport.Do` forwards the `[]byte` `json.Marshal` produced
  without reading it), its headers are unchanged byte-for-byte
  (`TestInteractHeadersUnchangedAfterTransportMigration`), and it sets no
  response-header timeout either, because `chatTransport.HTTPClient` is the
  same `*http.Client` `Interact` always used, not a fresh one — so a
  Glance/Ground/Detect call now survives a transient 5xx the same way a
  coding sub-turn does. `internal/gemini/client.go`'s `newRequest` and
  `wrapClientError`, both now unreachable, were removed rather than left as
  dead code beside `chatTransport`.

- `previous_interaction_id` and server-side state, as an optimisation.
- Per-step usage attribution from `step.stop`.
- Gemini's built-in tools — `google_search`, `code_execution`, `computer_use`,
  `url_context`, `file_search`, `retrieval`, `google_maps`, and Gemini's own
  `mcp_server` type. Each one is its own argument about what a coding session
  should be allowed to reach, and that argument is now made once in
  [GEMINI-BUILTIN-TOOLS.md](GEMINI-BUILTIN-TOOLS.md) rather than left open
  here. The short version: five of the nine execute on Google's servers and so
  cannot pass through the permission gate at all, `google_search` is the only
  one that adds something the harness does not already have, and adopting it
  turns on `validated` tool choice for every request and needs a
  terms-of-service question answered by a person. Still out of scope for this
  plan; no longer unexamined.
- Audio, video and document content types.
- Retiring `Glance`/`Ground`/`Detect` for Gemini sessions. They stay; a session
  that can see images natively may still want a cheap second opinion, and the
  tools are shared with DeepSeek sessions regardless.
- The 2027-01-01 price doubling. Still unwatched, still out of scope, now
  recorded in two places.
