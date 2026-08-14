import { describe, expect, it } from "vitest";
import { titleLines } from "./sessionListTitle";
import type { SessionState } from "../api/types";

// The Session cell's title/description rendering (SessionListScreen):
// a session with a title renders it bold on its own line with the
// description beneath; a description with no title renders on its own;
// a title without a description shows no description line at all (the raw
// prompt is not repeated under a title that says what the run is); and only
// a session with neither falls back to the raw prompt as the description
// line — no row ever goes blank. Boldness itself is the
// .sess-title span's font-weight 600 in styles.css; this helper decides
// what that span contains.

const base: SessionState = {
  id: "sess-1",
  model: "deepseek-v4-pro",
  effort: "high",
  workspace: "/tmp/ws",
  permission_mode: "full",
  status: "ok",
  created_at: "2026-01-02T03:04:05Z",
  version: 1,
  sub_turns: 4,
  usage: { cache_hit_tokens: 0, cache_miss_tokens: 0, completion_tokens: 0, reasoning_tokens: 0, cost_usd: 0 },
};

const sess = (over: Partial<SessionState>): SessionState => ({ ...base, ...over });

describe("titleLines", () => {
  it("renders the title bold with the description beneath it", () => {
    const lines = titleLines(
      sess({
        title: "Add session title fields",
        description: "Carry a title, description, and phase position from every producer onto the session row.",
      }),
    );
    expect(lines.title).toBe("Add session title fields");
    expect(lines.desc).toBe(
      "Carry a title, description, and phase position from every producer onto the session row.",
    );
  });

  it("falls back to the raw prompt when there is no title", () => {
    const lines = titleLines(sess({ task: "make the page match the mockup" }));
    expect(lines.title).toBeNull();
    expect(lines.desc).toBe("make the page match the mockup");
  });

  it("shows the description on its own line when there is no title", () => {
    const lines = titleLines(
      sess({ description: "Carry a title, description, and phase position from every producer onto the session row." }),
    );
    expect(lines.title).toBeNull();
    expect(lines.desc).toBe(
      "Carry a title, description, and phase position from every producer onto the session row.",
    );
  });

  it("shows no description line when a title exists but the description is blank", () => {
    // The title already says what the run is; the raw prompt is not repeated
    // under it.
    const lines = titleLines(sess({ title: "Match the mockup", task: "make the page match the mockup" }));
    expect(lines.title).toBe("Match the mockup");
    expect(lines.desc).toBe("");
  });

  it("renders no title and an empty description line when neither exists", () => {
    const lines = titleLines(sess({}));
    expect(lines.title).toBeNull();
    expect(lines.desc).toBe("");
  });

  it("shows a phase chip for a run in a chain", () => {
    const lines = titleLines(sess({ title: "Phase two", phase: 2, total_phases: 5 }));
    expect(lines.phase).toBe("phase 2/5");
  });

  it("shows no phase chip for a standalone run or a pre-migration row", () => {
    expect(titleLines(sess({ title: "Standalone" })).phase).toBeNull();
    expect(titleLines(sess({ title: "Standalone", phase: 0, total_phases: 0 })).phase).toBeNull();
  });
});
