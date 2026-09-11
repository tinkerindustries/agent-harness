# DeepSeek Harness (dsh) against this harness

## What was read, and how the vocabularies map

Compared against `deepseek-ai/deepseek-harness` at commit `47f9438`, version
`0.1.0-rc.5`, dated 2026-08-13, whose tip commit is the merge of PR #2519
(**observed**, `package.json:3` and `git log`). MIT, TypeScript, built on the
vendored Cordis plugin framework. It is a developer preview: "DeepSeek Harness
is currently in _developer preview_ and is iterating rapidly. **THERE WILL BE
COMPATIBILITY-BREAKING CHANGES.**" (**observed**, `README.md:11`).

Scale, because it bears on every judgement below. `dsh` is 219 packages and
198,402 lines of TypeScript under `packages/*/src` alone; this harness is
28,816 lines of non-test Go across 22 packages (**observed**, `find`/`wc`).
`dsh` is roughly seven times the size, and that is before `apps/`, `website/`,
`native/`, `python/` and `vendor/`.

**Vocabulary.** `dsh` defines: "A **step** is one model request plus the tools
it calls. A **turn** is zero or more steps: it opens before its first input is
claimed and closes once nothing is owed." (**observed**,
`docs/architecture.md:65`).

The mapping is:

| `dsh` | this harness |
| --- | --- |
| **step** | **sub-turn** (`docs/DESIGN.md` §5.9) |
| **turn** | no unit — the closest thing is the whole run between operator inputs |
| session | session |

A naming collision is worth flagging before it misleads anyone: this harness's
event kind is called `turn_started` (**observed**,
`internal/store/events.go:10`) but its payload field is `SubTurn`
(**observed**, `internal/store/events.go:93`), so the event named "turn" fires
once per `dsh` *step*, not once per `dsh` *turn*. Throughout this document,
`dsh` step ≡ this harness's sub-turn.

---

## 1. The session log

**They hold adjacent invariants, not the same one. `dsh` guarantees
reconstructability; this harness guarantees reconstructability *plus*
monotonicity.**

### What `dsh` guarantees

`dsh`'s stated rule is that model-visible means logged: "The request envelope —
the `EpochHeader` (call config + markers for adapter-supplied defaults +
rendered system prompt + assembled tool schemas) — is logged session state, so
every conversation request is a pure function of the log" (**observed**,
`docs/subsystems/session.md:156`).

The projection is genuinely pure and verbatim. `deriveEventMessage` is
documented as "THE per-node projection rule: `Session.deriveMessages` folds it
over the live surface, external reconstructors and pure projections fold the
same function over a log prefix's surface to rebuild the exact messages any
request was built from. The returned message is the already frozen message
nested in the event wrapper and shared by delivery, durable history, and model
requests." (**observed**,
`packages/core/session/src/surface.ts:74-79`). `user/message`,
`assistant/message` and `tool/result` return `event.data` unchanged
(**observed**, `packages/core/session/src/surface.ts:96-108`); everything else
projects to null. `deriveMessages()` is at
`packages/core/session/src/index.ts:726` (**observed**).

Because the derived message is the *same frozen object* that was delivered, not
a re-rendering of it, `dsh`'s derivation is at least as deterministic as this
harness's — the TypeScript/Go key-order worry does not actually bite here
(**inferred** from the "already frozen message" wording at `surface.ts:78`).

### Where they diverge

`dsh` has a first-class notion of a surface event that *replaces* a range
rather than appending: `isReplacementSurfaceEvent` returns "true when the event
replaced a surface range" (**observed**,
`packages/core/session/src/surface.ts:61-68`). This harness has no such
concept. Its fold's package doc states the stronger property outright: "Folding
`events[:n]` and `events[:n+1]` for any n therefore never disagrees on a
message both include — folding more events only appends, never rewrites"
(**observed**, `internal/fold/fold.go:6-9`).

