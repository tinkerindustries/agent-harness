# Implementation plan

Six phases. Each one ends at a state you can demonstrate, not just a set of
finished tasks.

Ordering is by capability and dependency — what the next thing needs, and what
becomes possible once a phase lands. It is not a risk-reduction sequence. There
are no throwaway spikes; every phase is production code in its final home.
Within a phase, the work is listed in the order it can be built.

This file says what gets built and in what order. Design rationale lives in
[docs/DESIGN.md](docs/DESIGN.md), and § references below point into it. Findings
measured against the live API are in [docs/OBSERVED.md](docs/OBSERVED.md) and
override the vendored docs where they disagree.

---

## Phase 1 — Speak DeepSeek

**Goal.** A Go client that talks to the API correctly, including the parts a
generic OpenAI-compatible client gets wrong.

**Work.**

- Go module, directory layout, config loading. API key from `DEEPSEEK_API_KEY`,
  base URL, default model, effort, and the price table, all from config (§4.9).
- Request and response types as structs, never maps, so serialisation is
  byte-stable (§3.2).
- `POST /chat/completions`, non-streaming, against `https://api.deepseek.com`,
  plus `GET /models` and `GET /user/balance`.
- `thinking` and `reasoning_effort` wired through from config, defaulting to
  enabled and `max` per [docs/MODELS.md](docs/MODELS.md).
- The request-shape rules (§4.4): no `tool_choice`, `max_tokens` set explicitly
  and generously, `system` rather than `developer`, `""` rather than `null` for
  tool-call assistant content. One line each, and a class of 400 if missed.
- Detect `finish_reason == "length"` with empty `content` as its own condition.
  Reasoning spends `max_tokens` before the answer starts, so a tight budget is
  billed in full and returns nothing ([docs/OBSERVED.md](docs/OBSERVED.md)).
- Streaming (§4.3). SSE reader with large-line handling, `:` comment filtering,
  an idle watchdog, `[DONE]` termination, transport-level timeouts and no
  client-level one. `stream_options.include_usage` on every request.
- Typed deltas onto a channel, and the tool-call assembler keyed by `index`.
- Retry classification: back off on 429, 500, 503; never retry 400, 401, 402,
  422 (§4.5).

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

*Store and fold.*

- SQLite through `modernc.org/sqlite` in WAL mode (§4.8). Events keyed by
  `(session_id, seq)`, plus session metadata, work requests, and workspace
  leases. Every write through a single writer goroutine fed by a channel; reads
  from a separate read-only pool (§4.5).
- The session freezes its rendered system prompt and tool schema at creation, so
  a harness upgrade cannot re-prefix a resumable session
  ([docs/CACHE.md](docs/CACHE.md)).
- The fold from event log to a `messages` array, preserving `reasoning_content`
  verbatim on every assistant message (§3.1) and emitting `""` rather than
  `null` for tool-call assistant content (§4.4).
- The disk mirror, written after the database commit and unable to fail a run:
  `session.json`, `events.jsonl`, and a `transcript.md` rewritten at turn
  boundaries. Plus `harness export`, which rebuilds a session directory from the
  database and doubles as the repair path.

*Tools.*

- The eleven tools in [docs/TOOLS.md](docs/TOOLS.md), schemas matching the
  trained-in shape, arguments validated in Go. Workspace root confinement with
  post-symlink escape checking. Per-tool timeouts and output byte caps with
  truncation labelled in the result.
- File and search tools first — `Read`, `Write`, `Edit`, `Glob`, `Grep`, `List`,
  `Bash`. `TodoWrite` is a state write with no side effects. `Complete` is a
  state write plus JSON Schema validation of `result` against the schema the
  session was given. `Task` and `WebFetch` need the loop, so they come after it.
- `Edit` carries the most rules and earns the most tests: unique-match
  enforcement, the prior-read gate, the line-number-prefix error, and the
  applied diff in the result.
- The permission policy in front of every tool (§4.6): mode plus deny patterns,
  evaluated in Go, returning a denial through the tool result channel rather
  than an error. The CLI's interactive resolver goes in behind the same
  interface.

*Loop.*

- Sub-turn iteration until a response arrives with no tool calls, or until
  `Complete` is called. Parallel calls executed concurrently, results appended
  in `tool_calls` order.
- The loop behind a session-runner interface, holding no state outside the
  session it is running (§4.5), so the phase 3 worker pool is a caller change
  rather than a rewrite.
- `Task` and `WebFetch` on flash, each in its own conversation, with the nested
  transcript kept out of the parent message array.
- Compaction at 768K, forking a new session seeded with the summary in its
  system prompt and linked to the parent (§3.2).
