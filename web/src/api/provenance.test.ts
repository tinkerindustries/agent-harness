import { describe, expect, it } from "vitest";

import { isUserStarted, startedBy } from "./provenance";

describe("isUserStarted", () => {
  it("is true for a producer-stamped user start", () => {
    expect(isUserStarted({ parent_is_user: true })).toBe(true);
  });

  it("is false for a producer-stamped agent start", () => {
    expect(isUserStarted({ parent_is_user: false, parent_agent_type: "claude-code" })).toBe(false);
  });

  it("reads the legacy encoding exactly like a producer-stamped user start", () => {
    // A pre-migration row carries parent_agent_type === "user" and no
    // parent_is_user at all.
    expect(isUserStarted({ parent_agent_type: "user" })).toBe(true);
    expect(isUserStarted({ parent_agent_type: "user", parent_agent_id: "geoff" })).toBe(true);
  });

  it("is false for an agent start even when parent_is_user is absent", () => {
    expect(isUserStarted({ parent_agent_type: "claude-code" })).toBe(false);
  });

  it("is false for a row with no provenance", () => {
    expect(isUserStarted({})).toBe(false);
  });

  // The fork and the label must agree about every row: startedBy is built on
  // isUserStarted, so a row can never be "started by you" to the label and an
  // agent run to the fork, or vice versa. The person label is the tell.
  it("agrees with startedBy on every row shape", () => {
    const rows: Array<{
      parent_is_user?: boolean;
      parent_agent_type?: string;
      parent_agent_id?: string;
    }> = [
      { parent_is_user: true },
      { parent_is_user: true, parent_agent_id: "geoff" },
      { parent_is_user: false, parent_agent_type: "claude-code", parent_agent_id: "sess-1" },
      { parent_agent_type: "user" },
      { parent_agent_type: "user", parent_agent_id: "geoff" },
      { parent_agent_type: "claude-code" },
      {},
    ];
    for (const row of rows) {
      expect(isUserStarted(row)).toBe(startedBy(row) === "started by " + (row.parent_agent_id ?? "a person"));
    }
  });
});

describe("startedBy", () => {
  it("labels a producer-stamped user start without a name", () => {
    expect(startedBy({ parent_is_user: true })).toBe("started by a person");
  });

  it("labels a user start with the operator name", () => {
    expect(startedBy({ parent_is_user: true, parent_agent_id: "geoff" })).toBe("started by geoff");
  });

  it("reads the legacy encoding exactly like a user start", () => {
    expect(startedBy({ parent_agent_type: "user" })).toBe("started by a person");
    expect(startedBy({ parent_agent_type: "user", parent_agent_id: "geoff" })).toBe("started by geoff");
  });

  it("labels an agent start with its kind and session id", () => {
    expect(startedBy({ parent_agent_type: "claude-code", parent_agent_id: "sess-1" })).toBe(
      "started by claude-code (sess-1)",
    );
  });

  it("labels an agent start with no session id by kind alone", () => {
    expect(startedBy({ parent_agent_type: "claude-code" })).toBe("started by claude-code");
  });

  it("returns null for a row with no provenance", () => {
    expect(startedBy({})).toBeNull();
  });
});