Likewise the frozen head. This harness renders the system prompt and tool
schema once at session creation, stores them on the session row, and folds from
there — `Fold` takes `sess store.Session` and seeds the array with
`wire.SystemMessage(sess.SystemPrompt)` (**observed**,
`internal/fold/fold.go:52-53`). `dsh` also logs its rendered head, but as a
*versioned snapshot*: "A full `request/header` snapshot with reason `'initial'`
or `'resume'` records each loop-instance boundary; a later changed request
records another full snapshot with reason `'change'`. `foldRequestHeader(events)`
reconstructs the header by selecting the latest snapshot." (**observed**,
`docs/subsystems/session.md:156`). So `dsh` can reconstruct which head was used
for any past request, but the head is explicitly allowed to change mid-session.
This harness forbids that by construction.

### Byte-identical reconstruction

**This harness: yes, and it is machine-checked.** `TestAppendOnly` folds every
prefix `events[:n]` for every n, `json.Marshal`s each resulting message, and
fails if any message differs from the corresponding message in the full fold
(**observed**, `internal/fold/fold_test.go:405-469`, assertion at `:465-467`).
That is a byte-identity guarantee over the entire prefix lattice, not a
convention. It is reinforced by the tool-result path taking its content purely
from the event payload "never from the filesystem, so replaying the log
reproduces the identical bytes no matter what happened to the file since"
(**observed**, `internal/fold/fold.go:27-30`), and by golden files pinning the
head and request body (**observed**, `internal/tools/testdata/tools_*.golden.json`,
`internal/wire/testdata/request_body.golden.json`).

**`dsh`: byte-identical for a *fixed* log, but not across a growing one.**
Replaying a given log prefix rebuilds "the exact messages any request was built
from" (**observed**, `surface.ts:76-77`). But a later `replace` surface event
means folding a longer log legitimately yields a *different* array for the same
early positions (**observed**, `surface.ts:61-68`; mechanism in §2). I found no
`dsh` equivalent of `TestAppendOnly` — no test asserting prefix-fold stability
(**inferred**; the file I would read next is
`packages/core/session/tests/` in full, which I did not exhaust).

### Resume

- **`dsh`**: seeding a session with an existing event log is the resume/fork
  primitive — `ctx.sessions.create(id, { seed, meta })`, with
  `fork(source, boundary?, childSessionId?)` as the policy API over it, which
  "requires the selected prefix to end outside an open turn" (**observed**,
  `docs/subsystems/session.md:534-536`). A seeded session writes a
  `session/end-seed` marker as its first live write, because "seed history and
  live work are otherwise byte-identical" (**observed**,
  `docs/subsystems/session.md:585,589`). Resume re-snapshots the header with
  reason `'resume'` (**observed**, `docs/subsystems/session.md:156`).
- **This harness**: resume replays the event log against the session row's
  *stored* prompt and schema, so a harness upgrade cannot alter the prefix of a
  resumable session (**observed**, `internal/fold/fold.go:52`, and
  `docs/DESIGN.md` §4.8 / `docs/CACHE.md` "Freeze the system prompt and tool
  schema into the session at creation").

`dsh`'s fork story is richer — this harness has nothing equivalent to forking a
live session at an arbitrary boundary. That is a genuine `dsh` capability this
harness lacks (**observed**).

---

## 2. Compaction and the cache

**This is the decisive question, and the answer is more interesting than
"`dsh` ignores the cache". `dsh` is systematically cache-aware — and then
chooses replacement compaction anyway, with its eyes open.**

### `dsh` is far *more* systematic about cache documentation than this repo

215 package READMEs in `dsh` carry a mandated `#### KV Cache effect` section
(**observed**, `grep -rl "KV Cache effect" packages --include=README.md | wc -l`).
The discipline is real, not decorative. Samples (**observed**):

- `packages/context/time-context/README.md`: "Append-only; newly visible
  content follows the reusable request prefix and does not invalidate existing
  KV-cache entries." — i.e. `dsh` puts the clock in an appended context
  message, the same conclusion this harness reached by forbidding clocks in the
  head.
- `packages/llm/llm-retry/README.md`: "The reconstructed request preserves the
  prior prefix and is eligible for provider cache reuse" — the same rule as this
  harness's "serialise once and retry the same bytes".
- `packages/llm/llm-deepseek/README.md`: "Changing the provider or model
  selects a different cache domain."

Any assumption that DeepSeek's own team built a harness naive about DeepSeek's
own prompt cache is wrong, and this document should say so plainly.

### But compaction *does* rewrite history, and says so

Compaction appends three log-only events (`compaction/start`,
`compaction/summary`, `compaction/end`) — those are append-only and do not
reach the model. The mutation is separate: "the summary itself rides on a
separate `user/message` with `surfaceOp: { op: 'replace', start, end }` — the
only surface mutation performed by summary compaction" (**observed**,
`docs/subsystems/compaction.md:9`).

