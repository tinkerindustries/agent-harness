# Claude provider and claude-session

- **Slug**: claude-provider
- **Memory**: [memory/claude-provider.md](memory/claude-provider.md)
- **Reports**: [reports/claude-provider/](reports/claude-provider/)

## What this builds

The harness runs coding sessions on `claude-opus-5`, `claude-sonnet-5` and `claude-fable-5-1`,
talking to Anthropic's Messages API directly. A parent can drive those models through
`harness stdio-session` in the Responses vocabulary it already speaks. A parent can also drive
them through a new `harness claude-session`, which puts Anthropic's Managed Agents vocabulary
on the pipe. Claude sessions carry Anthropic's server-side web search and web fetch in place of
the harness's own WebFetch.

## Not building

- Amazon Bedrock, Google Vertex AI, Microsoft Foundry or Claude Platform on AWS hosting. The client talks to `api.anthropic.com` alone.
- A vendored `third_party/anthropic-docs/` mirror or a refresh skill for one. Agents read Anthropic's live pages.
- `claude-haiku-4-5`, `claude-fable-5`, the Opus 4.x or Sonnet 4.x models, or any model beyond the three above.
- The official `anthropic-sdk-go` module. The client is hand-rolled like the other three.
- Anthropic's code execution, tool search, memory, bash or text-editor tools. Only web search and web fetch join the tool array.
- Server-side compaction (`compact-2026-01-12`) and context editing. The loop keeps its own compaction.
- Server-side refusal fallbacks (`fallbacks`), fast mode, task budgets, Priority Tier, the Batches API, the Files API, and `count_tokens`.
- Kimi's or DeepSeek's Anthropic-format endpoints. Those are other providers' compatibility shims.
- Hosting Claude under `gemini-session`, or hosting DeepSeek or Gemini under `claude-session`.
- The rest of the Managed Agents surface: agents, environments, vaults, deployments, threads, outcomes, resources, memory stores, webhooks and budgets. `claude-session` borrows the session and event vocabulary only.
- Changing how `harness serve` or any existing session talks to DeepSeek, Gemini or Kimi.
- Honouring `retry-after` or any other change to `internal/providerhttp`'s retry policy.

## Phases

| Phase | Name | Depends on | Runs beside |
|---|---|---|---|
| 1 | Wire contract for claude-session | nothing | 2 |
| 2 | The Anthropic client and the loop's replay plumbing | nothing | 1 |
| 3 | Host Claude in stdio-session | 2 | nothing |
| 4 | The claude-session subcommand | 1, 3 | nothing |

Phases 1 and 2 share no files. Phase 1 writes one new document under `docs/`, and phase 2
writes Go code. Phase 3 and phase 4 both edit `cmd/harness/stdiosession.go` and
`internal/stdiosession/modelinfo.go`, so they run in sequence.

### Phase 1 — Wire contract for claude-session

**Depends on**: nothing

**What it builds**

`docs/STDIO-MANAGED-AGENTS.md` is the protocol reference a client for `harness claude-session`
is built from. It follows the shape of `docs/STDIO-INTERACTIONS.md` and has the same contents
list. Every Managed Agents shape it uses is Anthropic's own, cited against
<https://platform.claude.com/docs/en/managed-agents/sessions.md> and
<https://platform.claude.com/docs/en/managed-agents/events-and-streaming.md>.

It settles:

- **The method mapping.** Map `POST /v1/sessions` (with `initial_events`),
  `POST /v1/sessions/{id}/events` (`user.message`, `user.interrupt`), `GET /v1/sessions/{id}`
  and `DELETE /v1/sessions/{id}` onto the `Dialect`'s five verbs and the handshake. It says
  which JSON-RPC method names the process accepts.
- **The unit of work.** The existing dialects address a run. Managed Agents addresses a
  session that goes running → idle → running. The document says what a run is under this
  vocabulary, what id a client holds, and how resume across process restarts is spelled.
- **Model and system selection.** Managed Agents puts `model` on an agent, which this process
  has none of. The document picks the create-body spelling, for example
  `agent: {type: "agent_with_overrides", model: ...}` or a documented deviation, and lists
  every agent field it refuses.
- **The notifications.** Say which events the process emits and in what order: `session.status_*`,
  `span.model_request_start`/`_end`, `agent.message`, `agent.thinking`, `agent.tool_use`/`agent.tool_result`
  for the harness's tools and the web tools, `session.usage`, `session.error`, and the
  `event_start`/`event_delta` live previews. Say what carries reasoning text and the harness's
  own extensions (usage, cost, permission denials).
- **Client-declared tools.** Say whether they cross as `agent.custom_tool_use` plus
  `user.custom_tool_result` or keep `harness.function_call`, and why.
- **The seam.** Name every change the `Dialect` or `Translator` interfaces in
  `internal/stdiosession/dialect.go` and `translator.go` need to host this vocabulary. Name
  none if the seam fits unchanged.
- **Deviations and Not built.** Write both sections the way `STDIO-INTERACTIONS.md` does.

