# Phase 3 — host Claude in stdio-session

## What was built

**`internal/provider`.** `provider.Anthropic`, the three models
(`claude-opus-5`, `claude-sonnet-5`, `claude-fable-5-1`) in `models`, and all
three `true` in `visionCapable`.

**`internal/tools/definitions.go`.** `definitionsClaude` —
`definitionsVisionCapable` minus `WebFetch` — with `DefinitionsFor`
resolving a Claude model to it ahead of the ordinary vision check, since
`provider.SeesImages` is also true for Claude and would otherwise hand back
the array with `WebFetch` still in it. Every other model's array is
unchanged; a new golden file (`tools_claude.golden.json`) and golden-test
case pin Claude's array, and `TestToolArrayGolden`/the rest of the suite
still pass unchanged for every other case.

**`internal/session/lifecycle.go`.** Left unchanged. Gemini's schema
lowering (`gemini.LowerToolSchemas`) is gated on `provider.ModelFor ==
provider.Gemini` already, so a Claude session bypasses it by construction.
The live check (below) confirms the live API accepted Claude's tool array
with no schema refusal, so nothing needed adding here this phase.

**`internal/stdiosession/modelinfo.go`.** A `provider.Anthropic` case in
`displayName`, `contextWindowTokens`, `reasoningEfforts`,
`reasoningEffortSupported`, each calling the matching
`internal/anthropic/modelinfo.go` function, and `missingKeyMessage` names
`ANTHROPIC_API_KEY`.

**`cmd/harness/stdiosession.go` and `main.go`.** `ANTHROPIC_API_KEY` read
from the environment and `-env` (`apiKeys` now returns three keys),
`providerAPIKeyVars` carries it (stripped from `Bash` and MCP children the
same way the other two are), a real `anthropic.Client` is built and
dispatched in `clientFor`/`HasAPIKey` alongside DeepSeek's and Gemini's.
`hostedModels` adds the three Claude models under `stdio-session` only —
`gemini-session` still advertises Google's rows alone.
`resolveHostedModel` gained a `defaultClaudeSessionModel` = `claude-sonnet-5`
branch: it is chosen only when neither the Google nor the DeepSeek key is
present and the Anthropic key is — DeepSeek's own default still wins when
both DeepSeek's and Anthropic's keys arrive with no Google key, so the
existing precedence order is extended rather than reordered. `geminiVisionModel`
and `GeminiModel` needed no change: no hosted model (Claude included) is
ever offered the vision tools that reach Google, so a Claude session's
vision-model closure falls back to the Gemini default exactly the way a
DeepSeek session's already did. `cmd/harness help`'s usage string now names
the three Claude models.

**Mid-stream retry.** `internal/anthropic/client.go`'s `StreamChatCompletion`
now calls a new `streamOneRound` per HTTP round (including each round of a
`pause_turn` resume) instead of inlining `transport.Do` +
`readSSE`. `streamOneRound` wraps the *unmodified* `readSSE` in a
`providerhttp.StreamOpener`/`StreamPump` pair and drives it through
`providerhttp.Transport.RetryStream`, so a connection that dies after a 200
but before any output is reopened with the same request body and re-pumped,
on the transport's existing backoff — the same mechanism DeepSeek, Kimi and
Gemini use. The round's `streamResult` (blocks, stop reason, usage) is
captured by a closure the pump writes into before closing its events
channel, which is race-free because a channel close happens-after every
send (and every write ordered before it) the closing goroutine performed;
`go test -race` confirms this. The retry is per round, not around the whole
`pause_turn` loop, because a round that already streamed output before
dying still ends the sub-turn on that output — the same "has this stream
spoken yet" gate every other provider's retry respects — and reopening a
round that already spoke would resend a request the API may be mid-way
through answering. Two new tests
(`internal/anthropic/streamretry_test.go`) prove a reset before the first
frame is retried invisibly, both on an ordinary request and mid-`pause_turn`
(round two's own connection resets and is reopened independently of round
one).

**`configs/prices.json`.** Entries for the three models, read from
<https://platform.claude.com/docs/en/about-claude/pricing> on 2026-09-22:
Opus 5 ($5/$25, $6.25 cache write, $0.50 cache hit), Sonnet 5 ($2/$10,
$2.50, $0.20 — the $2/$10 rate is now standard, not introductory; the
scheduled 2026-09-01 increase to $3/$15 was cancelled), Fable 5.1
($10/$50, $12.50, $0.25 — Fable 5.1's cache-hit multiplier is 0.025x base
input, not the standard 0.1x every other model in the table uses). Every
cache-write figure is the 5-minute rate, since `internal/anthropic` never
requests the 1-hour TTL. `TestEveryKnownModelIsPriced` passes.

