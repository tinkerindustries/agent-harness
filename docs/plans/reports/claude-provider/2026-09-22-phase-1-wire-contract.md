# Phase 1: wire contract for claude-session

## What was built

`docs/STDIO-MANAGED-AGENTS.md` — the protocol reference `harness
claude-session` will be built from, in the shape of `docs/STDIO-INTERACTIONS.md`
and covering the same contents list (adding "Framing" back after an initial
draft omitted it, and adding two sections beyond that list: "Client-declared
(custom) tools" for the async flow, and "The seam" for the plan's required
`Dialect`/`Translator`/`Server` inventory).

It settles the method mapping (`sessions.create` / `sessions.events` /
`sessions.get` / `sessions.delete`, no `sessions.cancel` — interrupt is a
`user.interrupt` event), the unit-of-work model (session id as the one
address a client holds for the conversation's whole life, plus a
`harness.turn_id` per turn), model/system selection
(`agent: {type: "agent_with_overrides", model: {id, effort}}`, every other
agent field refused, `environment_id` refused in favour of `harness.cwd`),
the notification set (`session.status_*`, `span.model_request_*`,
`agent.message`/`.thinking`/`.tool_use`/`.tool_result`/`.mcp_tool_use`/
`.custom_tool_use`, `session.usage`, `session.error`,
`event_start`/`event_delta`, `harness.tool_output`), client-declared tools
(Anthropic's own async `agent.custom_tool_use` + `user.custom_tool_result`
shape, not the blocking `harness.function_call` the other two dialects use),
the seam changes `internal/stdiosession` needs, and Deviations/Not built
sections in the house style.

## Sign-off quoted

Sent as a three-part proposal (method mapping, unit of work, seam changes)
to the orchestrator before writing the document, per the plan's
"replaces the plan's 'put it to the user'" instruction. The reply, quoted in
full because the document is built to it:

> 1. Methods: AGREED as proposed. That covers sessions.create /
> sessions.events / sessions.get / sessions.delete, interrupt as a
> user.interrupt event with no cancel method, agent_with_overrides carrying
> model only, every other agent field and environment_id refused, and the
> status mapping.
>
> 2. Unit of work: CHANGED. The user wants a turn id exposed as well as the
> session id. Keep the session id as the conversation's address, and keep
> user.message doing double duty: start a turn when idle, steer when
> running. Add a harness-level per-turn id, so get and cancel can address a
> single turn. Design how it's spelled: which frames carry it, how a client
> names it on sessions.get, and how interrupting a specific turn is spelled.
> Keep it inside the `harness` extension block, or as a documented
> deviation, so Anthropic's own shapes stay unaltered. Resume after a
> restart stays as you proposed (sessions.create + harness.resume_session_id).
>
> 3. Client-declared tools: CHANGED to the ASYNC, faithful shape.
> agent.custom_tool_use goes out as a notification, the session goes idle
> with stop_reason {type: requires_action, event_ids}, and the client
> answers with a user.custom_tool_result event through sessions.events.
> Specify it fully in the doc: ordering, what happens if the client sends
> user.message or user.interrupt while a tool result is pending, and a
> timeout or none. Also add a section naming what session.Runner needs to
> pause on a client tool call and resume when the result arrives. Phase 4
> will build that, and I'm widening phase 4 in the plan to say so.
>
> 4. Seam: AGREED. Adjust it for the turn id and the async tools as needed,
> and name any extra seam change they force.

`harness.turn_id` (point 2), the async `agent.custom_tool_use`/
`user.custom_tool_result` flow with its ordering, pending-input and timeout
rules (point 3), and the widened seam section (points 3 and 4) are all in
the document as written.

## What was verified, and how

This is a documentation-only phase; there is no `scripts/build.sh` gate and
no Go to compile. Verification was: reading `internal/stdiosession/dialect.go`,
`translator.go`, `interactions.go`, `resume.go`, `server.go`, `methods.go`,
`hosttools.go` and `interactionstranslate.go` in full, so every shape and
every seam claim in the document is checked against what the existing two
dialects' code actually does today rather than against `STDIO-INTERACTIONS.md`'s
prose alone (e.g. `RunView` already carrying both `ID` and `SessionID`,
confirmed by reading `translator.go` directly, which is why the document can
say the `Translator` interface needs no change). Every Managed Agents shape
used is checked against the live pages fetched during this run
(`overview`, `sessions`, `events-and-streaming`, `tools`,
`session-operations`), not against training data, and the document cites
each one to the page it came from.

`grep -n "^## "` confirms every section in `STDIO-INTERACTIONS.md`'s own
contents list has a counterpart heading in the new document (`Starting the
process`, `Framing`, `The handshake`, `Methods`, `Notifications`, `Ordering
guarantees`, `Tools`, `Resuming across process restarts`, `Permissions`,
`Errors`, `Lifecycle`, both `Deviations` sections, `Not built`), plus two
sections the plan's phase-1 bullet list asked for by name that
`STDIO-INTERACTIONS.md` has no counterpart for at all ("Client-declared
(custom) tools", "The seam").

## What was left undone

Nothing in scope. No Go code was written, as the phase requires. The
porting table in `docs/STDIO-PROTOCOL.md` and the other docs `CLAUDE.md`/
`ARCHITECTURE.md`/`internal/CLAUDE.md` name for phase 4 were not touched.

## Bugs found and not fixed

None — no code was read for correctness, only for shape.

## Mechanical friction

None worth recording. The four live-doc fetches and the two sign-off
round trips were the only points this run needed to wait on something
outside the repository.

## Memory suggestions

- The single biggest fact phase 4 needs and won't get from re-reading the
  plan: **`internal/anthropic` (phase 2) never calls Anthropic's real
  `POST /v1/sessions`.** `claude-session`'s Managed Agents vocabulary is a
  parent-facing wire convenience only — the loop underneath calls the plain
  Messages API, the same way `gemini-session` puts Interactions vocabulary
  on the pipe while the loop still runs entirely locally. A phase 4 agent
  that assumes there is a real session resource to reconcile against will
  waste time looking for one.
- The async custom-tool-result flow (sign-off point 3) needs a real
  architecture decision phase 1 could not make: what `internal/tools`'
  existing per-tool execution timeout actually is, and how to exempt (or
  widen) it for a pending client tool call under this dialect alone,
  without changing the timeout every other tool call gets under every other
  dialect. `docs/STDIO-MANAGED-AGENTS.md`, "The seam", names the shape of
  the fix but deliberately does not assert the mechanism — that needs
  reading the actual code, which this phase did not do.
- `docs/STDIO-MANAGED-AGENTS.md` asserts `is_error` exists on
  `user.custom_tool_result` by inference from the Messages API's own
  `tool_result` content block (which `docs/managed-agents/tools` cites
  custom tools as directly analogous to), not from a page that names the
  field explicitly. The document flags this inline; phase 4's live check
  should confirm it against a real payload before the dialect ships.
