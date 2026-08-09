# Initial implementation plan

Ordered by dependency: each step needs the one before it, and each ends with
something that runs and can be exercised by hand. The order is not a
risk-reduction sequence — there are no spikes here, every step is production
code in its final home.

Design decisions referenced by section number live in [DESIGN.md](DESIGN.md).

## 1. Module skeleton and DeepSeek client

Go module, directory layout, config loading. API key from `DEEPSEEK_API_KEY`,
base URL, default model, and the price table all from config (§4.9).

Request and response types as structs, not maps, so serialisation is byte-stable
(§3.2). Non-streaming `POST /chat/completions` against `https://api.deepseek.com`,
plus `GET /models` and `GET /user/balance`.

`thinking` and `reasoning_effort` wired through from config, defaulting to
enabled and `max` per [MODELS.md](MODELS.md). `GET /models` also settles whether
the `[1m]` suffix is real on the native endpoint.

The request-shape rules from §4.4 belong here, not later: no `tool_choice`,
`max_tokens` set explicitly, `system` rather than `developer`. They are one-line
each in the client and a class of 400 if missed.

Ends with: a one-shot CLI that sends a prompt and prints `content`,
`reasoning_content`, and the full `usage` block including cache hit and miss
counts, plus subcommands that list models and show the balance.

## 2. Streaming client

SSE reader: large-line handling, `:` comment filtering, idle watchdog, `[DONE]`
termination, transport-level timeouts with no client-level one (§4.3).
`stream_options.include_usage` on every request.

Typed deltas onto a channel. The tool-call assembler, written for the indexed
incremental form.

Ends with: the CLI streams both channels to stdout and prints usage at the end.
The real shape of tool-call deltas becomes observable here.

## 3. Session store and event log

SQLite schema through `modernc.org/sqlite`: events keyed by `(session_id, seq)`,
plus session metadata. Append and replay.

The fold from event log to a DeepSeek `messages` array, preserving
`reasoning_content` verbatim on every assistant message (§3.1) and emitting `""`
rather than `null` for tool-call assistant content (§4.4).

The session row stores its rendered system prompt and tool schema, frozen at
creation, so a harness upgrade cannot change a resumable session's prefix
([CACHE.md](CACHE.md)).

Ends with: unit tests over the fold, covering a plain turn, a tool-call turn, a
parallel-tool-call turn, and a multi-turn session carrying turn-1 reasoning into
turn 2. One test asserts the fold is append-only — folding N events then N+1
events yields identical bytes for the first N.

## 4. Tool layer

The ten tools in [TOOLS.md](TOOLS.md), schemas matching the trained-in shape,
arguments validated in Go. Workspace root confinement with post-symlink escape
checking. Per-tool timeouts and output caps with explicit truncation markers.

Build the file and search tools first — `Read`, `Write`, `Edit`, `Glob`, `Grep`,
`List`, `Bash`. `TodoWrite` is a state write with no side effects. `Task` needs
the agent loop, so it lands in step 5, and `WebFetch` needs a flash call, so it
lands with it.

`Edit` carries the most rules and deserves the most tests: unique-match
enforcement, the prior-read gate, the line-number-prefix error, and the diff in
the result.

Ends with: tools exercised directly by tests, no model involved. Escape attempts
rejected, timeouts fire, caps truncate and label.

## 5. Agent loop

Sub-turn iteration until a response arrives with no tool calls. Parallel tool
calls executed concurrently but appended in `tool_calls` order. Approval gating
as a blocking event. Retry and backoff by status class (§4.5). Per-model
semaphore.

`Task` and `WebFetch` land here, since both spawn a nested flash conversation
whose transcript stays out of the parent message array.

Compaction at 768K: fork a new session seeded with a summary in the new
session's system prompt, linked to the parent (§3.2).

Cache discipline lands here because here is where requests are built: serialise
once and retry the identical buffer, and record expected-miss against actual
`prompt_cache_miss_tokens` per sub-turn with the churn-point diff
([CACHE.md](CACHE.md)).

Ends with: the harness works end to end from the CLI. Give it a task in a scratch
directory and it edits files and runs commands until done, and the log shows
cache miss tokens matching the appended content on every sub-turn after the
first.

## 6. HTTP server

SSE endpoint with `Last-Event-ID` replay. Command POSTs for prompt, approve, and
cancel. Session list and creation. `embed.FS` for the built frontend, with a
dev-mode passthrough to the Vite server.

Ends with: `curl` against the SSE endpoint shows a live run, and disconnecting
and reconnecting with a `Last-Event-ID` replays cleanly.

## 7. Frontend shell

Vite, React, TypeScript. The external store and its `useSyncExternalStore`
bindings. SSE client with reconnect and resume. The event-to-view-model fold,
mirroring the Go fold in shape.

Ends with: events render as unstyled text in the browser, and a reload mid-run
picks the stream back up.

## 8. Transcript rendering

Frozen memoised blocks. The `requestAnimationFrame`-coalesced live tail.
Plain-text-while-streaming, parse-and-highlight-on-completion. The collapsing
reasoning panel.

Measurement belongs here, because here is the first point where there is
something to measure: a synthetic delta feed at a fixed rate, frame times
recorded with a few hundred blocks mounted. That number answers the
virtualisation question from §5.5.

Ends with: a readable transcript that holds frame rate under a synthetic
high-rate stream.

## 9. Tool UI

Tool call and result blocks, one shape per tool. Streaming command output. The
approval prompt wired to the blocking event from step 5, with permission modes.
Collapsed large outputs with expand. Diff rendering from the structured line
arrays Go sends.

The plan panel, pinned beside the transcript and driven by `TodoWrite`. Subagent
runs from `Task` render as a collapsed child transcript rather than inline.

Ends with: a full agent run is legible in the browser, and commands can be
approved or refused from the UI.

## 10. Cost, cache, and model selection

Per-turn and per-session token counts by category, cost derived from the config
price table, cache hit rate displayed prominently. Account balance from
`GET /user/balance`, refreshed on startup and after a 402.

The model and effort controls in the session header, populated from
`GET /models`. A model change shows its cache-miss estimate before it applies —
context size from the last `usage`, rate from the price table. Effort controls
hide `temperature` and `top_p` while thinking is enabled.

The churn diagnostic surfaced: miss tokens, expected miss tokens, and the index
of the first message that differs from the previous request when those two
disagree.

Ends with: a run shows its own cost, a deliberately churned system prompt is
caught by the diagnostic and named to the message, and switching model states
its price beforehand.

## 11. Session management

List, resume, and delete. Reload-mid-run verified against a live agent run rather
than a synthetic one.

Ends with: v1.
