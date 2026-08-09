# Implementation plan

Five phases. Each one ends at a state you can demonstrate, not just a set of
finished tasks.

Ordering is by capability and dependency — what the next thing needs, and what
becomes possible once a phase lands. It is not a risk-reduction sequence. There
are no throwaway spikes; every phase is production code in its final home.

Design rationale lives in [docs/](docs/). This file says what gets built and in
what order.

---

## Phase 1 — Speak DeepSeek

**Goal.** A Go client that talks to the API correctly, including the parts a
generic OpenAI-compatible client gets wrong.

**Work.**

- Go module, directory layout, config loading. API key from `DEEPSEEK_API_KEY`,
  base URL, default model, effort, and the price table.
- Request and response types as structs, never maps, so serialisation is
  byte-stable.
- `POST /chat/completions`, non-streaming, against `https://api.deepseek.com`.
- The request-shape rules: no `tool_choice`, `max_tokens` set explicitly,
  `system` rather than `developer`, `""` rather than `null` for tool-call
  assistant content.
- Streaming. SSE reader with large-line handling, `:` comment filtering, an idle
  watchdog, `[DONE]` termination, transport-level timeouts and no client-level
  one. `stream_options.include_usage` on every request.
- The tool-call assembler, written for the indexed incremental form.
- `GET /models` and `GET /user/balance`.
- Retry classification: back off on 429, 500, 503; never retry 400, 401, 402,
  422.

**Exit criteria.** `harness ask "..."` streams reasoning and content to the
terminal and prints usage with the cache hit and miss split plus computed cost.
`harness models` and `harness balance` return real data.

**Not yet.** No tools, no persistence, no loop.

**Effort.** Small to medium.

---

## Phase 2 — Become an agent

**Goal.** A working headless coding agent. This is the phase that makes the
thing real, and it holds most of the DeepSeek-specific correctness.

**Work.**

- SQLite through `modernc.org/sqlite`. Events keyed by `(session_id, seq)`, plus
  session metadata. Append and replay.
- The session freezes its rendered system prompt and tool schema at creation,
  so a later upgrade cannot re-prefix a resumable session.
- The fold from event log to a `messages` array, preserving `reasoning_content`
  verbatim on every assistant message.
- The ten tools from [docs/TOOLS.md](docs/TOOLS.md), schemas matching the
  trained-in shape, arguments validated in Go.
- Workspace confinement checked after symlink resolution. Per-tool timeouts and
  output byte caps with truncation labelled in the result.
- `Edit` carries the most rules and earns the most tests: unique-match
  enforcement, the prior-read gate, the line-number-prefix error, and the
  applied diff in the result.
- The agent loop. Sub-turns until a response arrives with no tool calls.
  Parallel calls executed concurrently, results appended in `tool_calls` order.
- Approval gating as a blocking event the loop waits on.
- `Task` and `WebFetch` on flash, each in its own conversation.
- Compaction at 768K, forking a new session with the summary in its system
  prompt.
- Cache discipline: serialise once and retry the identical buffer, and record
  expected miss against actual `prompt_cache_miss_tokens` per sub-turn.

**Exit criteria.** Point it at a scratch repository with a task and it edits
files and runs commands until the task is done. Cache miss tokens match the
appended content on every sub-turn after the first.

**Not yet.** No browser. It is a CLI.

**Effort.** Large. The biggest phase by some distance.

---

## Phase 3 — Put it on the wire

**Goal.** The agent visible and drivable in a browser.

**Work.**

- SSE endpoint with `Last-Event-ID` replay.
- Command POSTs for prompt, approve, and cancel. Session list and creation.
- `embed.FS` for the built frontend, with a dev-mode passthrough to Vite.
- Vite, React, TypeScript. The external store and its `useSyncExternalStore`
  bindings.
- SSE client with reconnect and resume.
- The event-to-view-model fold, mirroring the Go fold in shape.

**Exit criteria.** Drive a full agent run from the browser. Reload mid-run and
it picks the stream back up where it left off.

**Not yet.** It is unstyled, and it will not hold frame rate under load.

**Effort.** Medium.

---

## Phase 4 — Make the display hold up

**Goal.** A transcript that stays smooth at token rate and is genuinely readable.

**Work.**

