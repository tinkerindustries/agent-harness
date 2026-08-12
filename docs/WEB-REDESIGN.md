# Web UI redesign

The mockups are in [`design/`](../design/). This is the order to build them in.

Nine phases. Each one ships on its own and leaves the UI working; nothing here
needs a big-bang cutover. Phases 2 and 3 are the two the design pass was asked
for — the DONE status and the in-flight plan on the main page — and they come
early because they carry most of the value. Phase 7, the settings screen, is
independent of the six before it and can be taken out of order; it is late only
because it is the one screen that was already working. Phase 9, the shared top
nav and the front-page rollup, is layout and client-side arithmetic only.

Nothing in this plan touches the system prompt, the tool array, or the request
path, so the cache invariant in [DESIGN.md](DESIGN.md) §3.2 is not in play. Every
new endpoint is `GET`. The browser still observes and does not act.

All nine phases have landed. Each phase below carries a **Status** line
recording the outcome — and, where the outcome diverged from the intent, the
gap. The numbers live in [DESIGN.md](DESIGN.md) §5.9–§5.11.

## What the current UI does and where it hurts

Measured against `sess-f93b37beb37098b5637832e829c37d92` on 2026-08-10 — 142
sub-turns, 16:31, 99.3% cache hit, $0.0838:

- The session page is 86,674 pixels tall and mounts 974 block elements. There is
  no navigation within it beyond the scrollbar.
- The opening TASK block renders in full, about 3,000 words before the first
  sub-turn.
- A sub-turn's reasoning, text, usage row, and each of its tool results are five
  sibling blocks at the same visual weight.
- A tool call renders as its raw arguments JSON, so an `Edit` shows an escaped
  `old_string`/`new_string` pair rather than a path.
- Every session in the list shows a green `OK`, including the ones that gave up.
- The plan is visible only inside a session, and only as struck-through text.
- An in-flight session is a table row identical to a finished one.
- The settings screen renders all 27 registry entries as always-open write rows,
  and the bounds the registry validates against never reach the browser.

## Phase 1 — shadcn foundation

**Goal.** Install the component layer and the token set, with no visible change
beyond colour.

**Changes.** Add Tailwind v4, `tailwindcss-animate`, `clsx`,
`tailwind-merge`, and the Radix primitives shadcn wraps. Run `shadcn init` with
the neutral base, then add only what the mocks use: `badge`, `button`, `card`,
`collapsible`, `input`, `toggle`, `toggle-group`, `tooltip`. Port
`design/tokens.css` into the generated theme block, keeping the variable names.
Rewrite `web/src/styles.css` as the layer that remains: the transcript block
styles, the diff table, and the status and diff tokens shadcn has no opinion
about.

**Deliberate omissions.** No `ScrollArea` — the rail and the plan column are
plain sticky elements, and a custom scroll container costs a wrapper and a
listener per column on a screen that is already frame-budget sensitive. No
`DataTable` — TanStack Table earns nothing over a `<table>` here, and the diff
table renders inside the transcript.

**Doc changes.** `web/CLAUDE.md` says "plain CSS with custom properties — no
component framework". That sentence and DESIGN.md §5.7 both need rewriting to
name shadcn and to say which primitives are in and which are out. The frame
budget rules in that file are unaffected; they are about state and memoisation,
not styling.

**Exit.** Both screens look the same or better, `npm run build` passes, and the
built bundle is measured before and after. The bundle is embedded in the Go
binary, so record the delta in the commit.

**Risk.** Tailwind's preflight resets margins the current CSS relies on. Expect
a pass of small layout corrections in the transcript blocks.

