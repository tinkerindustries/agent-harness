# web/

The frontend: a session list, a transcript, and the settings screen, fed by
SSE from `harness serve` for the session surface. Vite, React, TypeScript,
with shadcn/ui on Tailwind v4 as the component layer — `accordion`, `badge`,
`button`, `card`, `collapsible`, `input`, `toggle`, `toggle-group`, and
`tooltip` are in (in `src/components/ui/`); `ScrollArea` and `DataTable` are
out, because the rail and the plan column are plain sticky elements and the
diff table renders inside the transcript. The theme variables are ported from
the design token set, and everything shadcn has no opinion about — the shared
top nav, the transcript block styles, the diff table, and the status and diff
tokens — is plain CSS in `src/styles.css`. No router, no data layer beyond the
SSE client, the store, and the settings fetch calls. `docs/DESIGN.md` §5 is
the reference for the reasoning behind all of it.

One shared top nav (`components/TopNav.tsx`) is mounted once by `App.tsx`
around whichever screen the route renders. It is not
redeclared inside the screens; each screen registers its page-specific
right-hand content — a search input, a LIVE/connection badge, a refresh button
— into the nav's right slot with `useNavRight`, so the slot stays owned by the
screen that holds its state. A session detail is the one route whose nav
drops the section links: it is a page you arrive at from the list and leave
by going back, so the links give way to a single back link to the session
list and the rest of the row belongs to the run's own controls. The `/perf`
harness route renders no nav.

What the screens are:

