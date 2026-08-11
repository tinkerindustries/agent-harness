# design/

Flat HTML mockups for the web UI design pass. No build step, no JavaScript,
nothing imported from `web/`. Open `index.html` in a browser.

| File | Shows |
| --- | --- |
| `index.html` | Index, and the measurements the mocks are drawn against |
| `sessions.html` | Phase 3 main page: in-flight sessions as collapsible plan cards, finished sessions as a dense table |
| `sessions-v2.html` | **Phase 9** main page: adds the shared top nav and a stat strip, and fixes the finished table's overflow bug — see the note at the bottom of the file |
| `nav.html` | **Phase 9** top nav in its four states (Sessions/Operations/Settings active, session-detail crumb) |
| `transcript.html` | Session page: sticky timeline rail, one card per sub-turn, density toggle |
| `session-chat.html` | **Session page, interactive** — a run a person started: app shell, bottom-aligned composer, plan rail right |
| `session-watch.html` | **Session page, watching** — a run an agent started: no composer, provenance strip, navigator rail left, live footer |
| `session-states.html` | The states those two cannot show: empty composer, steer pending/delivered/rejected, stop confirm, finished, result payload, dropped stream, no plan |
| `session.css` | The layer the two session pages share: app shell, the turn, the tool row, the rails, the footer |
| `settings.html` | Settings page: the registry as disclosures, closed to key and value, open to bounds and the write |
| `components.html` | Outcome-to-badge mapping, colour tokens, shadcn component mapping |
| `tokens.css` | Shared token layer and primitives, including the phase 9 `.topnav` additions |

Every collapsible is a `<details>` and every token name in `tokens.css` is
shadcn/ui's, so the mocks map onto components rather than needing a second
design decision at build time. `components.html` has the mapping table.

## The two session pages

`session-chat.html` and `session-watch.html` replace `transcript.html`, which
drew one screen for both kinds of run. Which one a session gets is decided by
a single field on the row — `SessionState.parent_is_user`, producer-stamped in
`internal/hub`. True means a person started this run and can talk to it; false
means another agent did, and the person looking at it is a spectator who may
stop it but not steer it.

They share `session.css`, so a turn reads identically on both, and they differ
in silhouette so you can tell them apart before reading a word: **interactive**
puts its rail on the right and a composer along the bottom; **watching** puts
its rail on the left and has no composer at all. The read-only page shows no
disabled input — a greyed-out control invites you to hunt for the way to
enable it.

Three rules the composer is drawn around, all from
[`../docs/RUN-CONTROL.md`](../docs/RUN-CONTROL.md):

- **A message is accepted, not delivered.** It reaches the model at the next
  sub-turn boundary and the run never pauses for it, so the page says so and
  shows every sent message as *pending* until the matching `steer_applied`
  lands. A steer pending for four minutes is the operator's signal that the
  run is wedged; it is a feature of the display.
- **Stop is an acceptance too.** Behind a confirmation, then *stopping…* until
  the terminal event.
- **There is no approve button** and there never will be — a loop that waits
  on a person stalls when nobody is watching (`../docs/DESIGN.md` §4.6).

And one layout rule, which is why long tool output truncates to a head and a
tail rather than getting its own box: **no scroll container inside the turn
list.** A nested scroller steals the wheel, hides its content from
find-in-page, and makes the scrollbar lie about how much is left.

The plan items, diffs, token counts and costs are from
`sess-f93b37beb37098b5637832e829c37d92` — 142 sub-turns, 16:31, 99.3% cache hit,
$0.0838, an 86,674-pixel page mounting 974 block elements. The settings rows are
the registry in `internal/settings` as it stood at the design pass — 27 entries
then, in registry order, with the
real defaults and bounds.

These are drawings, not a prototype. The implementation plan is
[`../docs/WEB-REDESIGN.md`](../docs/WEB-REDESIGN.md).
