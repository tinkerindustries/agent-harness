import type { SessionListRow, SessionState } from "./types";

// mergeLive layers the session list feed's live row over a full row fetched
// from GET /api/sessions — the overlay the Finished table renders through
// (finishedSessions.ts). It lives in its own module for the same reason the
// display helpers do: the hook that uses it opens an EventSource at import
// time, and a pure function should be testable without a network.
//
// It is a merge and not a replacement because the two rows are not the same
// shape: the stream sends the list projection (SessionListRow), and this
// table renders three things that projection does not carry — the model's
// summary in the Session cell, the cache hit rate in its own column, and the
// price table's date behind the Cost tooltip. Replacing the row wholesale,
// which is what the overlay did while both feeds carried everything, would
// blank all three the moment a session appeared on both.
//
// Two fields are taken from the fetched row on purpose:
//
//   - usage keeps its token counts and takes only the live cost, because that
//     is the one figure of it the stream carries.
//   - task is immutable and the stream's copy is capped (internal/hub's
//     MaxListTaskChars), so the fetched row's is both no staler and more
//     complete.
export function mergeLive(fetched: SessionState, live: SessionListRow): SessionState {
  const { task: _task, usage, ...rest } = live;
  return { ...fetched, ...rest, usage: { ...fetched.usage, cost_usd: usage.cost_usd } };
}