**Docs.** `CLAUDE.md`, `ARCHITECTURE.md` (prose and the dependency mermaid
diagram and ASCII graph text), `internal/CLAUDE.md` (a new `internal/anthropic`
codemap entry, and the `cmd/harness`/`internal/stdiosession`/`internal/provider`
entries updated for a third-and-fourth provider), `docs/STDIO-PROTOCOL.md`
(the credentials table, the hosted-models table and its surrounding prose,
the `-model`/`-env` flag docs, and the two `reasoning.effort`
cross-provider-set deviation notes), `TESTING.md` (the new Claude fake-server
end-to-end test), and `docs/ANTHROPIC-INTEGRATION.md` (the "hosts three
models" intro corrected now that the provider table and tool array actually
know them, and a new "Mid-stream retry" section).

**Tests.** `internal/stdiosession/claude_test.go` — a fake Anthropic Messages
API recorder at one end of a real pipe and a real `anthropic.Client` at the
other (`TestClaudeSessionOverResponsesDialect`), the same two-ended shape
`endtoend_test.go` uses for DeepSeek: asserts the outbound HTTP is a genuine
Messages API request (`system`/`messages`/`tools`, required headers, no
Responses-shaped fields), that the tool array carries Anthropic's
`web_search` server tool and not the harness's `WebFetch`, and that the
inbound frames round-trip a tool call and its result to `response.completed`
over the ordinary Responses vocabulary. `cmd/harness/stdiosession_test.go`
gained Anthropic-key coverage: reading from the environment and `-env`,
`resolveHostedModel`'s new default-precedence cases, `hostedModels`
including/excluding the three Claude models per subcommand, and
`ANTHROPIC_API_KEY` in the `stripProviderAPIKeys` coverage (the same
function `Bash` and MCP children's `EnvFilter` is wired to, so this is the
proof that the key is stripped from both).

**The live check.** See "What was verified, and how".

## What was verified, and how

`scripts/build.sh`'s tail:

```
=== gofmt
clean

=== go vet
clean

=== tests
ok  	github.com/mrgeoffrich/agent-harness/cmd/harness	(cached)
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

`go test ./internal/stdiosession -run TestGoldenFrames` passes without
`-update-golden` — nothing about a non-Claude session's frames moved.
`go test ./internal/anthropic/... -race` passes, covering the
`streamOneRound` closure-sharing concern directly.

**The live check.** A real `harness stdio-session -env
~/.config/agent-harness/anthropic.env` process per Claude model, driven by
hand over the pipe: `initialize`, a `responses.create` whose task calls
`Bash` once, a `responses.append` steer sent while that tool call was still
in flight, then a second `responses.create` naming the first response's id
as `previous_response_id` and asking a question only answerable from the
first response's history. All three models completed both responses
cleanly; findings are in `docs/OBSERVED.md`, "Claude Messages API — Phase 3
live check". Total spend for all three models: $0.31.

The excerpt the proof asks for, `claude-sonnet-5`, a tool call, its result
and `response.completed` (trimmed to the tool round; the full transcript
also has the steer's second tool call and the resume, both in
`docs/OBSERVED.md`):

```jsonc
// create
>>> {"id":"2","method":"responses.create","params":{"model":"claude-sonnet-5",
    "input":"Run `echo hello-claude-live` with Bash and tell me its exact output in one short sentence. Do not do anything else.",
    "harness":{"cwd":"/tmp/claude-live/claude-sonnet-5/work","permission_mode":"full"},
    "reasoning":{"effort":"low"},"max_output_tokens":2048}}

// the tool call
<<< {"method":"response.output_item.done","params":{"type":"response.output_item.done",
    "response_id":"resp_150f133308b2687c8020d700","output_index":2,
    "item":{"type":"function_call","id":"item_2","status":"completed",
    "call_id":"toolu_017MTz7GTtjXcgCyxq2sRsK3","name":"Bash",
    "arguments":{"command":"echo hello-claude-live","description":"Echo test string"},
    "harness":{"sub_turn":1}}}}

// its result
<<< {"method":"harness.tool_output","params":{"type":"harness.tool_output",
    "response_id":"resp_150f133308b2687c8020d700",
    "call_id":"toolu_017MTz7GTtjXcgCyxq2sRsK3","text":"hello-claude-live\n"}}
<<< {"method":"response.output_item.done","params":{"type":"response.output_item.done",
    "response_id":"resp_150f133308b2687c8020d700","output_index":3,
    "item":{"type":"function_call_output","id":"item_3","status":"completed",
    "call_id":"toolu_017MTz7GTtjXcgCyxq2sRsK3","name":"Bash",
    "output":[{"type":"input_text","text":"hello-claude-live\n"}],
    "harness":{"sub_turn":1}}}}

// (the steer's own tool call and second sub-turn happen here — see OBSERVED.md)