- **Session list.** In-flight sessions are collapsible plan cards — collapsed,
  the summary carries the run's title with the description beneath it (the raw
  prompt only when the run has no title at all, clamped to three lines; a
  title without a description shows no description line) and the trigger line
  shows the `in_progress` item's activeForm
  and the completed ratio; expanded, the whole plan and the actions row (Stop).
  The card's summary itself opens the session page — the caret is its own
  small toggle button, sibling of the summary, so toggling the plan never
  navigates. The card's stat
  row carries elapsed in primary weight and sub-turns dimmer — the finished
  table's columns minus Cost and Cache. Finished sessions are a dense
  table whose Session cell carries a one-line subtitle: the plan ratio and the
  model's summary. The Finished table is **server-paged at 20 rows a page**
  against `GET /api/sessions?status=finished&q=&limit=&offset=`
  (`api/finishedSessions.ts` `useFinishedSessions`): the search box now
  filters server-side, the query and the page are debounced, the refetch is
  keyed on `sessionListStore`'s `finishedRevision` (bumped only when the set
  of terminal sessions changes, so a streaming run never costs a refetch),
  and a `Pager` above and below the table pages the envelope — the Session
  column takes all the width the other six columns do not need, so none of
  them ever wraps. The in-flight cards
  and the stat strip still come from the SSE snapshot — only the Finished
  table reads the paged endpoint, and the Model column is the model name
  with the effort, job type and full provenance label on hover (the cell's
  `title`); the in-flight card's meta line reads the same way — the model
  name alone on the line, the effort, job type and provenance on the
  span's `title`, so the row leaves room for the stat figures beside it.
  Outcomes render as `DONE` / `GAVE UP` / `STOPPED` (`statusBadge.ts`), and a
  bare `ok` with no Complete status renders DONE too — the list shows one word
  for both, never an OK of its own. The queue banner sits at the top of the
  page, below the start-run form when it is open — it renders only when the
  pool halts (or the health poll reports an error), the routine pending /
  in-flight / redelivered counters are gone — and above the In flight
  section, when there is one. Below both of those, a stat strip — Running
  (of the pool's slots, from `worker.pool_size`), Spend today, Median
  duration today, Total time today — is a client-side reduction over the
  same session list, with no backend field behind it. The strip has two
  sizes: it is the page's hero, the first thing below the banner, while
  nothing is in flight, and compacts to roughly half its height — every
  figure kept — the moment the In flight section has anything in it,
  landing above the strip rather than displacing it below a full-size hero,
  on the phone layout as much as the wide one. The change is a transition
  only when it happens while somebody is watching (`hooks.ts`
  `useSettledFlip`, gated on the list's snapshot having landed, with
  `useHeldFrames` standing in for it on an empty harness): a page that loads
  with a run already going renders the compact strip and the section without
  either animating, because neither of those facts just happened. The
  finished table's columns run Status, Session,
  Elapsed, Cost, Model, Sub-turns, Cache: the two numbers an operator
  scans for sit right after Session, where they stay visible before any column
  that still needs the scroll container. The nav's right slot carries the
  start-run trigger (docs/RUN-CONTROL.md — it opens the start form as a card
  above the stat strip, and a null control token replaces it with a "run
  control not configured" note), the search input (id/workspace/request,
  server-side for the Finished table, client-side for the in-flight cards)
  and the LIVE badge.
- **Transcript.** The unit is the sub-turn, not the block: one card per
  sub-turn, reasoning, text, tool calls and results in one body and the usage
  block in the header (`src/api/groups.ts` builds the groups as a display-side
  view over the fold's `blocks`; the `Block` union is untouched). A
  Compact/Full toggle collapses every card to its header line; tool
  call headers show the target, not the arguments JSON, and a path target is
  relative to the session's workspace root (`toolArgs.ts` `trimWorkspace`).
  Every workspace path the UI prints is elided the same way, against the
  session directory rather than a literal root — the root is configurable
  (`HARNESS_WORKSPACES`) and is a real host path since path parity, so it
  carries the operator's own directory names and an elision keyed to one
  spelling of it silently stops working. `components/ui/workspacePath.ts`
  owns the split; where the value is a path on its own — the rails' workspace
  fact, the operations screen's session and lease rows — `ui/ElidedPath.tsx`
  renders it behind a control that puts the whole path back on click, so a
  shortened path is always visibly shortened and never a dead end. The
  opening block
  collapses to one summary line; a cache-churn banner links to the first
  sub-turn that churned. A `Screenshot`, `Glance`, `Ground`, `Detect` or
  `Crop` result renders the images above its text, fetched from the
  session's live workspace
  (docs/TOOLS.md, "Seeing the screenshots"); the session id reaches that leaf
  through `SessionIdContext` rather than a prop, because the path to it runs
  through the memoised cards that exist to bail out of re-rendering, and a
  file that is gone renders as "no longer available" rather than a broken
  image. Neither session page filters or searches its own
  transcript — the filter chips (All/Edits/Bash/Errors/Churn) survive only in
  `TranscriptToolbar`, for the child-transcript block and the perf harnesses,
  and `TurnTranscript`'s `filter` prop is `"all"` on both screens. The sticky
  left rail lists every sub-turn under the
  plan item that was `in_progress`, one glyph per tool call, with a single
  IntersectionObserver marking the current entry. On the watch page's own
  rail each sub-turn is a coloured square instead — error red, churn amber,
  edit green, bash blue, read violet, anything else plain, in that priority
  order (`WatchRail.tsx` `tickCls`, over `GroupTags`). The screen's own
  header is gone: the nav's crumb shows the session id and its right slot
  carries the connection badge.
- **Evals.** The eval list is every run over time — what it compared, where it
  got to, and a headline delta; the run page is the comparison table, then the
  runs it is built from (docs/EVALS.md). No statistics happen in the browser:
  the server computes every mean, standard error and delta through the same
  code the CLI's table uses, and decides significance, so `src/api/evals.ts`
  formats and arranges and nothing else. A metric with nothing to measure
  renders as a dash and never as a zero. Both screens poll while a run is
  going. Both are pushed over SSE, not polled: each frame is a whole snapshot
  rather than a delta, so the store is a straight replace and none of the
  frame-budget machinery below applies — an eval changes a few times a minute,
  not a few times a frame. The run stream is per-screen rather than an
  app-lifetime singleton, because a run stream is about one page. The nav's
  right slot carries the start trigger, which opens
  a form card above the table; a null control token replaces it with a "run
  control is not configured" note, the same shape the session list's start
  uses. The form names only suites and variants the build already has, both
  fed by the server's own lists, so no browser can introduce a system prompt
  this build does not know. It states the run count and an estimated cost —
  derived from what earlier runs actually cost per member, and absent rather
  than zero when there are none — before the confirm.
- **Settings.** One collapsible row per registry entry: closed is key, value
  and description; open is the write controls with the bounds the registry
  validates against. A closed set renders a `ToggleGroup`; only overrides and
  unset secrets are badged. The nav's right slot carries the key/description
  search input, which filters the rows client-side on top of the chip filter.
- **MCP.** One card per configured server (docs/MCP.md): its name, a status
  badge (OK with the tool count and a relative "probed N minutes ago", ERROR
  with the server's own `probe_error` verbatim, or NEVER PROBED), and the
  command it runs or its URL in monospace. Enable/disable is a `Toggle` on
  the card and the one write on this screen that is optimistic rather than
  re-fetching (`web/src/components/MCPScreen.tsx`, `web/src/api/mcp.ts`) — it
  flips immediately and rolls back with the server's message on a failed
  write, because turning a server on and off is the action this screen exists
  to make a single click. The tools a server contributes are a `Collapsible`,
  closed by default, each row the tool's `mcp__<server>__<tool>` qualified
  name in monospace beside its description. Refresh, Edit and Delete are
  per-card; Delete confirms inline, not `window.confirm`. The add form sits
  behind an "Add server" button and opens with a paste box wired to
  `web/src/api/mcpCommand.ts`'s `parseMCPCommand` — it reads a
  `claude mcp add ...` line, a bare command, or a bare URL, fills the fields
  below it, and those fields stay editable either way; editing reuses the
  same form pre-filled, with masked env/header values shown as the server
  sent them and a note that leaving one untouched keeps the stored secret
  (`mcpStatus.ts`'s `buildKVPatch` is what turns "untouched" into the
  keep-or-remove convention docs/MCP.md describes for a PATCH). The nav's
  right slot carries a refresh-all button.

Build output lands in `../internal/webassets/dist`, which the Go binary embeds.
Don't change `build.outDir`.

## Commands

| Task | Command |
| --- | --- |
| Dev server | `npm run dev`, with `harness serve -dev-frontend http://127.0.0.1:5173` |
| Build and typecheck | `npm run build` (runs `tsc -b`) |
| Test | `npm run test` |
| One file | `npm run test -- src/api/fold.test.ts` |

## Rules

The browser can write to the data the harness manages, and it can control a
running session — no approve button, and run control in full: a **start**
form on the session list (prompt, repos, a model, a thinking effort, an
explicit permission mode with the docker-socket warning stated next to the
control, and the optional fields behind a disclosure), a **steer** input and a
**stop** on the transcript
screen, both visible only while the session is running, the stop behind a
confirmation (docs/RUN-CONTROL.md "The frontend"). The steer write is an
acceptance, not a delivery: the text lands in the log and reaches the model
at the next sub-turn boundary, and the transcript's steer block shows it as
*pending* until the matching `steer_applied` arrives — a steer that sits
pending for minutes is the operator's signal that the run is wedged, which is
a feature of the display, not an accident of it. The stop and the start are
acceptances, not outcomes: the stop shows *stopping…* between the 202 and the
terminal event, and a started run appears on the session list when the pool
claims it — the screen never polls and never invents a row, because the SSE
stream the screens are already connected to is what says a session started.
The approve button stays out — docs/DESIGN.md §4.6, and a loop that waits on
a person is a loop that stalls when nobody is watching.

What that means for work here: a screen that writes is now ordinary, so the
settings screen stops being a special case and becomes the pattern to follow.
It re-fetches after every write rather than guessing at the new value, which is
the right default while a screen has one write in flight at a time. A screen
with several concurrent writes needs more than that, and it is the point to
stop and design rather than to spread the re-fetch.

Everything below is about frame budget, which is the only hard problem here. Two
text channels arrive as deltas and a long session accumulates hundreds of
blocks; the naive shape re-parses the whole transcript tens of times a second.

- **Streaming text stays out of React state.** Deltas append to a mutable buffer
  outside React and set a dirty flag; a `requestAnimationFrame` loop flushes it,
  so React sees at most one update per frame whatever the token rate. Components
  subscribe to the external store with `useSyncExternalStore`.
- **Completed content freezes.** Top-level blocks (opening, skills,
  `run_finished`, `error`) become immutable values wrapped in `React.memo`,
  keyed by block id, and never re-render again. A sub-turn card freezes at
  group granularity: its children array is reference-stable from the moment
  its last tool result lands, and the memoised card bails out on it
  (`src/api/groups.ts`, docs/DESIGN.md §5.9). This carries most of the win —
  in a long session every card but the tail one is inert.
- **A streaming block renders as plain preformatted text.** No markdown parse, no
  highlighting, no diff computation until the block completes; then it parses and
  highlights once and swaps in.
- **Never compute a diff in a render pass.** Go sends structured line arrays and
  the browser renders a table.
- **Large tool outputs collapse** to a head and tail preview with an expand
  control. A huge file read is a disclosure problem, not a virtualisation one.
- **`src/api/fold.ts` must stay in shape agreement with `internal/fold`.** Both
  walk the same event log — one produces the API `messages` array, the other
  display blocks. A new event kind needs both. The sub-turn grouping and the
  rail (`src/api/groups.ts`, `components/TimelineRail.tsx`) are display-side
  views over the fold's output and never add a `Block` variant.
- **A `live` SSE frame is not an event.** `ingestLive` takes model output the
  backend has not committed yet and appends it to `LiveTurn.liveReasoning` /
  `liveContent` — never to `reasoning` / `content`, which belong to the
  committed `reasoning_delta` and `content_delta` events. The frozen block is
  always built from the committed pair; the live pair is a preview that gets
  discarded. Note when the committed pair actually lands: the whole batch is
  written with the `turn_finished` that freezes the block, not progressively,
  so the live frames are the only text that arrives a piece at a time. That is
  why `liveContentChunks` — the same text kept as the frames it came in —
  exists, and why it is what the streaming reveal renders from
  (`components/ui/StreamText.tsx`). It is append-only, and an entry rewritten
  in place replays its animation on every flush.
- **`replayed` is the seam, and it comes from the server.** One named,
  id-less frame after the history and before the first live event
  (`internal/httpapi` `handleSessionStream`). It reaches the snapshot as
  `replayed`, and it is what tells the display which rows are backlog and
  which arrived while somebody was watching (`hooks.ts` `useArrivals`). Do not
  try to infer it: a long replay arrives across several reads, so "the first
  blocks I saw" is a fraction of the history. A store driven without a
  connection — the perf harnesses — calls `markReplayed()` once it has seeded
  its own history.
- **Two feeds, two shapes, and the narrow one is the list's.** `GET
  /api/stream` sends `SessionListRow` (`internal/hub`'s `ListRow`): only the
  fields the session list draws, with `task` capped. It re-sends a whole row on
  every sub-turn of every running session to every open tab, so a field added
  there is paid for at that rate — add one only by adding it to `ListRow` on
  the Go side too, and only if this screen renders it. Everything else comes
  from a full row: the Finished table fetches one per page (and *merges* the
  live row over it — `api/liveOverlay.ts` `mergeLive`, never a replacement, or
  the summary and cache columns blank), and the session detail screens read
  `state` frames off the session's own stream (`transcriptStore`'s
  `snapshot.state`, preferred over `useSessionMeta`'s fetch, which is what
  answers "does this session exist" and what covers a finished run).

Virtualisation is out, and the measurements that decided it are in §5.5: delta
commits are flat in block count, appending a block is linear and no amount of
memoisation removes it. The append cost is attacked at group granularity
instead — the walk runs over sub-turn cards and every earlier card bails out,
with the numbers in §5.9 — but the conclusion stands. Re-measure with the
harness in `src/perf` rather than arguing from first principles.

Tests cover the fold, the grouping, and the display helpers. There is no DOM
harness and the components are not unit-tested — see [../TESTING.md](../TESTING.md).
