# Design pass: the eval run screen

The screen under review is `web/src/components/EvalRunScreen.tsx`, serving the
run `evr-a18b273cf2ff91b757c550e35ad6abfe` — a real, cancelled comparison:
suite `search`, variants `base` against `kimi-steps`, 3 replicates, 24
members, 18 finished ok, 1 failed, 5 never started, no judge, and a headline
delta (+2.1pp on Searches via Grep/Glob) that the server's own statistics
mark insignificant. The screen's job is to report that partial, abandoned
experiment honestly — "no answer" is the correct answer, and the design
question is whether a reader can arrive at it.

This pass was done blind: appearance from `Screenshot` + describe-mode
`ReviewScreenshot` (what is on the screen, never whether it is good), and
every number from `scripts/layout-metrics.sh` against the development stack
at `host.docker.internal:8080`, which serves this run. The two instruments
were kept apart: the vision model was asked what is there, the metrics
script for the boxes, fonts and contrasts.

## Proposals

### 1. The Delta column is the answer to the screen's question, and it is the least reachable column on the screen (observed)

What I observed and how: on a 390px phone the comparison table is 452px wide
inside a 366px scrollport (`div.eval-table-scroll 12,445 366x501 scrolls-x`,
`table.session-table.eval-comparison 12,445 452x501`), with columns Metric
183 / base 94 / kimi-steps 94 / Delta 81. The Delta cell starts at x=383 —
the whole column sits beyond x=366 and is therefore *zero pixels visible*
when the page loads. On desktop the same column starts at x=937 while the
metric names start at x=176: the answer sits ~750px from the question, and
at any viewport narrower than ~1000px it disappears entirely behind a
horizontal scroll. The runs table has the same shape: 727px in a 366px
scrollport, with the Sub-turns header sliced at the viewport edge and Cost,
Judge and Session — the column that actually links somewhere — off-screen
at rest.

What it costs a reader today: on a phone (or a narrow laptop), the reader
must discover that the table scrolls — there is no affordance — and drag
~350px sideways to see the one column the screen exists to show. The screen
never presents its own conclusion at rest; it presents the first half of the
evidence.

What I would change: move the Delta column from last to second, directly
after Metric, in `ComparisonTableRow`. At 390px the row then shows Metric +
Delta fully at rest (183 + 81 = 264 < 366), and on desktop each row reads
"metric → outcome → evidence" — the same phrase the eval list already uses
in its Headline column ("Searches via Grep/Glob +2.1pp" is one cell there),
so the list-to-detail transition keeps the same sentence. The caption below
the table gains the direction, so the moved column stays self-explanatory:
"Delta is kimi-steps minus base. ± is the standard error of the mean. A
delta smaller than the two standard errors combined is not a result; add
replicates."

What I expect to improve: the answer column is visible without interaction
on every viewport; the column order now matches the screen's own narrative
order (what was measured, what changed, the evidence). I am aware this
diverges from the CLI table's `metric | base | kimi-steps | delta` order,
which the screen otherwise mirrors; the CLI has no viewport constraint, and
on this screen the answer column's reachability outweighs the mirror.

### 2. The comparison never states its verdict; "not a result" is encoded only as an absence (observed)

