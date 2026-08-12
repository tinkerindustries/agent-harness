// tableEmptyState is the finished table's empty-row decision, kept pure so
// the two states can never blur (SessionListScreen renders the message).
// With 83 sessions loaded and a query matching none of them, both sections
// used to drop and the page went blank below the stat strip — no table, no
// message, no hint that the filter was the reason. The states are distinct:
// "no-sessions" is a genuinely empty harness ("No sessions yet."), "no-match"
// is a filter that matched nothing ("No sessions match this query.").
export type TableEmptyState = "none" | "no-sessions" | "no-match";

export function tableEmptyState(totalSessions: number, matchedSessions: number): TableEmptyState {
  if (matchedSessions > 0) return "none";
  return totalSessions === 0 ? "no-sessions" : "no-match";
}