Before writing it up, the session puts the method mapping, the unit-of-work decision and the
seam changes to the user and gets agreement.

**Proof**

`docs/STDIO-MANAGED-AGENTS.md` exists, and the report quotes the user's agreement to the
mapping, the unit of work and the seam changes. Every section in the contents list of
`STDIO-INTERACTIONS.md` has a counterpart, or a line saying why none applies.

**Out of scope for this phase**

No Go code. `docs/STDIO-PROTOCOL.md`'s porting table and the other docs are phase 4's.

### Phase 2 — The Anthropic client and the loop's replay plumbing

**Depends on**: nothing

**What it builds**

Two halves, in this order.

*Replay plumbing,* in `internal/wire`, `internal/store`, `internal/session/turn.go`,
`internal/fold` and `internal/pricing`, following the memory's Schema section:

- Add the raw-block event type, the `wire.Item` field and the store payload field. `turn.go`
  commits the blocks with the sub-turn, and the fold puts them back on the item.
- Add the cache-write token count on `wire.Usage` and the optional cache-write price on
  `pricing.ModelPrices`. The usage payload's cost prices cache writes at that rate.
- DeepSeek, Kimi and Gemini never set any of these fields. Their request bodies and the
  `TestPromptGolden` and `TestGoldenFrames` captures stay byte-identical.

*`internal/anthropic`,* a new package implementing `session.Client` against
`POST https://api.anthropic.com/v1/messages`, hand-rolled on `internal/providerhttp.Transport`
with `SetAuth`. The shape follows `internal/gemini`:

- **Request.** Render `wire.ChatIntent` into a Messages body. The system prompt goes in
  top-level `system`. Items become `user`/`assistant` messages: `tool_use` and `tool_result`
  blocks, all of one sub-turn's results in one user message, and images as base64 blocks
  inside `tool_result`. An assistant turn with raw blocks is rendered from them verbatim.
  Mid-conversation system-role items render as system messages on Opus 5 and Fable 5.1, and
  as a user text block on Sonnet 5.
- **Thinking and effort.** Always send adaptive thinking with `display: "summarized"`,
  `intent.Effort` → `output_config.effort`, and `thinking.block_binding.prefix_mismatch_behavior:
  "error"` under the `thinking-binding-controls-2026-08-01` beta header.
- **Tools.** Render the frozen array as `input_schema` tools. Append `web_search_20260209` and
  `web_fetch_20260209` after it, in a fixed order.
- **Caching.** Place breakpoints as the memory's External facts section says.
- **Streaming.** Write its own SSE decoder into `wire.Event`s. `thinking_delta` becomes
  reasoning, `text_delta` becomes content, and `input_json_delta` becomes tool-call deltas.
  Emit one raw-block event per response, then usage and finish.
- **`pause_turn`.** Resume inside the client, bounded, so the loop sees one response.
- **Errors and retry.** Parse the error body. Retry 429, 500, 502, 503, 504 and 529 with the
  transport's existing backoff. A `refusal` stop reason ends the turn with an error carrying
  `stop_details`.
- **The rest of the seam.** `UsageSplit` and `CacheSlack` follow the memory. Neither
  reasoning-starvation nor argument repair applies.
- **`modelinfo.go`.** Display names, context windows and effort sets for the three models.
- **Tests.** Unit tests beside the code, in the style of `internal/gemini`'s: SSE fixtures, the
  request shape from fixed intents, byte stability across two renders, retry classification,
  and raw-block round trip through the fold.
- **`docs/ANTHROPIC-INTEGRATION.md`.** The provider reference, in the manner of
  `docs/GEMINI-INTEGRATION.md`.
- **The live check.** With `ANTHROPIC_API_KEY` from the user, drive the client by hand through
  a short tool-using exchange on each model: two sub-turns with a tool call and thinking
  replayed, one web search, and a cache hit on the second request. Record the findings in
  `docs/OBSERVED.md`, including the measured cache over-prediction behind `CacheSlack`.

**Proof**

`scripts/build.sh` passes. `go test ./internal/anthropic ./internal/session ./internal/fold
./internal/pricing` passes. `go test ./internal/stdiosession -run TestGoldenFrames` and
`go test ./internal/session -run TestPromptGolden` pass without `-update-golden`. The report
quotes the `docs/OBSERVED.md` entry from the live check.

**Out of scope for this phase**

Nothing outside `internal/anthropic` names a Claude model yet. The provider table, the tool
array and `cmd/harness` are phase 3's.

### Phase 3 — Host Claude in stdio-session

**Depends on**: 2

**What it builds**

- **`internal/provider`.** A `provider.Anthropic` name, the three models in `models`, and all
  three `true` in `visionCapable`.
- **`internal/tools/definitions.go`.** Claude models get the vision-capable array without
  WebFetch. Every other model's array is unchanged.
- **`internal/session/lifecycle.go`.** Whatever the Claude tool array needs at session creation.
  None of Gemini's schema lowering applies unless the live API refuses a schema.