- Completed blocks frozen, memoised, keyed by block id, never re-rendered.
- The live tail buffered outside React state and flushed on
  `requestAnimationFrame`.
- Plain preformatted text while streaming; parse and highlight once on
  completion.
- The reasoning panel, expanding while streaming and collapsing on completion,
  showing elapsed time and token count.
- Tool call and result blocks, one shape per tool. Streaming command output.
- The approval prompt wired to the blocking event from phase 2, with permission
  modes.
- Large outputs collapsed to head and tail with expand.
- Diffs rendered from the structured line arrays Go sends.
- The plan panel, pinned beside the transcript, driven by `TodoWrite`.
- `Task` subagents as collapsed child transcripts.
- Measurement: a synthetic delta feed at a fixed rate, frame times recorded with
  a few hundred blocks mounted.

**Exit criteria.** A long session with large tool outputs holds frame rate, and
approvals work from the UI. The measurement answers the virtualisation question
with a number rather than an intuition.

**Effort.** Large.

---

## Phase 5 — Make the economics visible

**Goal.** The cost lever is legible and controllable. Cache hit rate is the
difference between a cheap harness and a ruinous one, so this phase is not
decoration.

**Work.**

- Per-turn and per-session token counts by category, cost derived from the
  config price table.
- The churn diagnostic: expected miss, actual miss, and the index of the first
  message that differs from the previous request when the two disagree.
- Balance from `GET /user/balance`, refreshed on startup and after a 402, which
  is surfaced as an empty account rather than a failure.
- Model and effort controls in the session header, populated from `GET /models`.
- A model switch shows its cache-miss estimate before it applies.
- `temperature` and `top_p` hidden while thinking is enabled.
- The prefix warmup, if the phase 1 and 2 numbers justify it.
- Session list, resume, and delete.

**Exit criteria.** v1. A run states its own cost, a deliberately churned prefix
is caught by the diagnostic and named to the specific message, and switching
model prices itself beforehand.

**Effort.** Medium.

---

## Threaded through, not phased separately

**Cache work** does not get its own phase because it cannot. The invariants are
structural and land in phase 2 — frozen prompts, append-only folds, stable tool
arrays, identical retry bytes. The diagnostic that proves they hold lands in
phase 5. Treating cache as a later optimisation pass would mean rebuilding the
fold.

**Doc validation** repeats rather than completes. Pro's effort mapping and the
pricing table were both flagged as changing within days of the 2026-08-09
capture. Re-check at phase 1 and again before v1, and when refreshing the vendor
mirror, copy to a new dated directory and diff rather than overwriting
`docs/sources/2026-08-09/`.

---

## Open questions and where they close

None of these block the phase they sit in. Each closes as a side effect of work
that was happening anyway.

| Question | Closes at | How |
| --- | --- | --- |
| Does pro honour `low` effort yet? | Phase 1 | One request. The docs said this changes in early August 2026 |
| Is the `[1m]` suffix real on the native endpoint? | Phase 1 | `GET /models` |
| Do streaming tool-call deltas arrive incrementally? | Phase 1 | Observe. The assembler handles both shapes |
| Are the Claude Code tool names the right vocabulary? | Phase 2 | Tool-call error rate against a rename, which is cheap |
| Does changing effort mid-session disturb the cache? | Phase 2 | Hit rate across an effort change |
| Flash or pro for the main loop? | Phase 2 | Same task both ways. Flash-0731 beats V4-Pro-Preview on published agent benchmarks |
| Is virtualisation needed? | Phase 4 | Frame times with a few hundred blocks |
| Is the prefix warmup worth its two probe requests? | Phase 5 | Measure first-request miss with and without |

---

## Sequencing notes

Phases 1 and 2 together are a real deliverable on their own — a headless
DeepSeek coding agent usable from a terminal. If value is wanted early, stop
there and use it while phases 3 to 5 get built.

Phase 2 is where the DeepSeek-specific correctness concentrates: the
`reasoning_content` round-trip, the non-null tool-call content, the append-only
fold, the ordered tool results. Errors there surface as 400s and silent cost
blowouts rather than as obvious breakage, which is why the exit criterion is a
cache measurement and not just a working demo.

Phases 4 and 5 can run in either order, or in parallel if there are two people.
Neither depends on the other.
