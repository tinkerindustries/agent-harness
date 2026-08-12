import type { SessionState } from "../api/types";
import { isUserStarted, startedBy } from "../api/provenance";

// startedByShort is the finished table's Model-cell label — the list-row
// variant of the provenance label. provenance.ts's startedBy stays the one
// label the watch screen's provenance strip and ChatRail render, id and all;
// this module only shortens it for the one cell that had to fit. The full
// "started by claude-code (97bd67b8-…)" wrapped the id to three lines and
// blew the Model column out to 292px, pushing the Sub-turns and Cache
// columns past the table's 928px container edge. The short label drops the
// id — "started by claude-code" — and the cell carries the full label in its
// title attribute, still reachable on hover.
export function startedByShort(
  sess: Pick<SessionState, "parent_is_user" | "parent_agent_type" | "parent_agent_id">,
): string | null {
  const label = startedBy(sess);
  if (label === null) return null;
  // For a person-started run the id IS the short name ("started by geoff"),
  // so there is nothing to drop; only an agent-started run carries the long
  // id the cell cannot afford.
  if (isUserStarted(sess)) return label;
  return "started by " + sess.parent_agent_type;
}
