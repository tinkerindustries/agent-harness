import { describe, expect, it } from "vitest";
import { startedByShort } from "./sessionListLabel";

// The short label is the finished table's Model-cell rendering of the same
// provenance the full label serves elsewhere: for an agent-started run the id
// is dropped (the cell must stay narrow enough for the table to fit its
// container — the full label's UUID wrapped the cell to three lines), and for
// a person-started run there is nothing to drop, because the id is the
// person's short name. The full label still comes from provenance.ts's
// startedBy and rides on the cell's title attribute; this helper only ever
// shortens.

const sess = (over: Partial<Parameters<typeof startedByShort>[0]>): Parameters<typeof startedByShort>[0] => ({
  parent_is_user: false,
  parent_agent_type: "claude-code",
  parent_agent_id: "97bd67b8-a353-4fd0-9a02-918fbb91dc47",
  ...over,
});

describe("startedByShort", () => {
  it("drops the agent id, keeping the type", () => {
    expect(startedByShort(sess({}))).toBe("started by claude-code");
  });

  it("keeps the full label for a person-started run (the id is the name)", () => {
    expect(startedByShort(sess({ parent_is_user: true, parent_agent_type: "user", parent_agent_id: "geoff" }))).toBe(
      "started by geoff",
    );
  });

  it("keeps the legacy user encoding readable the same way", () => {
    // A pre-migration row carries parent_agent_type === "user" and no
    // parent_is_user; it must read exactly like a producer-stamped user start
    // (provenance.ts's own rule).
    expect(startedByShort(sess({ parent_is_user: undefined, parent_agent_type: "user", parent_agent_id: "geoff" }))).toBe(
      "started by geoff",
    );
  });

  it("falls back to 'a person' for a user-started run with no id", () => {
    expect(startedByShort(sess({ parent_is_user: true, parent_agent_type: "user", parent_agent_id: undefined }))).toBe(
      "started by a person",
    );
  });

  it("returns null when provenance.ts's startedBy does", () => {
    expect(startedByShort(sess({ parent_is_user: undefined, parent_agent_type: undefined, parent_agent_id: undefined }))).toBeNull();
  });

  it("keeps an agent run without an id (the type alone)", () => {
    expect(startedByShort(sess({ parent_agent_id: undefined }))).toBe("started by claude-code");
  });
});