- Cache discipline, because this is where requests get built: serialise once and
  retry the identical buffer, and record expected miss against actual
  `prompt_cache_miss_tokens` per sub-turn with the churn-point diff
  ([docs/CACHE.md](docs/CACHE.md)). The churn state is per-session.

**Exit criteria.** Point it at a scratch repository with a task and it edits
files and runs commands until the task is done, with cache miss tokens matching
the appended content on every sub-turn after the first. Two runs launched at
once against different workspaces complete without touching each other's state,
and the second one's head is a cache hit.

Tests that carry weight here: the fold over a plain turn, a tool-call turn, a
parallel-tool-call turn, and a multi-turn session carrying turn-1 reasoning into
turn 2; one asserting the fold is append-only, so folding N events then N+1
yields identical bytes for the first N; one asserting an exported session
matches the mirror written during the run; and the tool suite proving escapes
are rejected, timeouts fire, caps truncate and label, and each permission mode
denies what it should while the tool array stays identical across all three.

**Not yet.** No browser and no queue. It is a CLI.

**Effort.** Large. The biggest phase by some distance.

---

## Phase 3 — Take work off the queue

**Goal.** The harness is a service. Requests arrive on NATS, several run at
once, results come back.

**Work.**

- `docker-compose.yml` for a local JetStream server, with host ports taken from
  the environment because a machine may already run NATS on the defaults.
- Streams and consumer declared by the harness at startup rather than by a setup
  script, so an empty server converges: the `WORK` work-queue stream, the
  `RESULTS` stream, and a durable pull consumer with `MaxAckPending` matched to
  the worker pool size (§4.10).
- Request and result structs, with validation of `workspace` against the
  configured roots, of `permission_mode`, and of `result_schema` as a
  well-formed schema. A request that fails validation gets a `failed` result and
  a `Term`, not a retry.
- The worker pool: pull, dispatch to a session goroutine, recover from panics,
  honour the request deadline and `max_sub_turns`.
- Ack discipline: `InProgress` heartbeats every 20s while a run holds a message,
  publish the terminal result then ack, `Nats-Msg-Id` set so a redelivered
  publish deduplicates, `Nak` with delay on transient failure.
- Idempotency through the `work_requests` row. A terminal row republishes the
  stored result and acks without running. A row left `running` by a dead process
  is taken over as a new session linked to the abandoned one, so the failed
  attempt's transcript survives.
- Workspace leases enforced, with wait-or-fail chosen per request. This is the
  first caller that can produce two runs wanting one directory.
- Progress publication at turn level, rate-limited to one message per second,
  never token level.
- The per-model semaphore shared across the pool, sized under the account
  limits.

**Exit criteria.** Publish four requests at once and get four results back with
correct usage figures. Kill the harness mid-run and restart it: the unacked
request redelivers and completes, and the abandoned session's transcript is
still on disk. Publish the same `request_id` twice and get one run and one
result.

**Not yet.** No browser.

**Effort.** Medium.

---

## Phase 4 — Put it on the wire

**Goal.** The sessions visible in a browser. Read-only.

**Work.**

- `GET` endpoints for session list, session metadata, and paged events. Every
  other method returns 405 (§4.2). Bound to loopback by default.
- SSE endpoint with `Last-Event-ID` replay, plus a session-level stream for the
  list view.
- `embed.FS` for the built frontend, with a dev-mode passthrough to the Vite
  server.
- Vite, React, TypeScript. The external store and its `useSyncExternalStore`
  bindings. SSE client with reconnect and resume.
- The event-to-view-model fold, mirroring the Go fold in shape.
- The session list: status, model, workspace, elapsed, sub-turns, running cost,
  request id.
- No write path. No command client, no optimistic update, no reconciliation
  between local intent and server state.

**Exit criteria.** `curl` against the SSE endpoint shows a live queue-driven
run, reconnecting with a `Last-Event-ID` replays cleanly, and a `POST` to any
path returns 405. In the browser, watch a queue-driven run start to finish,
reload mid-run and pick the stream back up, and see several concurrent runs all
update in the list.

**Not yet.** It is unstyled, and it will not hold frame rate under load.

**Effort.** Medium.

---

## Phase 5 — Make the display hold up

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
- Denied calls as their own block, showing the call and the rule that refused
  it. Denials are what an operator looks for when a queue-driven run did less
  than its request asked for.
- Large outputs collapsed to head and tail with expand.
- Diffs rendered from the structured line arrays Go sends.
- The plan panel, pinned beside the transcript, driven by `TodoWrite`.
- `Task` subagents as collapsed child transcripts.
- Measurement, because this is the first point where there is something to
  measure: a synthetic delta feed at a fixed rate, frame times recorded with a
  few hundred blocks mounted.

