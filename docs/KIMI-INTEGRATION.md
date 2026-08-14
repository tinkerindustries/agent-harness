# Adding Kimi K3 to the harness

How a second model provider fits, which seams carry it, and what has to be
decided before any of it is built. The vendored reference is
[`third_party/kimi-docs/`](../third_party/kimi-docs/README.md); the API facts
below cite it rather than restating from memory.

## 1. What this is

The harness runs one provider today. `internal/deepseek` is both the API client
and the wire vocabulary every other package speaks: `internal/fold` folds the
event log into `[]deepseek.Message`, `internal/session` builds
`deepseek.ChatCompletionRequest`, `internal/evals` uses the client for its
judge. Nine packages import it.

Adding Kimi K3 means separating those two jobs. The vocabulary is shared; the
client is not.

## 2. How close the two APIs already are

Kimi's Chat Completions API is OpenAI-format, as DeepSeek's is. The overlap is
larger than it looks, and three existing decisions land on the right side of it
by accident:

- **`reasoning_effort` uses the same words.** K3 takes a top-level
  `reasoning_effort` of `low` / `high` / `max`, default `max`
  ([reasoning effort](../third_party/kimi-docs/guide/use-reasoning-effort.md)).
  `internal/deepseek/types.go` already declares exactly those constants and
  already sends the field.
- **The fold already replays reasoning.** K3 runs with Preserved Thinking
  always on and requires each historical assistant message to carry its
  `reasoning_content` back
  ([thinking models](../third_party/kimi-docs/guide/use-thinking-models.md)).
  `internal/fold/fold.go:41` sets `ReasoningContent` on every assistant message
  it emits, so the harness already produces the shape K3 wants.
- **The price table is already multi-provider.** `configs/prices.json` carries
  Gemini entries beside the DeepSeek ones, and `pricing.ModelPrices` already
  splits cache-hit from cache-miss input.

What differs:

| Concern | DeepSeek | Kimi K3 |
| --- | --- | --- |
| Base URL | `https://api.deepseek.com` | `https://api.moonshot.ai/v1` |
| Reasoning control | `thinking: {type}` | `reasoning_effort`; sending `thinking` is an error |
| Non-thinking mode | supported | not available — K3 always reasons |
| Usage cache fields | `prompt_cache_hit_tokens` / `_miss_tokens` | `cached_tokens` only |
| `tool_choice` | rejected under thinking | `auto` / `none` / `required` |
| Message content | string | string, or an array of parts for images |
| Sampling params | not sent | must not be sent; fixed server-side |
| Balance endpoint | `/user/balance` | `/v1/users/me/balance` |
| Rate limiting | per-model concurrency | account-wide RPM/TPM tiers |

Two further Kimi behaviours matter to the cache discipline. A prefix is only
cached when the previous request's prompt exceeded 256 tokens, and changing
`reasoning_effort` mid-conversation invalidates the prefix
([tool calling best practice](../third_party/kimi-docs/guide/kimi-k3-tool-calling-best-practice.md)).
Changing `tool_choice` does not. The harness already fixes effort for a
session's life, so it satisfies this.

## 3. Cost, which shapes the defaults

Per million tokens, from `configs/prices.json` and
[K3 pricing](../third_party/kimi-docs/pricing/chat-k3.md):

| | cache hit | cache miss | output |
| --- | --- | --- | --- |
| `deepseek-v4-pro` | $0.003625 | $0.435 | $0.87 |
| `kimi-k3` | $0.30 | $3.00 | $15.00 |

K3 costs roughly 7× more on a cache miss and 17× more on output. The
cache-hit-to-miss ratio is 10× rather than DeepSeek's 120×, so the frozen-head
discipline still pays but protects less of the bill.

The practical consequence: K3 cannot inherit the DeepSeek run budget. A run
that is affordable at `deepseek-v4-pro` output rates is not affordable at K3's.
`run.max_sub_turns` and `run.compaction_threshold` need per-model resolution,
or K3 needs its own lower ceilings.

## 4. The seams

### 4.1 Wire vocabulary out, dialect in

Move the provider-neutral types out of `internal/deepseek` into a new package —
`Message`, `Tool`, `ToolCall`, `Usage`, the chunk types, the SSE scanner, the
tool-call assembler, `Event`. Both providers produce and consume them
unchanged. `internal/fold` folds into that package's `Message`, and its
append-only property is untouched.