Its own cache verdict is explicit: "**Replacing rather than append-only.** Each
checkpoint invalidates reuse from the first replaced history token; the
unchanged request prefix before that range remains reusable." (**observed**,
`packages/compaction/compaction-basic/README.md`, "KV Cache effect").

`compaction-tool-result-pruner` is the sharper case, and it does exactly what a
prefix cache cannot survive: "Replacing an earlier result invalidates reuse
from the first changed token. The pruned prefix is eligible for reuse while its
route, envelope, and preceding history remain identical." (**observed**,
`packages/compaction/compaction-tool-result-pruner/README.md:56`). The rewrite
is a durable, validated event, not a silent edit — the session invariant
explicitly tolerates it: "Session has already validated a content rewrite that
cites its replaced event." (**observed**,
`packages/core/session/src/invariant.ts:129-136`). So auditability is
preserved; cache reuse is the casualty.

### How often this fires

Not rarely. `compaction-basic` defaults to `auto: true` — "Register
step-boundary pressure and overflow-recovery listeners" (**observed**,
`packages/compaction/compaction-basic/README.md:41`) — with `thresholdRatio`
defaulting to `0.8`: "Compact at `floor(routedContextWindow × ratio)`"
(**observed**, `:32`). It retains a recent verbatim window and replaces the
older range (**observed**, `:89`), and retries up to `compactionRetries` if the
result does not fit (**observed**, `:17`).

### Verdict

**Structurally incompatible with this harness's cache design, but the damage is
bounded and the two teams differ on frequency and visibility, not on physics.**

- Both designs reset the cache. Neither claims otherwise.
- The head survives in both: `dsh`'s "unchanged request prefix before that
  range remains reusable" holds under DeepSeek's whole-unit semantics too,
  because the 128-token units before the replacement point are still wholly a
  prefix of the new request (**inferred**, from `docs/CACHE.md`'s statement of
  the mechanism plus `compaction-basic`'s claim).
- What differs is **where the reset happens and how often**. This harness
  resets at a *session boundary*, visibly, at 768K tokens (`docs/DESIGN.md`
  §3.2). `dsh` resets *mid-session*, invisibly to the model, at 80% of the
  routed context window, repeatedly, and additionally on every tool-result
  prune (**observed**, citations above).
- The measured stake here is not hypothetical. Aggregate prompt-cache hit rate
  across the 20 most recent sessions on the running production stack is
  **99.47%**, top session 99.77% (**observed**, computed from
  `GET http://127.0.0.1:8180/api/sessions` on 2026-08-15). The brief's ~98.5%
  is if anything conservative.

Adopting `dsh` wholesale means accepting `compaction-basic` and the pruner, or
replacing them. They are optional plugins — compaction "is **one optional
capability**, not part of the agent-loop spine" (**observed**,
`docs/subsystems/compaction.md:5`) — so replacing them with a
start-a-new-session backend is *permitted* by the architecture. That is the
single most important compatibility finding in this document: the seam exists,
and the default filling of it is wrong for this harness's economics.

---

## 3. Is there a multi-session service in there?

**No. Plainly: `dsh` has nothing equivalent to this harness's durable work
queue plus worker pool.** It has the *pieces* to run many sessions in one process, and an
SDK for driving a runtime from outside, but no durable work intake, no
distributed queue, and no result stream.

What is actually there (**observed**, via a dedicated read of each package):

- **`packages/api/gateway`** — a two-sided Typert RPC transport between one
  host Cordis process and its clients, dispatch "unary only"
  (`packages/api/gateway/README.md:9,40`). A same-process RPC bridge for the
  browser client, not a job intake.