**Status.** Landed (commit `de32e2a`, PR #21). The measured bundle growth,
8.44 → 46.71 kB of CSS and 376.28 → 478.92 kB of JS (gzip 2.38 → 9.71 kB and
119.31 → 150.27 kB) across phases 1–6, is in DESIGN.md §5.11.

## Phase 2 — Outcome vocabulary

**Goal.** `OK` becomes `DONE`, `GAVE UP`, or `STOPPED`. The mapping table is in
`design/components.html`.

The three outcomes are already distinct in the store. `run_finished` carries
`reason` (`complete`, `no_tool_calls`, `max_sub_turns`) and `status` (the
argument to the `Complete` tool: `done` or `gave_up`). The transcript screen
already receives both, so it needs no backend change. The session list receives
only `store.Session.Status`, which flattens all three to `ok`.

**Backend.** Add `complete_status` to the sessions table through
`sessionMigrationColumns` (`TEXT NOT NULL DEFAULT ''`), set it in
`Runner.finishRun` from the `completeStatus` argument it already has, and carry
it on `hub.SessionState` as `complete_status`. Rows written by an older binary
keep the empty string, which the browser renders as the plain terminal status —
`OK` for `ok` — rather than guessing.

**Frontend.** One function, `outcome(session)`, returning the badge label and
variant. Both screens call it. Add the badge variants from `tokens.css`.

**Exit.** A session that called `Complete(status: "gave_up")` shows `GAVE UP` in
the list and in the transcript header. A pre-migration row shows `OK`. Add a
store test for the migration backfill and a `fold.ts` test for the mapping.

**Status.** Landed (commit `295389b`, PR #22).

## Phase 3 — In-flight sessions on the main page

**Goal.** Running sessions leave the table and become collapsible cards showing
the live plan. Collapsed, a card answers what the session is about — the job's
description — and what it is doing and how far in it is; expanded, it shows the
whole plan and the run's actions.
See `design/sessions.html`.

**Backend.** The list feed has no plan. Todos are parsed client-side from the
latest `TodoWrite` arguments, which only the transcript stream carries.

Persist rather than derive. A `plan` TEXT column on the sessions table, written
whenever `TodoWrite` executes, holding the todos array verbatim. Carry it on
`hub.SessionState`. Deriving it on demand means re-walking the event log on
every state publish, and it would lose the plan for finished sessions, which the
finished table needs for its "11 of 11 plan items" subtitle.

**Frontend.** Split the list into an in-flight group and a finished table. The
in-flight card is a `Collapsible`; several can be open at once. The collapsed
line shows the `activeForm` of the `in_progress` todo and the completed ratio.
The finished table gains a subtitle row: the plan ratio and the model's summary.

**Exit.** Two concurrent sessions each show their own plan, updating as
`TodoWrite` fires, with the disclosure state surviving a list update. A session
that never wrote a plan shows the card without a plan section rather than an
empty one.

**Status.** Landed (commit `64412b6`, PR #25). §5.8 describes the split list.
The card has since moved on: the summary carries the job's description (a
`task` column on the sessions table, written at creation) and opens the
session page, the caret is its own small toggle button so toggling the plan
never navigates, and the expanded body is the plan plus the actions row —
the "Last calls" panel is gone. §5.8 and web/CLAUDE.md describe the current
card.

## Phase 4 — The sub-turn becomes the unit

**Goal.** Reasoning, assistant text, tool calls, and their results render as one
card per sub-turn. Usage moves into the card header.

This is the largest change and the one with a real constraint attached. Two
rules bear on it:

- `src/api/fold.ts` must stay in shape agreement with `internal/fold`. So the
  grouping is a display-side view over `blocks`, not a new `Block` variant. The
  `Block` union and the event fold stay exactly as they are.
- Completed blocks freeze and never re-render. A sub-turn group cannot freeze
  until its last tool result lands, so the group is a container memoised on its
  own children array. Only the last group is open at the tail; every earlier one
  holds an unchanged array and bails out.

**Changes.** Add `groupBySubTurn(blocks)` returning an array of groups, computed
incrementally in the same style `FoldState.pushBlock` uses — append to the last
group, or start a new one on the next `assistant` block. A `usage` block is
absorbed into its group's header. `run_finished` and `error` stay top-level.

**Exit.** Re-measure with `src/perf` before and after. The claim to test is that
delta commits stay flat in group count, the same property §5.5 measured for
blocks. Do not ship this on the argument that fewer DOM nodes must be faster.

**Status.** Landed (commit `38ba276`, PR #24). The re-measurement the exit
clause demanded is in DESIGN.md §5.9: delta commits stay flat in group count
(means 0.02–0.24 ms before and after), and at 2000 blocks the append mean
dropped 6.66 → 1.43 ms and the max 8.9 → 2.3 ms.

## Phase 5 — Density, tool headers, and filters

**Goal.** 142 sub-turns fit in two screens. A tool call shows its target rather
than its arguments JSON.

**Changes.**

- A Compact/Full toggle. Compact collapses every sub-turn card to its header
  line. A card containing a failed result stays open in both modes.
- Tool call headers built from the arguments the fold already keeps in
  `toolCallsById`: `Edit` and `Write` show the path and the `+n −n` from the
  diff, `Bash` shows the command, `Read` and `Grep` show the path or pattern,
  `Task` shows the description and the child session's turn count and cost.
  Suppress a zero count rather than printing `−0`.
- The opening block collapses to one summary line: word count and the files it
  names.
- Filter chips over the folded blocks: edits, bash, errors, churn. Counts come
  from the same pass.
- A cache churn banner above the transcript when any usage block carries
  `churn_point_index`, linking to that sub-turn. The churn diagnostic is the one
  number on this screen that an operator needs to see without looking for it.

**Exit.** The session page's scroll height in Compact mode, measured on the same
session, recorded in the commit against the 86,674-pixel baseline.

**Status.** Landed (commit `16f1755`, PR #26) — with one exit clause not met
literally, and phase 8 is here to say so. The 6,973 px figure was taken on the
**synthetic** feed from `web/src/perf`, not on the real session, and the commit
read it as "about 8% of the old baseline", which compared two different
sessions. The honest record: on the synthetic 142-sub-turn feed, Compact is
6,973 px against Full's 7,880 px — about 12% shorter on the same feed. The
86,674 px baseline was measured on a real session and has not been re-measured
against the new UI; the two are not comparable (DESIGN.md §5.9).

## Phase 6 — The timeline rail

**Goal.** Browsing 142 sub-turns without the scrollbar.

**Changes.** A sticky left column listing every sub-turn, grouped under the plan
item that was `in_progress` when it ran. The boundary is free: every `TodoWrite`
call in the event stream marks one, and the fold already parses them. Each entry
shows the sub-turn number and one glyph per tool call, coloured by family, with
a failed result overriding to red. Clicking scrolls to the card; an
`IntersectionObserver` marks the current entry.

**Exit.** The rail renders 142 entries and updates its current marker while
scrolling without a measurable frame cost. Phases collapse and expand
independently. The observer is one instance over the group containers, not one
per block.

**Status.** Landed (commit `7322d6e`, PR #27). The measurements are in DESIGN.md
§5.10: one IntersectionObserver for 142 sub-turns in a production build, 8
phases / 142 entries, and scrolling stayed under frame budget. One harness bug
was left for this phase and is fixed here: the perf harness printed "0 entries"
while the rail rendered 142, because the rail's accordion mounted with every
phase closed and Radix unmounts a closed phase's rows.

## Phase 7 — The settings screen

**Goal.** The registry reads as a list rather than a wall of write controls. The
mock is `design/settings.html`.

The screen renders all 27 registry entries with a text input and two buttons on
every row, so a screen an operator visits to change one key opens with 27 fields
and 54 buttons in the tab order, and the four keys this installation has actually
overridden are indistinguishable from the twenty-three at their defaults.

**Backend.** One change: serialise `min`, `max` and `allowed` on
`settingEntry` (`internal/httpapi/server.go`). All three already exist on
`settings.Descriptor` and none of them reach the browser, so the screen cannot
show a bound the CLI shows and cannot know that `model.effort` is a closed set.
Emit `min`/`max` only for the types that have them — integer and duration — and
`allowed` only when non-empty, so a string setting's payload does not grow a
pair of meaningless zeroes. Validation stays where it is: the screen shows the
bound, Go enforces it.

**Frontend.** Each row becomes a `Collapsible`: closed is key, value and
description in one grid line; open is the description in full, the bounds, and
the write controls. Weight carries the state — a stored value at full weight, a
default muted — and only override and not-set are badged. A setting with
`allowed` renders a `ToggleGroup` instead of an input. An unset secret says what
stops working rather than "not set — default  applies", which renders with a
hole in it because a key's registry default is the empty string.

**Exit.** The screen opens with no field focused and nothing to tab through but
the disclosures; the four overridden keys are findable without reading a badge on
every row; `model.effort` cannot be set to a value the registry rejects; and a
rejected write still shows the server's own message under the field.

**Status.** Landed (commit `06c9626`, PR #23).

## Phase 8 — Measure and record

**Goal.** Leave the next person the numbers rather than the argument.

**Changes.** Run the `src/perf` harness on the synthetic feed at the sizes §5.5
used, and record the results in DESIGN.md §5. Rewrite §5.8 for the new session
list, add a section for the transcript rail and grouping, and update
`web/CLAUDE.md` to match. Delete the superseded paragraphs instead of appending
corrections to them.

**Exit.** Someone reading DESIGN.md §5 can tell what the frontend does now
without reading the diff.

**Status.** This phase. DESIGN.md §5.8–§5.11 rewritten to describe the shipped
frontend and carry the measurements; web/CLAUDE.md brought in line; the status
lines above added; and the rail harness's entries counter fixed.

## Phase 9 — Shared top nav and front-page rollup

**Goal.** One header on every product screen, and a front page that answers
"how is the harness doing right now" at a glance. The mocks are
`design/nav.html` (the four nav states) and `design/sessions-v2.html` (the
redesigned list).

**Changes.**

- `web/src/components/TopNav.tsx`, mounted once by `App.tsx` around whichever
  screen the route renders — not redeclared inside any screen. Wordmark, the
  three sections as real links with an active state (`"list"` and `"session"`
  both count as the Sessions section; only `"session"` adds the session-id
  crumb), and a right slot the screen itself fills through `useNavRight`: the
  session list's search input and LIVE badge, the settings screen's search
  input, the operations screen's refresh button, the transcript's connection
  badge. That is all the nav replaces — each screen's own "← sessions" back
  link and the list's ad hoc settings/operations buttons are gone, and
  `/perf` keeps its own header and renders no nav at all.
- A stat strip above the queue health bar: Running (of the pool's slots —
  `worker.pool_size` off the settings the settings screen already fetches),
  Spend today, Median duration today, and Done vs gave up today. "Today" is
  the current local calendar day, judged by `created_at`; all four are plain
  client-side reductions over the session list the screen already holds, so
  there is no backend field and no endpoint. The reductions recompute only
  when the list changes or the day rolls over, never on the second tick.
- The in-flight card's stat row now weights Elapsed and Cost as the primary
  numbers — bold, a step larger — and demotes Sub-turns and Cache to the
  dimmer secondary weight (`.run-stats .primary` / `.secondary`). The same
  four figures as the phase 3 design, with cache back in the card; only which
  ones are loud changed.
- The finished table's real overflow bug: `.sess-cell` had no `max-width`, so
  a long plan summary stretched the Session column until Model, Elapsed,
  Sub-turns, Cache, Cost and Request all scrolled off the right edge
  (measured live at 1600px — only Status and Session visible). The cell now
  caps at 360px and the subtitle ellipses instead of pushing, and the columns
  reorder to Status, Session, Elapsed, Cost, Model, Sub-turns, Cache, Request
  — Elapsed and Cost sit right after Session, so the two numbers this pass
  was asked to prioritise are visible before any column that still needs
  `.table-scroll`'s horizontal scroll on a narrow viewport. Elapsed and Cost
  cells carry the same primary weight as the in-flight card.

**Exit.** The four nav states in `design/nav.html` render as designed, with
each screen's right-hand content in place; the stat strip's four numbers match
a hand reduction over the same list; at 1600px the finished table shows
Elapsed and Cost without scrolling; the in-flight card keeps all four stats
visible by letting the run-meta line ellipsize first.

**Status.** Landed (commit `ef406cf`, PR #46).

## What is deliberately not here

- Virtualisation. §5.5 ruled it out on measurement. Collapse-by-default reduces
  mounted DOM by a different route, and phase 4 re-measures rather than
  re-arguing.
- Any control that starts, steers, or stops a run.
- A theme switcher. The tokens respond to `prefers-color-scheme`; a manual
  override is a separate decision.
- Server-side search over transcripts. The phase 5 search filters the blocks the
  browser already holds.
