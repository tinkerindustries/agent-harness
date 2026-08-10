# web/

The frontend: a session list, a transcript, and the settings screen, fed by
SSE from `harness serve` for the session surface. Vite, React, TypeScript,
plain CSS with custom properties — no component framework, no router, no data
layer beyond the SSE client, the store, and the settings fetch calls.
`docs/DESIGN.md` §5 is the reference for the reasoning behind all of it.

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

The browser observes and, where runs are concerned, does not act. No prompt
box, no approve button, no cancel control — the server serves `GET` and
`HEAD` everywhere and the settings writes (`PUT`/`DELETE /api/settings/{key}`)
are the only writes it allows; the settings screen is the browser's one write
and it cannot reach a run (docs/DESIGN.md §4.2).

Everything below is about frame budget, which is the only hard problem here. Two
text channels arrive as deltas and a long session accumulates hundreds of
blocks; the naive shape re-parses the whole transcript tens of times a second.

- **Streaming text stays out of React state.** Deltas append to a mutable buffer
  outside React and set a dirty flag; a `requestAnimationFrame` loop flushes it,
  so React sees at most one update per frame whatever the token rate. Components
  subscribe to the external store with `useSyncExternalStore`.
- **Completed blocks freeze.** They become immutable values wrapped in
  `React.memo`, keyed by block id, and never re-render again. This carries most
  of the win — in a long session almost every block is inert.
- **A streaming block renders as plain preformatted text.** No markdown parse, no
  highlighting, no diff computation until the block completes; then it parses and
  highlights once and swaps in.
- **Never compute a diff in a render pass.** Go sends structured line arrays and
  the browser renders a table.
- **Large tool outputs collapse** to a head and tail preview with an expand
  control. A huge file read is a disclosure problem, not a virtualisation one.
- **`src/api/fold.ts` must stay in shape agreement with `internal/fold`.** Both
  walk the same event log — one produces the API `messages` array, the other
  display blocks. A new event kind needs both.

Virtualisation is out, and the measurements that decided it are in §5.5: delta
commits are flat in block count, appending a block is linear and no amount of
memoisation removes it. Re-measure with the harness in `src/perf` rather than
arguing from first principles.

Tests cover the fold and the display helpers. There is no DOM harness and the
components are not unit-tested — see [../TESTING.md](../TESTING.md).