- **`packages/api/remotes`** — a BFF selecting which business remote methods
  the client mounts, owning agent/session identity policy such as resuming a
  cold session (`packages/api/remotes/README.md:5,7,11`). "Remote" as in
  remote-procedure-call, not remote submission.
- **`packages/jobs`** — background *tool* work inside one session, and it says
  so: "**The contract is in-process** — `JobStart.run()` passes callbacks and
  exact `Agent` objects; a durable or cross-process backend must reshape
  identity, restart, ownership, and observation semantics before it can
  implement this seam" (`packages/jobs/jobs/README.md:40`), and "**Jobs are
  process-local** — records die with the harness process"
  (`packages/jobs/jobs-local/README.md:33`).
- **`packages/schedule`** — cron-like reminders, but "**Session-local delivery
  only** — a reminder runs on time only while its original Session is live; a
  cold Session receives no external notification"
  (`packages/schedule/schedule/README.md:111`).
- **`packages/bundle/headless`** — strictly one-shot: it "creates one fresh
  persisted Agent… submits the task as an ordinary user message, and waits for
  quiescence… requests exit", with "**One submitted task only**… no interactive
  follow-up surface" as a stated limitation
  (`packages/bundle/headless/README.md:7,19`). No listening port opens.
- **`packages/bundle/web-app`** — mounts an HTTP server only to serve its own
  browser client and the gateway RPC bridge (`packages/bundle/web-app/README.md:5`).

Grepping `packages/` and `docs/` for work queue, worker pool and message
dispatch returns nothing related to job dispatch (**observed**).

**How far it partly goes, stated exactly.** Two things do exist and matter:

1. **Many live agents per process is a designed-for pattern.** `ctx.agents` is
   the "live registry" and `list()` "returns a fresh array" of live agents
   (**observed**, `docs/subsystems/core.md:16,710`). `ctx.workspaceRegistry`
   owns "persistent workspaces: user directories with titles and **ordered
   session membership**" (**observed**, `packages/workspace/README.md:5`). So
   the concurrency model is not the obstacle; the *intake* is.
2. **There is an out-of-process driving SDK** — which the brief did not name and
   which changes the hybrid calculus. `packages/sdk` is "the protocol stack for
   driving a Harness runtime from another process" (**observed**,
   `packages/sdk/README.md:1`), with a TypeScript client that drives "a
   DeepSeek Harness runtime as a subprocess over stdio JSON-RPC" (**observed**,
   `packages/sdk/client/README.md:5`) and a Python twin distributed as
   `deepseek-harness-sdk` with bundled runtime binaries (**observed**,
   `python/README.md:5,11-12`).

So the honest summary is: `dsh` gives you a *runtime* you can drive
programmatically, one subprocess at a time, and expects the queue, the
durability, the retry discipline, the idempotency and the result fan-out to
live outside it. Everything in `internal/queue` and `internal/worker` — the
`work_queue` table, the pool as flow control, the single-use
`request_id` row, result-then-ack ordering — has no counterpart and no home
in `dsh`.

---

## 4. Could this repo's distinctive parts survive as `dsh` plugins?

Taken one at a time, they rank very differently. One is easy, one is
awkward-but-possible, one has no home at all.

### (a) The durable work intake and worker pool — expressible, but it is not a plugin, it is a host

**Seam**: `ctx.agents` (`create()` / `resume()` / `list()`, **observed**,
`docs/subsystems/core.md:24,51,710`), plus `session/event` for progress and
`ctx.workspaceRegistry` for per-session directories.

