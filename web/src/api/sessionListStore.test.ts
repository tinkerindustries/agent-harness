import { describe, expect, it } from "vitest";
import { finishedSignature } from "./status";
import type { SessionListRow } from "./types";

// A row needs only the fields finishedSignature reads: id and status. The
// rest of SessionListRow's required fields are filled with placeholders so
// the type is satisfied.
function row(id: string, status: string): SessionListRow {
  return {
    id,
    status,
    model: "test-model",
    effort: "high",
    workspace: `/ws/${id}`,
    created_at: "2026-01-01T00:00:00Z",
    sub_turns: 0,
    usage: { cost_usd: 0 },
  };
}

// finishedSignature is the fingerprint of the *terminal* set: the count plus
// the newest terminal session's id. The in-flight list counts the other side
// of the same line — a session is either in-flight (isLive) or finished —
// so a creating session must be absent from the terminal set exactly like a
// running one.
describe("finishedSignature", () => {
  it("counts a creating session as in-flight, not finished", () => {
    // Two live rows — one running, one still creating — and nothing terminal:
    // the terminal set is empty even though the creating row has no events
    // and no sub-turns.
    expect(finishedSignature([row("sess-1", "creating"), row("sess-2", "running")])).toBe("0:");

    // The creating row flipping to terminal is what moves the count.
    expect(finishedSignature([row("sess-1", "ok"), row("sess-2", "running")])).toBe("1:sess-1");
  });

  it("names the first terminal row in list order and ignores live rows between", () => {
    // The list arrives sorted by created_at DESC, so the first terminal row
    // in the array is the newest one — even when a live creating row sits
    // between it and the rest of the terminal set.
    const sessions = [
      row("newest", "ok"),
      row("creating-now", "creating"),
      row("older", "ok"),
    ];
    expect(finishedSignature(sessions)).toBe("2:newest");
  });
});