What I observed and how: this run's delta column reads "+2.1pp, −5.8pp,
+2.00, +0.2pp, no change, +0.44, +1k, +1.22, +$0.0251" (layout-metrics
desktop readout, e.g. `span.eval-delta 945,346 44x16 14px/22 400 #09090b on
#ffffff`). Not one carries the significance star, because none is
significant — the screen's headline difference is, by its own statistics,
"not a result". Nothing on the screen says that. The verdict exists only as
(a) the *absence* of a `*` the reader must know to look for, and (b) a
caption sentence below the table ("A delta smaller than the two standard
errors combined is not a result; add replicates.") that the reader must map
back onto the numbers by hand. The significance flag itself is already in
the payload — the server sends `headline` on the run row and `significant`
on every delta — and this screen ignores `headline` entirely.

What it costs a reader today: to answer "did kimi-steps help?" the reader
must scan nine rows looking for a marker that is not there, read the caption,
and perform the standard-error comparison themselves. On this run — where
the honest answer is "no answer" — the screen is silent about it.

What I would change: two things. First, a verdict line under the
"Comparison" heading, rendered from `detail.headline` (already fetched):
the headline metric's name and formatted delta, with the star and semibold
when significant, and muted with "— not a result" appended when not.
Second, render insignificant deltas in the delta column in the muted
foreground, keeping body colour + star + semibold for significant ones, so
the per-row state is scannable and a significant finding stands out instead
of being a tiny star among identical rows. The caption stays as the legend.

What I expect to improve: this run's page opens with "Searches via Grep/Glob
+2.1pp — not a result" above the table, and every row of the delta column
is visibly grey — the "nothing here is a result" state is the first thing a
reader meets rather than the last thing they deduce.

### 3. The status badges misstate what happened: ok and pending are indistinguishable, and the screen contradicts its own list screen's rule (observed)

What I observed and how: on the phone capture of the runs table, the "ok"
and "pending" badges were both described as "grey border, grey text" — the
same outline treatment — while "FAILED" is red. In code the member badge is
`variant={member.error ? "failed" : "outline"}`: every non-failed member
gets the neutral outline regardless of whether it finished, is queued, or
is mid-flight. The run's own status badge is `status === "running" ?
"running" : "outline"`, so CANCELLED renders as a plain outline. Meanwhile
`EvalListScreen.tsx` defines `evalStatusVariant` with the stated purpose
that "a badge means the same thing wherever it appears" — ok → done (green),
cancelled → stopped — and this screen, the one that shows the statuses of
twenty-four individual runs, is the screen that does not use it. The member
status text is also lowercase ("ok", "pending") while the failed badge is
uppercase ("FAILED"), so the column mixes cases.

What it costs a reader today: on the very screen whose story is "18 finished,
1 failed, 5 never started", the two states that matter most — finished and
never-started — look identical. The cancelled-run narrative is invisible in
the table.

What I would change: share the mapping. Move `evalStatusVariant` out of
`EvalListScreen.tsx` into `web/src/components/ui/badge.tsx` as
`statusBadgeVariant`, use it from both screens, apply it to member badges
and the run-status badge in `EvalRunScreen.tsx` (ok → done, failed → failed,
running → running, cancelled → stopped, pending falls through to outline),
and uppercase the member status text so the column reads OK / PENDING /
FAILED consistently.

What I expect to improve: 18 green "ok" rows, one red FAILED, five grey
PENDING — the state of the abandoned experiment is legible at a glance, and
the two eval screens stop disagreeing about what a badge means.

### 4. The warning understates the incompleteness of a cancelled run (observed)

What I observed and how: the header facts read "Runs 19/24 · 1 failed" and
the warning under the comparison reads "1 of 24 runs did not finish cleanly
(failed). Their metrics stop where the run stopped, not where the work did —
read this comparison with that in mind, or run it again with a larger
budget." The run's own data says 18 ok, 1 failed, 5 pending. The server
counts `finished` as "reached a terminal state" (internal/httpapi/evals.go:
ok and failed both increment Finished), so the truthful picture is 19 ran to
completion (18 cleanly), 5 never started. The warning counts only the failed
member — `terminated` filters pending out — so it reports 1 unfinished run
where there are 6 incomplete measurements, and it recommends "a larger
budget" for a run that was cancelled, not budget-capped.

What it costs a reader today: the one paragraph on the screen that narrates
"this comparison is partial" says the opposite of what happened. A reader
who trusts it believes only one of twenty-four measurements is missing;
the table's n=9 vs n=9 (and n=7 for the decay metric) tells the truth but
only to a reader who goes looking.

What I would change: count pending members when the run is over (a pending
member during a live run is just the queue and must not trigger the warning,
so keep the current behaviour while `status === "running"`), and reword:
"5 of 24 runs never started and 1 run failed. Their metrics are missing or
stop where the run stopped, not where the work did — read this comparison
with that in mind, or run it again." The count is now consistent with the
header's 19/24.

What I expect to improve: the screen's own narration of the experiment
matches the experiment.

### 5. The Judge column is a dead column on unjudged runs (observed)

What I observed and how: this run has no judge (`judge_model` absent from
the API payload, no member carries a verdict), yet the runs table renders
eight columns including Judge, every cell of it an em dash
(`td … "—"`, `.eval-absent`). The header already conditionally omits the
Judge fact (`{detail.judge_model && <Fact label="Judge">…}`); the table
column has no such condition.

What it costs a reader today: an empty column's worth of horizontal width
on a table that is already wider than every phone viewport, pushing the
Session column — the only interactive one — further off-screen.

What I would change: render the Judge header cell and the Judge body cells
only when `detail.judge_model` is set, and make the error row's colSpan
follow (8 with a judge, 7 without).

What I expect to improve: unjudged runs lose a dead column and gain a little
room for the columns that carry information.

### 6. The header total cost carries terminal precision (observed)

What I observed and how: the header fact shows "$8.6008"
(`dd … 14px/21 … "$8.6008"`), four decimals, while the eval list renders
the same quantity — the same `cost_usd` field on the same run — as "$8.60"
(`EvalListScreen.tsx`: `toFixed(run.cost_usd > 0 && run.cost_usd < 0.01 ? 4
: 2)`). The run screen's header uses `toFixed(4)` unconditionally.

What it costs a reader today: the same number is printed at two different
precisions on two screens a reader moves between; four decimals on a total
implies a precision the total does not have.

What I would change: apply the list's rule to the header total. Member-level
costs in the runs table keep their four decimals — at $0.63 per run, two
decimals would round away the differences the table exists to show.

What I expect to improve: the two screens agree on how much a run cost.

### 7. Withdrawn: the "(n= 9 )" spacing was a readout artifact, not a page defect

What I first thought I observed: layout-metrics printed every spread line as
"±2.0% (n= 9 )" — `span.eval-spread` with text "±2.0% (n= 9 )" — and I
planned to fix a JSX whitespace wart. On implementing, I checked the
rendered text directly (a DOM textContent probe through Playwright): the
page renders "±2.0% (n=9)", correctly, in both the old bundle and the new.
The spaces exist only in the metrics readout, whose own-text collector joins
an element's adjacent text nodes with spaces — `(n=`, `9`, `)` become
"(n= 9 )" regardless of the rendered string. The same artifact put a space
before the period in the caption readout ("minus base . ±").

Why I am recording it rather than deleting it: the two instruments of this
pass — the vision model and the metrics script — both translate, and both
can translate wrongly; this is the metrics script's known failure mode, and
"measured" claims need a direct check when they concern literal text. The
proposal is withdrawn; no page change resulted from it (the source line is
unchanged in the diff).

### 8. The header's failed count is not coloured (observed)

What I observed and how: the Runs fact renders "19/24 · 1 failed" all in
body colour, while the list screen renders the same phrase with the failed
count in `.eval-failed` (destructive red) — the class exists and is used
there.

What it costs a reader today: the one red datum the run has (a failed
member) is invisible in the header, where the eye lands first.

What I would change: wrap the failed clause in the existing `.eval-failed`
span, as the list does.

### 9. A live run has no progress indication on the screen that watches it (inferred)

What I observed and how: I could not observe a live run — the development
stack is read-only and the copied database contains only the finished,
cancelled run. The inference is from the screen's own nature: it is the
SSE-live detail view (the nav badge reads LIVE, the table fills in as
members finish), and the eval list — the screen one click away — renders a
progress bar under the runs counter while a run is going (`.eval-progress`,
"Two pixels tall and only as wide as the text read as an underline rather
than a progress indicator"). The detail screen has no such indication: the
Runs fact is static text.

What it costs a reader today: watching an eval from its detail screen — the
natural place to watch it — gives no sense of whether it is moving.

What I would change: render the existing `.eval-progress` bar under the Runs
fact while `status === "running"`, exactly as the list does.

What I expect to improve: the live detail screen and the list agree about
what "in flight" looks like.

### 10. The header facts carry no time information (inferred)

What I observed and how: the facts strip (Comparing, Replicates, Runs, Cost,
Status — no time) is visible in every capture; nothing anywhere on the
screen shows when the run happened or how long it took, while the list
screen shows Elapsed for the same run and the screen itself shows the cost —
the other half of "what did this run consume".

What it costs a reader today: a reader who lands on the detail screen
directly (refresh, bookmark, a link shared in chat) cannot tell whether this
run is from today or last month without going back to the list.

What I would change: add a Duration fact built from `started_at` /
`finished_at` through the list's own `formatDuration` helper.

What I expect to improve: the header answers "how much did this take" next
to "how much did this cost".

### 11. Narrow screens give no sign that the tables continue (observed)

What I observed and how: at 390px the runs table's viewport cuts the
Sub-turns header mid-letter at x=366 (`th 297,1163 85x34` against a 366px
scrollport) and hides Cost, Judge and Session entirely; the comparison table
did the same to Delta before proposal 1. There is no edge shadow, gradient
or other cue that the tables scroll — a hard cut looks like a rendering bug
rather than an invitation to drag.

What it costs a reader today: on a phone the reader has no reason to believe
there is more table, so the Session links (the runs table's only
interaction) might as well not exist.

What I would change: a CSS scroll-edge fade on `.eval-table-scroll` — the
classic dual-background trick (a `local`-attached background-colour mask
covering a `scroll`-attached shadow at each edge) so the fade exists only
while the table overflows and disappears on wide screens, using
`var(--background)` and a `color-mix` of the foreground so it works in both
colour schemes.

What I expect to improve: "there is more this way" is visible exactly when
it is true.

## What I examined and chose to leave alone

- **The LIVE badge in the nav.** It says LIVE on a cancelled run because it
  is the SSE *connection* indicator, shown the same way on the session list
  and transcript screens ("The mark pulses while the stream is open"). It is
  app-wide vocabulary, not this screen's, and changing its meaning here
  would make the connection state unreadable. The header's CANCELLED badge
  is the run's state; the two live side by side, and I judged that
  acceptable.
- **Vertical rhythm.** layout-metrics shows a consistent scale: 32px between
  header and sections and between sections, 24px under section headings,
  2px inside facts — nothing to fix.
- **Contrast.** Every measured text passes: muted on white 4.8:1, the
  warning red 5.8:1, muted on dark 6.2:1. No change needed.
- **The runs table staying a table on phones.** The phone pass deliberately
  keeps eval-run tables scrolling with their headers ("horizontal scroll is
  the treatment for dense numeric tables") while converting the eval *list*
  to cards. I considered cards for the runs table and judged the scroll
  treatment right for 24 rows of short values — the fix is the scroll
  affordance (proposal 11), not a card conversion.
- **The "Comparing" fact's prose form** ("base against kimi-steps") versus
  the list's variant badges. The prose is clearer than badges here and
  wraps acceptably at 390px.
- **The metric column's min-width floor** (11em on phones) and the
  left-aligned numeric cells. The comparison is read row-wise, across the
  two arms, where left alignment is as good as right; the metric floor keeps
  names readable.
- **Member-level costs at four decimals**, the mono typeface, and the
  section headings "Comparison" / "Runs".
- **The header's "19/24" arithmetic.** The server counts `finished` as
  "reached a terminal state, including failed" — 19 = 18 ok + 1 failed —
  and the header renders the server's numbers faithfully. The warning
  (proposal 4) is where the narration had to be fixed, not the counter.

## Confirmed after implementing

Verified on the worktree stack serving the same run from a copy of the
development stack's database, before/after like-for-like (layout-metrics on
the same route, describe captures of the same viewport):

- **1 (delta second).** Confirmed. Desktop metrics show the header order
  Metric | Delta | base | kimi-steps (`th "Delta" 552,338 167x34`) and the
  phone metrics show the Delta cell at x=195–276 — fully inside the 366px
  scrollport, visible at rest where it was zero pixels visible before. At
  768px the table fits its container exactly (736px) with all four columns
  on screen. The caption renders "Delta is kimi-steps minus base."
- **2 (verdict).** Confirmed in its insignificant branch, which is the
  branch this run's data exercises: the verdict line renders muted
  ("Searches via Grep/Glob +2.1pp — not a result", 13px, 4.8:1 light /
  6.2:1 dark) and every delta cell renders muted (14px, 4.8:1). The
  significant branch — body colour, star, semibold — is CSS only and was
  not observable in this data; the star markup is the same pattern the
  screen already used.
- **3 (badges).** Confirmed. The phone capture of the runs table shows OK
  green, PENDING grey, FAILED red as three distinct badges; the header
  badge renders CANCELLED with the stopped tint (grey background) on
  desktop and RUNNING blue in a synthetic running state.
- **4 (warning).** Confirmed in both states: the cancelled run renders
  "5 of 24 runs never started and 1 run failed. Their metrics are missing
  or stop where the run stopped…"; with the run's status temporarily set to
  running in my own stack's database, the same screen renders "1 run
  failed. …" — pending members correctly stay out of the warning while the
  run is live.
- **5 (Judge column).** Confirmed: this unjudged run's runs table is seven
  columns (727px → 680px at 390px) and the header shows no Judge fact.
- **6 (cost).** Confirmed: header renders "$8.60"; member costs remain
  four-decimal.
- **7 (withdrawn).** See the section above: the rendered page always showed
  "(n=9)".
- **8 (failed count).** Confirmed: "19/24 · 1 failed" with the failed
  clause in destructive red, matching the list screen.
- **9 (progress bar).** Confirmed, in a synthetic running state (status
  edited in my own stack's database copy): a 6ch progress bar renders under
  the Runs fact, ~80% filled for 19/24. Not confirmed on a genuinely live
  run — the development stack is read-only and no real run was started.
- **10 (duration).** Confirmed: "Duration 34m" renders from the run's
  timestamps and disappears when `finished_at` is absent (running state).
- **11 (scroll fade).** Confirmed: the phone capture of the runs table
  shows a subtle shadow at the right edge of the scroller ("a subtle shadow
  gradient on the far right edge suggests horizontal scrolling is
  available"), and the desktop captures show neither table with any edge
  shadow — the fade exists only while the content overflows.