What stays provider-specific is small enough to name: base URL, how reasoning
is requested, how usage maps onto cache hit and miss, how an error body reads,
and which auxiliary endpoints exist. That set is a `Dialect` — one interface,
two implementations, chosen by model name.

The client itself is shared. It is the same HTTP shape, the same SSE reading,
the same idle watchdog, the same retry classification.

This is the seam the repo's architecture already favours: a narrow declared
interface rather than a second copy of the loop. It matches how `RunPublisher`
and `RunController` were cut.

### 4.2 Byte-stability is the constraint on doing it

The head of every request is frozen and shared (`docs/DESIGN.md` §3.2,
[`docs/CACHE.md`](CACHE.md)). Moving types between packages must not move a
single byte of the serialised output. `encoding/json` emits struct fields in
declaration order, so the guard is: preserve field order exactly, and pin it
with a golden test that captures a full request body before the move and
asserts it byte-for-byte after.

That test is worth writing first. It makes the whole extraction verifiable
rather than argued.

### 4.3 Routing a model to a provider

`queue.Request.Model` already carries the model per request, and
`Runner.ModelLimits` is already keyed by model. Nothing new is needed on the
wire. What is needed is one table mapping model name to provider, held in a
single place, so `kimi-k3` resolves to the Kimi dialect and `deepseek-v4-pro`
to the DeepSeek one. A table rather than string-prefix matching, so an unknown
model fails loudly at validation instead of silently choosing a default.

### 4.4 The system prompt

The prompt is frozen per build and stored per session, and a resumed session
replays the prompt it was created with. A Kimi-specific prompt is therefore a
second frozen head, not a mutation of the first.

`internal/promptvariant` already exists for exactly this: named alternatives to
the shipped prompt, validated on the work request, with `internal/session`
owning the text. A Kimi prompt starts as a variant.

That also means it can be measured. `harness eval` runs a suite under two named
variants and compares what the sessions did
([`docs/EVALS.md`](EVALS.md)). The question "does K3 need different wording"
has a machine answer here rather than an opinion, and the machinery is already
built. Kimi publishes its own
[prompt guidance](../third_party/kimi-docs/guide/prompt-best-practice.md)
to draw candidate wording from.

One caveat: the prompt names its tools and says "All nineteen are always
available" for DeepSeek, "All fourteen" for Kimi. Any prompt variant has to
keep that inventory truthful.

### 4.5 Vision

K3 reads images natively; the DeepSeek models do not. Kimi sessions take the
multimodal path, and none of `Screenshot`, `Glance`, `Ground`, `Detect`, or
`Crop` is offered to them.

Today no image reaches any model but Gemini's. `Screenshot` captures a PNG to
disk and returns prose about it — dimensions, page title, what the viewport
clipped. `Glance`, `Ground`, and `Detect` ship the file to Gemini and return a
prose answer, a located pixel box, or a numbered inventory of boxes;
`Crop` cuts one out locally, with no model call at all. The model reads
descriptions of, or coordinates in, pictures it never sees
([`TOOLS.md`](TOOLS.md), "Glance" and "Ground and Detect").

For Kimi that inverts. `Message.Content` gains a parts representation, and
`Read` returns an `image_url` part when the path is an image and the provider
can see one. Capture happens through Bash and the `playwright-cli` skill the
repo already ships; `Read` is what puts the result in front of the model.

Two facts from
[`openapi.json`](../third_party/kimi-docs/openapi.json) make this work:

- **Parts are legal on any role**, `tool` included. `Message.role` and the
  `oneOf[string, array]` content sit on the same object, so a tool result can
  carry an image without a synthetic user message after it. This was the design
  risk; the schema removes it. Confirm it against the live API in Phase 6
  before Phase 7 depends on it.
- **Parts carry `text`, `image_url`, and `video_url`.** K3 reads video as well
  as stills.

Byte-stability holds through a marshaller that emits a bare string whenever the
parts slice is empty, so every message the harness sends today serialises
exactly as it does now. That needs its own golden test.

Kimi accepts images as base64 data URIs or uploaded file IDs, and rejects plain
URLs and SVG.

