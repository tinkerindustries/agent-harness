import { describe, expect, it } from "vitest";
import { outcome, statusVariant } from "./statusBadge";

// The session outcome vocabulary, pinned to design/components.html's mapping
// table (docs/WEB-REDESIGN.md phase 2): one badge per meaning, with status ok
// split on complete_status and, where the transcript's run_finished reason is
// available, on no_tool_calls.
describe("outcome", () => {
  it("maps every store status from the design table", () => {
    expect(outcome({ status: "running" })).toEqual({ label: "RUNNING", variant: "running" });
    expect(outcome({ status: "max_turns" })).toEqual({ label: "MAX TURNS", variant: "gaveup" });
    expect(outcome({ status: "failed" })).toEqual({ label: "FAILED", variant: "failed" });
    expect(outcome({ status: "timeout" })).toEqual({ label: "TIMEOUT", variant: "stopped" });
    expect(outcome({ status: "cancelled" })).toEqual({ label: "CANCELLED", variant: "stopped" });
    expect(outcome({ status: "compacted" })).toEqual({ label: "COMPACTED", variant: "outline" });
  });

  it("splits status ok on the Complete status argument", () => {
    expect(outcome({ status: "ok", complete_status: "done" })).toEqual({ label: "DONE", variant: "done" });
    expect(outcome({ status: "ok", complete_status: "gave_up" })).toEqual({ label: "GAVE UP", variant: "gaveup" });
  });

  it("renders STOPPED from the run_finished reason only the transcript carries", () => {
    expect(outcome({ status: "ok", complete_status: "", reason: "no_tool_calls" })).toEqual({
      label: "STOPPED",
      variant: "stopped",
    });
  });

  it("renders an empty complete_status as the plain terminal status, not a guess", () => {
    // A pre-migration row, or a session that ended without calling Complete:
    // either way the browser shows the plain status rather than guessing
    // which outcome it was.
    expect(outcome({ status: "ok" })).toEqual({ label: "OK", variant: "done" });
    expect(outcome({ status: "ok", complete_status: "" })).toEqual({ label: "OK", variant: "done" });
    // The reason only refines the empty case; a complete reason with no
    // status argument is still the plain status, because complete_status is
    // the only signal the session list has.
    expect(outcome({ status: "ok", reason: "complete" })).toEqual({ label: "OK", variant: "done" });
  });

  it("falls back to the raw status for a status the table does not name", () => {
    expect(outcome({ status: "denied" })).toEqual({ label: "denied", variant: "outline" });
  });

  it("keeps statusVariant consistent with outcome's colours", () => {
    for (const status of ["running", "ok", "failed", "timeout", "max_turns", "cancelled", "compacted"]) {
      expect(statusVariant(status)).toBe(outcome({ status }).variant);
    }
  });
});