// response.completed
<<< {"method":"response.completed","params":{"type":"response.completed",
    "response":{"id":"resp_150f133308b2687c8020d700","status":"completed",
    "model":"claude-sonnet-5",
    "usage":{"input_tokens":23118,"input_tokens_details":{"cached_tokens":23113},
    "output_tokens":195,"total_tokens":23313,
    "harness":{"cost_usd":0.0358576,"sub_turns":3}},
    "harness":{"reason":"no_tool_calls","sub_turns":3}}}}
```

The resume request that followed (`previous_response_id` naming this
response) got back "The exact shell command I ran first was `echo
hello-claude-live`." — the model correctly recalled the first sub-turn's
tool call from the replayed history, and `harness.usage` on that request
showed 11710/11712 input tokens read from cache.

## What was left undone

- **`internal/session/lifecycle.go`'s comment doesn't yet say explicitly
  "Claude needs no schema lowering."** The code path already excludes
  Claude by construction (the `provider.Gemini` check), and the live check
  confirms nothing broke, but a future reader of that file has no pointer
  to *why* Claude was never considered for lowering short of this report.
  Worth a one-line comment if a later phase touches that function anyway.
- **`CacheSlack`'s figure is still the phase 2 guess (1024), not
  re-measured against a real multi-sub-turn harness session** in the sense
  the plan's open question asked for. The live check here *did* run a real
  multi-sub-turn session (four sub-turns across two responses) and every
  cache-read ratio stayed at 99.6%+, which is consistent with 1024 being
  generous rather than tight, but this was three short sessions on a
  trivial task, not the incrementally-growing session Gemini's own figure
  was tuned against. Worth revisiting if a production Claude session ever
  reports a churn warning.
- **No test drives `cmd/harness`'s `clientFor`/`HasAPIKey` dispatch against
  a live or fake Anthropic server from that package** — the dispatch logic
  itself (`providerFor(m) == provider.Anthropic`) is exercised indirectly
  by `TestTheSubcommandPicksTheDialect`'s model-list assertions and fully
  by the live check, but there's no `cmd/harness`-level test that starts a
  session on a Claude model and drives it to completion the way
  `TestHostedSessionNeverWritesTheKeyToSettings` does for Gemini. The
  `internal/stdiosession` end-to-end test covers the same code path one
  layer down (the `Runner`/`Client` wiring is identical either way), so
  this is a coverage gap in one specific place rather than an unverified
  behaviour.

## Bugs found and not fixed

None. The live API accepted every request this phase sent — no schema
refusal, no unexpected stop reason, no pricing or usage-shape surprise
beyond what phase 2 had already found and this phase's `OBSERVED.md`
section confirms held under real multi-sub-turn traffic.

## Mechanical friction

- **The live-check driver script's first attempt sent `"harness.cwd":
  "..."` as a flat, dotted key** and got back `harness.cwd is required`,
  which briefly looked like a validation bug. It was a scripting mistake,
  not a doc gap: `docs/STDIO-PROTOCOL.md`'s own `responses.create` example
  JSON, a few lines above the field table that names the field
  `harness.cwd` in prose, already shows the correct nested shape
  (`"harness": {"cwd": ...}`) — the dotted spelling in the table is just
  how every nested field in that table is named for readability
  (`harness.permission_mode`, `reasoning.effort` get the same treatment).
  Worth remembering for the next hand-driven client rather than worth
  fixing: read the example object, not just the field-name column.
- Otherwise no friction: the build, the existing suite, and the live API
  all behaved as documented. `scripts/build.sh`'s full cross-compile run
  (six targets) is the slowest step in the loop, at roughly a minute; not a
  problem, just worth knowing before assuming a build hung.

## Memory suggestions

- **`resolveHostedModel`'s three-way default precedence (Google >
  DeepSeek > Anthropic) is now the whole rule**, not just Google-vs-DeepSeek.
  Phase 4's `claude-session` subcommand hosts Claude alone, so it will not
  need this function's multi-provider precedence at all — but if a later
  feature ever adds a fourth hosted provider to `stdio-session` specifically,
  this is the function whose `switch` grows, not `hostedModels` or the key
  dispatch (those are already provider-generic via `provider.ModelFor`).
- **The live check's cost figures are a useful sanity range for anyone
  budgeting a Claude smoke test**: a four-sub-turn session with two Bash
  calls costs $0.04 on Sonnet 5, $0.10 on Opus 5, $0.17 on Fable 5.1 — driven
  almost entirely by each model's own per-token rate, not by anything
  this harness does differently per model, since cache-read ratios were
  effectively identical across all three (99.6–99.98%).
- **The `internal/stdiosession/claude_test.go` two-ended pattern
  (`endtoend_test.go`'s shape, applied to a fake Anthropic recorder) is
  the one this phase found most valuable relative to its cost.** It caught
  the "Claude's array needs to drop `WebFetch`, not just the six vision
  tools" requirement as an assertion rather than something only the live
  check would have caught, and it runs in milliseconds. A future phase
  adding a fourth Claude-adjacent behaviour to the tool array should extend
  this file rather than only trusting the live check.