**Exit criteria.** A long session with large tool outputs holds frame rate, and
a run that did less than asked can be traced to the denials that stopped it. The
measurement answers the virtualisation question (§5.5) with a number rather than
an intuition.

**Effort.** Large.

---

## Phase 6 — Make the economics visible

**Goal.** The cost lever is legible. Cache hit rate is the difference between a
cheap harness and a ruinous one, so this phase is not decoration.

**Work.**

- Per-turn and per-session token counts by category, cost derived from the
  config price table, cache hit rate displayed prominently.
- The churn diagnostic surfaced: miss tokens, expected miss tokens, and the
  index of the first message that differs from the previous request when the two
  disagree.
- The same figures in the published result, so a requester can price its own job
  without reading the database.
- Balance from `GET /user/balance`, refreshed on startup and after a 402, which
  is surfaced as an empty account rather than a failure and stops the pool
  rather than failing each queued request in turn.
- Model, effort, and the price table's capture date in the session header,
  populated from `GET /models`.
- Queue health on the session list: consumer lag, in-flight count, redelivery
  count.
- Session resume and delete from the CLI; the browser only lists. Reload-mid-run
  verified against a live queue-driven run rather than a synthetic one, with
  several sessions in flight.

**Exit criteria.** v1. A run states its own cost, a deliberately churned prefix
is caught by the diagnostic and named to the specific message, and a published
result carries figures that match the database.

**Effort.** Medium.

---

## Threaded through, not phased separately

**Cache work** does not get its own phase because it cannot. The invariants are
structural and land in phase 2 — frozen prompts, append-only folds, stable tool
arrays, identical retry bytes. The diagnostic that proves they hold lands in
phase 6. Treating cache as a later optimisation pass would mean rebuilding the
fold.

**Concurrency** is likewise structural. Phase 2 builds a session runner that
holds no shared mutable state and a store that serialises its writes; phase 3
adds the pool that exercises it. Retrofitting concurrency onto a runner that
assumed one session would mean rewriting the loop, the store, and the churn
diagnostic together.

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
| ~~Does pro honour `low` effort yet?~~ | Closed 2026-08-09 | No. `low` and `high` produced reasoning within 1% on a real task; `max` was 2.4× both |
| ~~Is the `[1m]` suffix real on the native endpoint?~~ | Closed 2026-08-09 | No. `GET /models` returns only `deepseek-v4-flash` and `deepseek-v4-pro` |
| ~~Do streaming tool-call deltas arrive incrementally?~~ | Closed 2026-08-09 | Yes, OpenAI indexed form. Arguments fragment mid-token |
| ~~Is the prefix warmup worth its two probe requests?~~ | Closed 2026-08-09 | No. 128-token blocks persist from any single request, so there is nothing to warm |
| Are the Claude Code tool names the right vocabulary? | Phase 2 | Tool-call error rate against a rename, which is cheap |
| Does changing effort mid-session disturb the cache? | Phase 2 | Hit rate across an effort change |
| Flash or pro for the main loop? | Phase 2 | Same task both ways. Flash-0731 beats V4-Pro-Preview on published agent benchmarks |
| Is `max` effort worth 2.1× the wall-clock over `high`? | Phase 2 | Quality comparison on real tasks. Cost and latency are already measured |
| Does the model call `Complete` reliably when it cannot be forced? | Phase 3 | Rate of runs ending without it across a batch of queued jobs |
| What worker pool size does one process hold? | Phase 3 | Raise it against a fixed batch until wall-clock stops improving. SQLite writes and API concurrency are the two candidate ceilings |
| Do concurrent sessions actually share the head? | Phase 3 | `prompt_cache_miss_tokens` on the first request of the second session |
| Is virtualisation needed? | Phase 5 | Frame times with a few hundred blocks |

---

## Sequencing notes

Phases 1 to 3 together are the deliverable that matters — a queue-driven
DeepSeek coding agent running several jobs at once, with every conversation in
the database and on disk. Phases 4 to 6 make it watchable and legible. If value
is wanted early, stop at 3 and read transcripts off disk while the browser gets
built.

Phase 2 is where the DeepSeek-specific correctness concentrates: the
`reasoning_content` round-trip, the non-null tool-call content, the append-only
fold, the ordered tool results. Errors there surface as 400s and silent cost
blowouts rather than as obvious breakage, which is why the exit criterion is a
cache measurement and not just a working demo.

Phase 3 comes before the browser deliberately. Building the frontend against a
harness that already runs many sessions avoids designing a single-session view
and then retrofitting a list into it.

Phases 5 and 6 can run in either order, or in parallel if there are two people.
Neither depends on the other.
