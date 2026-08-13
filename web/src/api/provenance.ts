import type { SessionState } from "./types";

// isUserStarted is the single predicate behind both the "started by" label
// and the session-route fork: True means a person started this run and can
// talk to it; false means another agent did, and the person looking at it is
// a spectator who may stop it but not steer it (design/README.md). It is the
// only place the legacy encoding lives: a pre-migration row carries
// parent_agent_type === "user" and no parent_is_user, and must read exactly
// like a producer-stamped user start. The fork and the label both call this,
// so they can never disagree about the same row.
export function isUserStarted(
  sess: Pick<SessionState, "parent_is_user" | "parent_agent_type" | "parent_agent_id">,
): boolean {
  return sess.parent_is_user === true || sess.parent_agent_type === "user";
}

// An eval stamps parent_agent_type "eval" and puts the member's request id in
// parent_agent_id (internal/evals/suite.go), so a session says on its face
// that it is part of a comparison and cannot be steered.
export const EVAL_AGENT_TYPE = "eval";

// evalRunLink is the path a session's provenance label links to, when the
// session belongs to an eval and nothing else. The label is only a hint: the
// authoritative membership is GET /api/sessions/{id}/eval, which the session
// page fetches. This exists so the list's label is clickable without a second
// request per row.
export function evalRunLink(
  sess: Pick<SessionState, "parent_is_user" | "parent_agent_type">,
  membership: { eval_run_id: string } | null,
): string | null {
  if (isUserStarted(sess) || sess.parent_agent_type !== EVAL_AGENT_TYPE) return null;
  return membership ? `/evals/${encodeURIComponent(membership.eval_run_id)}` : null;
}

// startedBy renders a session row's provenance as the one-line label both
// screens show ("started by geoff", "started by claude-code (sess-1)").
export function startedBy(
  sess: Pick<SessionState, "parent_is_user" | "parent_agent_type" | "parent_agent_id">,
): string | null {
  if (isUserStarted(sess)) {
    return "started by " + (sess.parent_agent_id || "a person");
  }
  if (sess.parent_agent_type) {
    return "started by " + sess.parent_agent_type + (sess.parent_agent_id ? " (" + sess.parent_agent_id + ")" : "");
  }
  return null;
}