**Verdict**: expressible in principle, and the concurrency model does not fight
it (§3). But what you would write is not a plugin that extends `dsh` — it is a
process that *owns* `dsh`, holding the queue consumer, calling
`ctx.agents.create()` per delivery, and translating `session/event` into
progress publishes. Everything that makes the current worker correct —
heartbeating the lease, recording the terminal result *before* acking, the
single-use `session_id` discriminator
that stops a redelivered request re-cloning and re-spending — is ack-discipline
logic that has no `dsh` vocabulary to reuse (**observed**, `docs/DESIGN.md`
§4.10; **inferred** for the absence of a counterpart, from §3's package sweep).

**Cost**: a new host application plus a queue consumer, in TypeScript, with the
whole of `internal/worker`'s idempotency reasoning rewritten and re-tested. The
alternative — drive `dsh` subprocesses from the *existing* Go worker over the
SDK's stdio JSON-RPC (**observed**, `packages/sdk/client/README.md:5`) — is
strictly cheaper and keeps the ack discipline that already works. That is the
hybrid boundary the recommendation draws.

### (b) The vision path — the easy one; `dsh` is a better host for it than this harness

**Seam**: `ctx.tools` for registration (**observed**,
`docs/architecture.md:111`), and a *second route* on `ctx.llm` for the Gemini
call. The key enabling fact: the provider is a **per-call** field —
`GenerateOptions.provider` is the "Provider route key" (**observed**,
`docs/subsystems/llm-streaming.md:352`), and adapter registration returns a
handle whose routes can be replaced atomically (**observed**, `:325-345`). So
one process can hold a DeepSeek route and a Gemini route simultaneously and
pick per call. `ctx.llm` does **not** assume one adapter per session
(**observed**).

**And `dsh` needs the workaround less.** `ImageBlock` is a first-class content
block backed by a durable image attachment (**observed**,
`docs/subsystems/llm-streaming.md:27,32`), with a whole `packages/attachment`
family behind it. Where this harness must route screenshots to Gemini for a
model with no native vision — `deepseek-v4-pro`, which it does not currently
route at all ([DEEPSEEK-VISION.md](DEEPSEEK-VISION.md)) — `dsh` already has
the vocabulary to carry an image to any adapter that accepts one.

**Cost**: low — one package registering a tool on `ctx.tools` plus a Gemini
adapter registration on `ctx.llm`. This is the one component that would be
*cleaner* as a `dsh` plugin than it is today. (**inferred** on cost; I did not
find an existing in-tree tool that calls a second adapter mid-execution, so
there is no precedent to copy — the file I would read next is
`packages/core/tools/src/` for whether a tool's `execute` receives a context
with `ctx.llm` in scope.)

### (c) The eval comparison harness — no home, and `dsh` agrees

**Seam**: none. There is **no eval or benchmark package anywhere in
`packages/`** (**observed**, glob for `*eval*` returns no matches).

More telling is what `dsh` does instead. Its entire benchmarking instruction is:
"Follow [Get started with the Python SDK] to install the SDK and run the
`jsonrpc-agent` minimal variant. Use separate workspaces and session IDs for
independent benchmark tasks." (**observed**, `BENCHMARK.md:3`). That is
architecturally the *same answer* this harness already implements — publish a
suite of tasks, run each as its own session in its own workspace, score
afterwards — except performed from outside the harness by a driver script,
which is what `internal/evals` is.

**Cost**: this component does not port to a plugin because in `dsh`'s own model
it was never supposed to be one. It stays outside, and the only question is
whether the thing it drives is `harness publish` or the `dsh` SDK.

---

## 5. `agent/pre-step` against steering

**Different mechanisms for different jobs. `agent/pre-step` is the more
powerful seam; this harness's steering is the safer one, and for this
harness's economics safer is better.**

The mechanics, both verified in source:

- **This harness**: `pickUpSteers` (`internal/session/turn.go:145-185`) is
  called from `runSubTurn` at `turn.go:204`, immediately *before*
  `fold.Fold` at `turn.go:208`. It reads `steer_message` events past the
  applied high-water mark and appends one `steer_applied` per steer, linked by
  `SourceSeq`. It never touches previously committed content (**observed**;
  design in `docs/RUN-CONTROL.md:321-452`).
- **`dsh`**: `agent/pre-step` is a waterfall dispatched at
  `packages/core/agent-loop/src/agent.ts:234-239`, once per step from `turn()`
  at `agent.ts:266`. `agent.inject()` is `agent.ts:130-132` —
  `this.send(input, 'next-step', false)` — the same `next-step` inbox target
  `steer()` uses at `agent.ts:126-127`, differing only in `wakeup`
  (**observed**).

**(a) Timing.** Effectively identical, contrary to what the docs' phrasing
suggests. For an already-running loop both land at the next step boundary. The
`wakeup` flag is the only difference and it only matters when the driver is
*idle*: `inject` (`wakeup: false`) will not restart a stalled loop, `steer`
(`wakeup: true`) will (**observed**, `agent.ts:126-132`,
`packages/core/agent/src/runtime-types.ts:123-140`). This harness's steer is
equivalent to `dsh`'s `steer`, not its `inject`.

**(b) Expressiveness.** `dsh` is strictly more powerful. `PreStepDecision` is
`{kind:'reject'}` or `{kind:'enter', messages}` (**observed**,
`packages/core/agent/src/runtime-types.ts:231`), so a listener can cancel the
step entirely — the turn closes with no model call (**observed**,
`agent.ts:267-270`) — or substitute an arbitrary messages array, not merely the
claimed batch. This harness's steering has no reject path and can only append
verbatim operator text (**observed**, `turn.go:170-172`). That extra power is
what makes `dsh`'s compaction, gating and context-injection plugins possible
through one seam; it is also what makes them able to break the prefix.

**(c) Auditability.** `dsh` logs the inbox mechanics thoroughly — every
insertion and claim is a durable `agent/inbox/spliced` event (**observed**,
`packages/core/agent/src/inbox.ts:186`) — but not the *decision*: a rejected
step records `turn/end` with `{ kind: 'blocked' }`, carrying no listener
identity and no reason (**observed**, `docs/subsystems/session.md:558`,
`agent.ts:268`). Recovering "what did a listener change" means diffing claimed
against entered. This harness's two-kind split exists precisely to make that
legible: `steer_message` is "an operator sent this", `steer_applied` is "the
model was shown this, here", and the pair is what lets a resumed run recompute
outstanding steers from the log alone (**observed**,
`docs/RUN-CONTROL.md:332-384`; the awkward interleaving it defends against is
documented in the test itself, `internal/fold/fold_test.go:416-428`).

**(d) Cache safety.** This is the asymmetry that decides it. A `pre-step`
listener cannot retroactively edit an already-committed prefix — `deriveMessages`
walks immutable committed events — so the risk is not literal history
corruption (**observed**). But nothing in the type prevents a listener from
dropping or reordering messages such that the next request's array is no longer
an extension of the last one; correctness rests on the documented convention
that listeners "preserve downstream messages unless replacement is intentional"
(**observed**, `docs/agent-lifecycle.md:78`) — a convention, not an enforced
invariant. This harness's steering *cannot* express the unsafe case: appending
is the only operation available, which is why `TestAppendOnly` can prove the
property rather than assert it by review (**observed**,
`internal/fold/fold_test.go:405-469`).

**Verdict**: `agent/pre-step` is the better *framework* seam — it is one hook
that serves interception, policy, compaction and injection, and this harness
would need three or four separate mechanisms to match it. This harness's
steering is the better *production* seam for a harness whose unit economics
rest on a 99.47% cache hit rate, because it makes the cache-breaking case
structurally unrepresentable instead of merely discouraged.

---

## Recommendation

**Keep building — with one boundary drawn deliberately, and one thing worth
stealing outright.**

The spine of this recommendation: **`dsh` is a better framework and a worse
service, and this project is a service.** Everything that makes this harness
what it is lives in the half `dsh` does not have — durable work intake, ack
discipline, single-use idempotency, a worker pool, a results stream, and a
cache invariant enforced by a test rather than by a convention. Everything
`dsh` is better at — plugin composition, forkable sessions, image attachments,
the LSP/terminal/sandbox seams, an ecosystem — is either not needed here or is
cheaper to borrow as an idea than to adopt as a dependency.

The three findings that decide it:

1. **§3: the gap is the whole product.** `dsh`'s own packages say
   "the contract is in-process" and "records die with the harness process"
   (`packages/jobs/jobs/README.md:40`, `jobs-local/README.md:33`). Rewriting
   `internal/queue` and `internal/worker` as TypeScript plugins would be a
   ground-up rebuild of the part of this system that actually works, in
   exchange for nothing `dsh` provides.
2. **§2: the default compaction is wrong for these economics, and the numbers
   are real.** 99.47% measured hit rate is not a design preference to trade
   away lightly. `dsh` *permits* replacing the compaction backend, so this is
   survivable — but it means the very first thing you do after adopting `dsh`
   is replace one of its shipped defaults with something that behaves like what
   you already have.
3. **The governance cost is not a footnote.** Promised
   compatibility-breaking changes at rc.5 (`README.md:11`), issues and pull
   requests disabled, and an explicit "we cannot accept external pull requests
   at the moment" (`CONTRIBUTING.md:9`). Depending on `dsh` means depending on
   a 198k-line codebase that will break under you, where you cannot file a bug,
   cannot send a fix, and can only post in Discussions and hope. For a system
   already in production, that is a standing, unpriceable risk — and it is a
   risk taken *in exchange for* rewriting the working half of the product.

**The boundary, drawn explicitly.** Do not adopt `dsh` as a framework. Do
consider it, later and optionally, as a *runtime behind the existing seam*: this
harness already declares a narrow `Client` interface in `internal/session`
implemented per-provider, and `internal/session`'s runner already "sits behind
an interface" so execution can move to a subprocess without touching the loop,
the tools, or the store (`docs/DESIGN.md` §4.5). `dsh` ships exactly the
protocol for that — a runtime driven as a subprocess over stdio JSON-RPC
(`packages/sdk/client/README.md:5`). If a future task needs something `dsh` has
and this harness does not — an LSP-aware session, a live fork, real image
input — that is the shape: queue, worker pool, store, ack discipline and
cache diagnostics stay in Go; one `dsh` subprocess per session sits where the
session runner is today. Nothing above needs building now, and I would not build it
speculatively.

**Steal this regardless of the decision.** The mandated `#### KV Cache effect`
section in all 215 package READMEs is the single best practice in `dsh`, and it
is free. This repo enforces its cache invariant centrally (`docs/CACHE.md`) and
proves it in one test; `dsh` additionally forces every package author to state,
at authoring time, what their feature does to the prefix. Adding that heading
to `internal/CLAUDE.md`'s per-package entries would catch the next
prefix-churning change at review rather than in the churn diagnostic.

### What would change my mind

Not a better plugin API — the plugin API is already better. Specifically:

- **A durable, cross-process job seam in `dsh` with ack semantics.** The
  moment `packages/jobs` stops saying "the contract is in-process" and ships a
  backend with at-least-once delivery, redelivery bounds and an idempotency
  key, the §3 gap closes and the calculus inverts — that is the one thing whose
  absence forces this project to own a runtime at all. Watch
  `packages/jobs/jobs/README.md:40` for that sentence disappearing.
- **An append-only compaction backend shipped as a default**, or any evidence
  that `dsh` treats mid-session prefix invalidation as a defect rather than a
  documented cost. A `compaction-new-session` package in-tree would be the
  signal.
- **Stability**: the developer-preview banner removed, a 1.0, PRs opened, and
  a deprecation policy. Any two of those together would make the dependency
  priceable.
- **Something genuinely unavailable here that the workaround cannot reach** —
  the strongest candidate is real image input, since `ImageBlock` (§4b) makes
  the whole Gemini vision path unnecessary rather than merely tidier. If
  DeepSeek ships a vision-capable model that `dsh` supports and the raw API
  makes awkward, that alone is worth reopening the question.

### Where I could not determine something

- Whether `dsh` has any test asserting prefix-fold stability (the analogue of
  `TestAppendOnly`). I did not exhaust `packages/core/session/tests/`; that is
  the next file to read, and a positive finding there would strengthen §1's
  assessment of `dsh` materially.
- Whether a `dsh` tool's `execute` receives a context with `ctx.llm` in scope,
  which is what makes §4b's vision plugin a few dozen lines rather than a
  redesign. Next file: `packages/core/tools/src/`.
- Whether per-session workspace isolation in `dsh` is as strict as this
  harness's per-run directory. `ctx.workspaceRegistry` tracks "ordered session
  membership" (`packages/workspace/README.md:5`), which suggests many sessions
  may share one workspace — the opposite of §4.5's "two sessions never share a
  workspace". Next file: `packages/workspace/workspace/README.md`.

I did not run `dsh`. Everything above is from reading it.
