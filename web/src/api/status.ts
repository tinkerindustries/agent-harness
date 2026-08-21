import type { SessionListRow } from "./types";

// The session-status vocabulary on the frontend side: one helper per
// meaning, so every place that asks "is this session live" or "can this
// session be steered" goes through the same function and the list, the
// screens and the operations logic cannot drift apart about what a status
// means. The wire type is `status: string` (types.ts), so no type change is
// needed for a new status — only these functions and the badge table
// (components/statusBadge.ts) know the vocabulary.

// isLive reports whether a session row is still going: "running", or
// "creating" while a queue-driven run's workspace is being prepared. A
// creating session counts as in-flight — it appears on the in-flight list
// and its absence of events is "quiet", not finished.
export function isLive(status: string): boolean {
  return status === "running" || status === "creating";
}

// canSteer reports whether a session's agent loop is actually running.
// Only then does it read steer messages at its next sub-turn boundary; a
// creating session has no loop yet, so the composer stays disabled while
// the workspace is being built.
export function canSteer(status: string): boolean {
  return status === "running";
}

// canResume reports whether a session can be continued from where it stopped
// (docs/RUN-CONTROL.md "Continuing"). It is canSteer's other half: a message
// typed into the composer is a steer while the loop is alive and a resume
// once it is over, and between them they cover every status but "creating",
// which is neither — the workspace is still being built.
//
// Every terminal status qualifies, not just a clean finish: a run that failed,
// timed out or was stopped is exactly the kind a person wants to talk their
// way out of, and the loop continues it the same way (internal/session's
// Resume). "compacted" is the one exclusion — that session was retired and
// its continuation is its child, which the server says in the 409.
export function canResume(status: string): boolean {
  return !isLive(status) && status !== "compacted";
}

// finishedSignature is the cheap fingerprint of the terminal-session set:
// the count plus the newest terminal session's id (newest because sessions
// arrive sorted by created_at DESC, so the first terminal row is the newest
// one). A live session's progress — running or still creating — changes
// neither; a run finishing changes both; a deletion changes the count. The
// in-flight list counts the other side of the same line — a session is
// either in-flight (isLive) or finished — so this is the "live" vocabulary
// inverted, and it lives beside isLive for the same reason.
export function finishedSignature(sessions: SessionListRow[]): string {
  let count = 0;
  let newest = "";
  for (const s of sessions) {
    if (isLive(s.status)) continue;
    count++;
    if (newest === "") newest = s.id;
  }
  return `${count}:${newest}`;
}