The cost of dropping `Screenshot` is real and worth naming: a Kimi session
loses scripted capture with its scale, colour-scheme, selector, and full-page
options, and has to drive `playwright-cli` through Bash instead.

## 5. Decisions

### Settled

1. **The harness is no longer DeepSeek-only.** `CLAUDE.md`'s preamble is
   rewritten in Phase 1 to say the harness runs coding agents against a
   provider's own API, with DeepSeek the default and Kimi added behind a narrow
   dialect seam. The spirit survives: two named dialects, no plugin system, no
   third provider without a reason.

2. **The Go module path does not change.** Renaming it while the repository
   keeps its name breaks `go get` and touches every import for nothing.

3. **The MCP tool names do not change.** `deepseek_agent` and its siblings are
   what external callers use, and the installed `deepseek-flash-*` skills call
   them by name. Renaming them is a coordinated break, independent of this
   work.

4. **`$HOME/.deepseek-harness` and the compose project names do not change.**
   The worktree port registry lives in that directory and `deepseek-harness-prod`
   is a live stack. Either rename needs a migration and an operator window.

5. **Kimi sessions get their own tool array**, without `Screenshot`,
   `Glance`, `Ground`, `Detect`, or `Crop`. K3 sees images directly, so the
   Gemini round-trips and the capture tool are all redundant for it.

6. **A second frozen head is now a supported thing, and nothing enforces it —
   until Phase 8.** Decision 5 means the tool array varies by provider, which
   the architecture permits but had never had to hold. Until Phase 8,
   "permission changes which tool calls run, never which tools are offered"
   made the array a single constant. Phase 8 settled the guard: **a golden
   file per provider** (`internal/tools/testdata/tools_deepseek.golden.json`,
   `tools_kimi.golden.json`), both asserted by one table-driven test
   (`TestToolArrayGolden`). Two files rather than one, so a failing assertion
   names the provider whose head moved; one test, so the two assertions
   cannot drift apart. The DeepSeek golden was captured from the pre-Phase-8
   array and must keep passing byte-for-byte; Kimi's pins the new array.

## 6. Phased plan

Each phase leaves the system working and is independently revertible.

### Phase 1 — Rename the product identity — done

Landed as PR #80, squashed onto the integration branch as `b1ca077`. 13 files,
27 insertions. Every protected identifier verified intact afterwards.

It surfaced Phase 2: `scripts/test.sh` cannot run from inside an agent
container, so the run verified itself by hand instead.

### Phase 2 — Make the test script work from inside a container

Every remaining phase is a Go change verified by an agent running in the
harness image, and none of them can run `scripts/test.sh` as it stands. The
script starts its broker with `docker compose`, published on the host's
loopback, which the container cannot reach. `TESTING.md` documents the trap and
prescribes a local `nats-server` instead — but that binary is not in the image,
so the Phase 1 agent installed it mid-run and its workaround does not
reproduce.

Two parts: bake `nats-server` into the Dockerfile beside `gh`, and teach
`scripts/test.sh` to start it directly when the compose broker is unreachable,
keeping the compose path for a normal host run.

Verify: `scripts/test.sh` green on the host, and green from inside the
container.

### Phase 3 — Extract the wire vocabulary

Write the golden request-body test first. Then move the neutral types, SSE
scanner, and tool-call assembler out of `internal/deepseek` into the shared
package, leaving the DeepSeek client behind it. Update the nine importing
packages. No behaviour changes, no Kimi yet.

Verify: the golden test passes unchanged, `scripts/test.sh` green, and a live
DeepSeek run shows the same cache-hit ratio as before via `internal/cache`.

### Phase 4 — Introduce the Dialect seam

Define the interface, implement it for DeepSeek, and change nothing else.
Reasoning control, usage mapping, error parsing, and the auxiliary endpoints
move behind it. Add the model→provider table with one entry.

Verify: as Phase 3. The seam is proven when DeepSeek still works through it.

### Phase 5 — The Kimi client

Second `Dialect`: base URL, `reasoning_effort` instead of `thinking`,
`cached_tokens` mapped onto cache hit with miss derived as
`prompt_tokens - cached_tokens`, Kimi's error body, `/v1/users/me/balance`.
Add `kimi.api_key` to the settings registry and `kimi-k3` to
`configs/prices.json`. Register the model.

Verify: `harness models` and `harness balance` against Kimi, then
`harness ask` on `kimi-k3`. No agent loop yet.

