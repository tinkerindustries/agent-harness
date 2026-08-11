import type { SessionState } from "./types";

// startedBy renders a session row's provenance as the one-line label both
// screens show ("started by geoff", "started by claude-code (sess-1)"). It is
// the single place the legacy encoding lives: a pre-migration row carries
// parent_agent_type === "user" and no parent_is_user, and must read exactly
// like a producer-stamped user start.
export function startedBy(
  sess: Pick<SessionState, "parent_is_user" | "parent_agent_type" | "parent_agent_id">,
): string | null {
  if (sess.parent_is_user === true || sess.parent_agent_type === "user") {
    return "started by " + (sess.parent_agent_id || "a person");
  }
  if (sess.parent_agent_type) {
    return "started by " + sess.parent_agent_type + (sess.parent_agent_id ? " (" + sess.parent_agent_id + ")" : "");
  }
  return null;
}
