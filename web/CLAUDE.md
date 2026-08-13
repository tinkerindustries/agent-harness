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
  the summary carries the job's description (the session's `task`, clamped to
  three lines) and the trigger line shows the `in_progress` item's activeForm
  and the completed ratio; expanded, the whole plan and the actions row (Stop).
  The card's summary itself opens the session page — the caret is its own
  small toggle button, sibling of the summary, so toggling the plan never
  navigates. The card's stat
  row carries elapsed in primary weight and sub-turns dimmer — the finished
  table's columns minus Cost and Cache. Finished sessions are a dense
  table whose Session cell carries a one-line subtitle: the plan ratio and the
  model's summary.
  Outcomes render as `DONE` / `GAVE UP` / `STOPPED` (`statusBadge.ts`), not one
  green OK. A stat strip above the queue health bar — Running (of the pool's
  slots, from `worker.pool_size`), Spend today, Median duration today, Total
  time today — is a client-side reduction over the same session list, with
  no backend field behind it. The finished table's columns run Status, Session,
  Elapsed, Cost, Model, Sub-turns, Cache: the two numbers an operator
  scans for sit right after Session, where they stay visible before any column
  that still needs the scroll container. The nav's right slot carries the
  start-run trigger (docs/RUN-CONTROL.md — it opens the start form as a card
  above the stat strip, and a null control token replaces it with a "run
  control not configured" note), the search input (id/workspace/request,
  client-side) and the LIVE badge.
- **Transcript.** The unit is the sub-turn, not the block: one card per
  sub-turn, reasoning, text, tool calls and results in one body and the usage
  block in the header (`src/api/groups.ts` builds the groups as a display-side
  view over the fold's `blocks`; the `Block` union is untouched). A
  Compact/Full toggle collapses every card to its header line; tool
  call headers show the target, not the arguments JSON, and a path target is
  relative to the session's workspace root (`toolArgs.ts` `trimWorkspace`);
  the opening block
  collapses to one summary line; a cache-churn banner links to the first
  sub-turn that churned. A `Screenshot` or `ReviewScreenshot` result renders
  the images above its text, fetched from the session's live workspace
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
  committed `reasoning_delta` and `content_delta` events that arrive moments
  later carrying the same text. One field for both would double every streamed
  sub-turn. The frozen block is always built from the committed pair; the live
  pair is a preview that gets discarded.

Virtualisation is out, and the measurements that decided it are in §5.5: delta
commits are flat in block count, appending a block is linear and no amount of
memoisation removes it. The append cost is attacked at group granularity
instead — the walk runs over sub-turn cards and every earlier card bails out,
with the numbers in §5.9 — but the conclusion stands. Re-measure with the
harness in `src/perf` rather than arguing from first principles.

Tests cover the fold, the grouping, and the display helpers. There is no DOM
harness and the components are not unit-tested — see [../TESTING.md](../TESTING.md).