### Phase 6 — Run a session on K3

Route a full session through the Kimi dialect. Confirm streamed reasoning and
tool-call deltas assemble correctly, that Preserved Thinking replay is accepted
across sub-turns, and that the prefix cache actually hits — Kimi's 256-token
floor and the effort-switching rule both need observing rather than assuming.
Set K3's own `run.max_sub_turns` and compaction ceiling per §3.

Verify: a live multi-tool run end to end, `internal/cache` reporting hits, and
recorded cost matching the price table. Findings go in
[`docs/OBSERVED.md`](OBSERVED.md).

### Phase 7 — Multimodal content parts

`Message.Content` gains a parts representation behind a marshaller that emits a
bare string whenever the parts slice is empty, so every message sent today
serialises unchanged. Extend the fold and the golden tests.

Verify: the Phase 3 golden test still passes byte-for-byte, plus a new one
covering a message that does carry parts.

### Phase 8 — Kimi's tool array and image Read — done

Landed as PR #93: `internal/tools` now ships two frozen arrays (DeepSeek's
sixteen unchanged byte for byte, Kimi's fourteen without `Screenshot` and
`ReviewScreenshot`), each pinned by its own golden file
(`TestToolArrayGolden`, decision 6). DeepSeek's array has grown since — an
`AskVision` tool was added, and then both `ReviewScreenshot` and `AskVision`
were replaced by `Glance`, `Ground`, `Detect`, and `Crop`
(docs/VISION-TOOLKIT.md) — but the golden-file mechanism this phase built is
what still pins both providers' arrays, at their current sizes of nineteen
and fourteen. `Read` returns an `image_url` part when
the path is a PNG/JPEG/WebP and the provider can see images — the bytes
base64-encoded into a data URI, capped by the existing screenshot cap
setting (`tools.reviewscreenshot_max_bytes`), with a refusal naming the limit
over the cap. The image bytes live in the `tool_result` event payload
(`ImageURL`), so the fold rebuilds the parts array identically on every
replay and stays append-only.

Verify: a Kimi run that captures a page through Bash and `playwright-cli`,
reads the PNG, and answers a question about what is in it. A DeepSeek run over
the same workspace, unchanged.

### Phase 9 — A Kimi system prompt

The tool inventory changed in Phase 8, so the prompt's tool list was wrong
for Kimi and had to be restated regardless. Two halves:

1. **The correction, shipped without an eval.** A prompt naming tools the
   model was never sent is a bug, not a wording choice. Kimi sessions now
   render a second frozen head — assembled in `internal/session` from the
   same fragment table as DeepSeek's, driven by Kimi's fourteen-tool array
   and the seesImages capability — whose inventory names the fourteen tools
   in Kimi's array, whose count word is corrected, and whose vision rule is
   the one sentence that is true for K3 (Read returns the image for a PNG,
   JPEG or WebP path). The dropped tools — five of them today, `Screenshot`,
   `Glance`, `Ground`, `Detect`, and `Crop` — are named nowhere in it.
   DeepSeek's head is untouched byte for byte, proven by the golden files
   `internal/session/testdata/prompt_*.golden.txt`.
   `TestPromptNamesExactlyTheToolArray` pins each head to its own array —
   no more, no less — so the inventories cannot drift apart again.
2. **The wording A/B, set up and unrun.** `kimi-steps` is a registered
   variant (the corrected Kimi prompt plus a step-by-step execution rule
   drawn from Kimi's own prompt guidance), comparable against `base` on
   `kimi-k3` over the `search` suite — its tasks need only reading and
   searching, and none depends on the dropped tools. The eval itself
   costs real tokens against a live key and was recorded `not_run` at
   landing; run it before shipping any wording as K3's default.

Verify: the eval comparison, recorded per [`docs/EVALS.md`](EVALS.md).

## 7. What this plan does not cover

- Kimi's batch API, file upload, and dynamic tool loading. All are real surface
  in the vendored docs and none is needed to run a session.
- Kimi's Anthropic-format endpoint at `https://api.moonshot.ai/anthropic`. The
  harness speaks the OpenAI format to DeepSeek, and using one format for both
  keeps a single client.
- Peak/off-peak pricing, which `configs/prices.json` already records as a
  pending DeepSeek change and is unrelated.
