# design/

Flat HTML mockups for the web UI design pass. No build step, no JavaScript,
nothing imported from `web/`. Open `index.html` in a browser.

| File | Shows |
| --- | --- |
| `index.html` | Index, and the measurements the mocks are drawn against |
| `sessions.html` | Main page: in-flight sessions as collapsible plan cards, finished sessions as a dense table |
| `transcript.html` | Session page: sticky timeline rail, one card per sub-turn, density toggle |
| `settings.html` | Settings page: the registry as disclosures, closed to key and value, open to bounds and the write |
| `components.html` | Outcome-to-badge mapping, colour tokens, shadcn component mapping |
| `tokens.css` | Shared token layer and primitives |

Every collapsible is a `<details>` and every token name in `tokens.css` is
shadcn/ui's, so the mocks map onto components rather than needing a second
design decision at build time. `components.html` has the mapping table.

The plan items, diffs, token counts and costs are from
`sess-f93b37beb37098b5637832e829c37d92` — 142 sub-turns, 16:31, 99.3% cache hit,
$0.0838, an 86,674-pixel page mounting 974 block elements. The settings rows are
the registry in `internal/settings` as it stood at the design pass — 27 entries
then, in registry order, with the
real defaults and bounds.

These are drawings, not a prototype. The implementation plan is
[`../docs/WEB-REDESIGN.md`](../docs/WEB-REDESIGN.md).