- **`internal/stdiosession/modelinfo.go`.** A Claude case in each function. `missingKeyMessage`
  names `ANTHROPIC_API_KEY`.
- **`cmd/harness/stdiosession.go`.** Read `ANTHROPIC_API_KEY` from the environment and `-env`.
  Add it to `providerAPIKeyVars`, build the client, and dispatch Claude models in `clientFor`
  and `HasAPIKey`. `hostedModels` adds the Claude models under `stdio-session`.
  `resolveHostedModel` defaults to a Claude model only when the Anthropic key is the only key
  present. The flash model follows the session's own model, as it already does.
- **`configs/prices.json`.** Entries for the three models with the cache-write rate, source
  URL and capture date, read from the live pricing page.
- **Docs.** `CLAUDE.md`, `ARCHITECTURE.md`, `internal/CLAUDE.md` (a codemap entry for
  `internal/anthropic`), `docs/STDIO-PROTOCOL.md` (models, credentials) and `TESTING.md`.
- **Tests.** End-to-end tests in `internal/stdiosession` drive a Claude model over the pipe
  against a fake Anthropic server. Tests in `cmd/harness` cover the key handling, and the key is
  stripped from `Bash` and MCP children.
- **The live check.** Run a real `harness stdio-session` session on each Claude model, with a
  tool call, a steer and a resume. Record findings in `docs/OBSERVED.md`.

**Proof**

`scripts/build.sh` passes. `go test ./internal/stdiosession -run TestGoldenFrames` passes
without `-update-golden`. The report quotes a live `stdio-session` transcript excerpt on
`claude-sonnet-5` showing a tool call, its result and `response.completed`.

**Out of scope for this phase**

No `claude-session` subcommand and no Managed Agents dialect. Those are phase 4's.

### Phase 4 — The claude-session subcommand

**Depends on**: 1, 3

**What it builds**

- **The dialect.** Implement the Managed Agents vocabulary exactly as
  `docs/STDIO-MANAGED-AGENTS.md` specifies, as a third `Dialect` in `internal/stdiosession`.
  Follow the file split of `interactions.go`, `interactionstranslate.go` and
  `interactionswire.go`, and make the seam changes phase 1 named.
- **Per-turn id.** The harness-level turn id `docs/STDIO-MANAGED-AGENTS.md` specifies. The
  session id is the conversation's address, and the turn id lets get and interrupt address a
  single turn.
- **Async client-declared tools.** Teach `session.Runner` to pause a run on a client tool
  call and resume it when the result arrives, as `docs/STDIO-MANAGED-AGENTS.md` specifies. The
  parent receives `agent.custom_tool_use` as a notification, the session goes idle with
  `stop_reason: requires_action`, and `user.custom_tool_result` through `sessions.events`
  resumes the run. The Responses and Interactions dialects keep the blocking
  `harness.function_call` unchanged.
- **`cmd/harness`.** A `claude-session` subcommand that hosts the Claude models alone, in
  `main.go`'s dispatch, `harness help` and `dialectFor`, with the Anthropic key only.
- **Tests.** End-to-end tests over a pipe against a fake Anthropic server, and a golden
  capture of the new vocabulary's frames beside `TestGoldenFrames`.
- **Docs.** The porting table in `docs/STDIO-PROTOCOL.md` gains the third vocabulary.
  `CLAUDE.md`, `ARCHITECTURE.md` and `internal/CLAUDE.md` name the third subcommand and dialect.
  Correct `docs/STDIO-MANAGED-AGENTS.md` wherever the build had to depart from it, so it still
  describes the wire.
- **The live check.** Drive `harness claude-session` by hand through create, a tool call,
  append, interrupt, get and resume on a Claude model. Record findings in `docs/OBSERVED.md`.

**Proof**

`scripts/build.sh` passes. `go test ./internal/stdiosession` passes, including the existing
`TestGoldenFrames` without `-update-golden` and the new capture. The report quotes a live
`claude-session` transcript excerpt showing a create, a tool round and the session going idle.

**Out of scope for this phase**

Any Managed Agents resource beyond sessions and events (see Not building).

## Open questions

- Whether Opus 5 and Sonnet 5 can put more than one `thinking` block in a single response.
  The raw-block store holds either case. Phase 2's live check settles it for `OBSERVED.md`.
- The measured `CacheSlack` for Anthropic. Phase 2 starts from a stated guess and replaces it
  with the live figure.
- How server-side web tool activity appears to a `stdio-session` parent in the Responses and
  Interactions vocabularies. Phase 3 decides, and it must not change a frame for a non-Claude
  session. Phase 1 decides it for `claude-session`.

## Changes to this plan

- Phase 1 sign-off. The user chose to expose a harness-level per-turn id alongside the
  session id, and to deliver client-declared tools through Anthropic's async shape
  (`agent.custom_tool_use` → idle `requires_action` → `user.custom_tool_result`) rather than
  the blocking `harness.function_call`. Phase 4 grew to build both. If phase 4 turns out too
  big for one session, split the async tool pause and resume in `session.Runner` into its
  own phase ahead of the dialect.
