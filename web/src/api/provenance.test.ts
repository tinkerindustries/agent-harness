import { describe, expect, it } from "vitest";

import { startedBy } from "./provenance";

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
