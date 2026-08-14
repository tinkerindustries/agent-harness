# Design review: the production session list

The production main page (`/` on the released stack, 114 finished sessions and
one in flight at review time) was captured four ways — light/dark ×
desktop/phone — and reviewed with Gemini vision against `web/src/styles.css`
as the spec, with a deterministic geometry check for the phone layout. The
page is in good shape: the in-flight card, the stat strip, the section heads,
the pagers and the stacked-card phone layout all verified aligned and
unclipped across two independent review passes. Two findings were real enough
to fix; the rest of what the reviewer surfaced is recorded under "Looked at
and not changed" with the reason.

## 1. The nav search placeholder truncates mid-word with no ellipsis

**Where.** The search input in the top nav (`Filter by id, workspace,
request…`), at desktop width — the `.topnav .nav-search` field is 220px and
the placeholder needs more. The phone layout is unaffected: the field takes
the full line there and the placeholder fits.

**What is wrong.** At 220px the placeholder renders as `Filter by id,
workspace, reque` — cut in the middle of "request" with no ellipsis, so it
reads as clipped text, not as a field that says what it filters. An input
clips its text by default; every other clipped string in the product (the
nav crumb, the workspace cell, the settings values) ellipsizes.

**Why it matters.** The placeholder is the only statement of what the field
does on the page's primary screen. A word severed mid-glyph reads as a
rendering bug, and a user who cannot read "request…" cannot tell the filter
covers request ids.

**Change.** Give the field the product's own truncation vocabulary:
`overflow: hidden; text-overflow: ellipsis;` on `.topnav .nav-search`. The
placeholder then ends `…` like the crumb and the workspace cell do, and a
typed query that outgrows the field ellipsizes too. *Implemented in
`web/src/styles.css`; visually confirmed against the local stack, whose nav
is identical.*

## 2. The Finished table's four numeric columns are left-aligned

**Where.** The Elapsed, Cost, Sub-turns and Cache columns of the Finished
table on the desktop layout.

**What is wrong.** `m:ss` durations, dollar amounts, counts and percentages
all start at the column's ragged left edge. Down a column of e.g. `20:32`,
`3:07`, `0:54` the seconds digits wander, and dollar figures do not line up
at the decimal point. Everywhere else in the product, numbers that sit in a
column are right-aligned — the rail's turn numbers, the diff table's line
numbers, the settings values — so the table's figures are the odd ones out
even though they are the ones an operator actually scans.

**Why it matters.** The table is the page's data-dense surface; columnar
comparison is the reason it exists. Left-aligned numerics make the columns
harder to scan than the same figures right-aligned, at no cost in density.

**Change.** Right-align the Elapsed, Cost, Sub-turns and Cache cells and
their headers (`.table-scroll .session-table th/td:nth-child(3), (4), (6),
(7)`), leaving Status, Session and Model left; the phone stacked cards reset
the cells to left alignment inside the ≤560px query, where the caption sits
above the figure and reads left. Scoped to `.table-scroll` so the eval
tables, which reuse `.session-table` with different columns, are untouched.
*Implemented in `web/src/styles.css`. Not visually confirmable on the local
stack — see below.*

## Looked at and not changed

- **Phone touch targets below 44px** (nav links at 28px tall, the in-flight
  caret's ~32px-wide tap area, the Start run button). The reviewer flagged
  this; it is a documented, deliberate trade. The design tokens state it
  outright — "Control heights. Density is the point: nothing in the harness
  is 44px" (`styles.css`, the `--control-h` block) — and every target is at
  or above the 24px floor of WCAG 2.5.8 AA (Target Size Minimum), which is
  the standard the layout actually meets. Raising links to 44px would also
  ripple through `--navlink-h`/`--nav-height` into the sticky session-page
  nav. Left alone.
- **The LIVE badge vs the Start run button on the phone nav.** One pass
  reported the badge sitting low against the button; the row is
  `align-items: center` and later passes did not reproduce it. Not changed.
- **The in-flight caret's vertical position.** Reported "floating high" in
  one phone pass and "vertically centered" in the desktop pass of identical
  geometry (`align-items: flex-start` + 13px top padding levels it with the
  badge row). Measurement noise, not a defect.
- **The in-flight card's left edge.** The reviewer's first pass reported the
  badge row and the text block at different x positions; a measured
  close-up shows badge, title, description and activeForm line all starting
  at the same x, with the caret intentionally to their left. Not changed.
- **The phone stacked cards "missing" their figures.** The first review
  reported Elapsed/Cost/Model/Sub-turns/Cache absent at phone width; a
  full-page capture shows all five labelled figures rendering under each
  card's description. The first report was the 800px viewport fold cutting
  the first card in half, not a defect.
- **Header/body column alignment, pager alignment, stat-strip baselines,
  section-head baselines, gutters.** All verified aligned in independent
  passes (light and dark, desktop and phone). Not changed.
- **Dark-scheme contrast.** Contrast ratios for every text surface were
  computed from the tokens (muted foreground on card ≈ 5.5–5.8:1, status
  badge pairs ≥ 5.7:1) and two review passes found no unreadable text. Not
  changed. The one known dark-mode contrast blemish — the `—` empty-subtitle
  placeholder, previously noted as nearly invisible on dark — never rendered
  on this page (every row carries a subtitle) and cannot be judged here.
- **The table's density.** DESIGN.md §5.8 specifies a dense table on
  purpose; row heights vary with the title/description/subtitle lines and
  that is content, not layout drift. Not changed.

## What the local stack could and could not confirm

The fixes were confirmed against the local stack (same branch, same
frontend, empty session history):

- **Confirmed visually:** finding 1. The empty-state page carries the same
  top nav and the same 220px search field, so the placeholder's ellipsis
  rendered and was compared against the production capture before/after.
- **Not visually confirmable:** finding 2. The local stack has no sessions,
  so the Finished table renders only the empty row and no numeric cells
  exist to compare. The change is verified by the CSS diff and by
  production's page rendered with the local bundle (the same markup, the
  new stylesheet, the production API), not by the local stack's own page.
