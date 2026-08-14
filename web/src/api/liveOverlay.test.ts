import { describe, expect, it } from "vitest";
import { mergeLive } from "./liveOverlay";
import type { SessionListRow, SessionState } from "./types";

// The Finished table renders full rows fetched from GET /api/sessions and
// overlays the SSE feed's live copy on top. The two are different shapes now
// — the feed carries the list projection — so the overlay is a merge, and
// these are the fields that would go missing if it ever went back to being a
// replacement.

const fetched: SessionState = {
  id: "sess-1",
  model: "deepseek-v4-pro",
  effort: "high",
  workspace: "/w/sess-1",
  permission_mode: "full",
  status: "running",
  task: "the whole launching prompt, thousands of characters of it",
  summary: "wired it up",
  version: 4,
  created_at: "2026-08-14T00:00:00Z",
  sub_turns: 3,
  usage: {
    cache_hit_tokens: 900,
    cache_miss_tokens: 100,
    completion_tokens: 50,
    reasoning_tokens: 20,
    cost_usd: 0.5,
  },
  price_table_date: "2026-01-01",
};

const live: SessionListRow = {
  id: "sess-1",
  model: "deepseek-v4-pro",
  effort: "high",
  workspace: "/w/sess-1",
  status: "ok",
  complete_status: "done",
  task: "the whole launching prompt, thousands of ch…",
  created_at: "2026-08-14T00:00:00Z",
  finished_at: "2026-08-14T00:10:00Z",
  sub_turns: 9,
  usage: { cost_usd: 1.75 },
};

describe("mergeLive", () => {
  it("takes what moved from the live row", () => {
    const got = mergeLive(fetched, live);
    expect(got.status).toBe("ok");
    expect(got.complete_status).toBe("done");
    expect(got.sub_turns).toBe(9);
    expect(got.finished_at).toBe("2026-08-14T00:10:00Z");
    expect(got.usage.cost_usd).toBe(1.75);
  });

  it("keeps the columns the live row does not carry", () => {
    const got = mergeLive(fetched, live);
    // The Session cell's subtitle, the Cache column, and the Cost tooltip:
    // all three are off the list feed, and all three were blanked by the
    // replacement this merge replaced.
    expect(got.summary).toBe("wired it up");
    expect(got.usage.cache_hit_tokens).toBe(900);
    expect(got.usage.cache_miss_tokens).toBe(100);
    expect(got.price_table_date).toBe("2026-01-01");
    expect(got.permission_mode).toBe("full");
    expect(got.version).toBe(4);
  });

  it("keeps the fetched task, which the live row carries only capped", () => {
    expect(mergeLive(fetched, live).task).toBe(fetched.task);
  });
});
