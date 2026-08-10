import { describe, expect, it } from "vitest";
import { foldEvents } from "./fold";
import { groupBySubTurn, SubTurnGroupState, type SubTurnGroup, type TranscriptItem } from "./groups";
import type { StoreEvent } from "./types";

function ev(seq: number, kind: StoreEvent["kind"], payload: unknown): StoreEvent {
  return { session_id: "s", seq, kind, payload, created_at: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z` };
}

function usage(subTurn: number, extra: Record<string, unknown> = {}): StoreEvent {
  return ev(99, "usage", {
    sub_turn: subTurn,
    prompt_tokens: 10,
    prompt_cache_hit_tokens: 5,
    prompt_cache_miss_tokens: 5,
    completion_tokens: 3,
    reasoning_tokens: 0,
    cost_usd: 0.0001,
    expected_miss_tokens: 5,
    ...extra,
  });
}

function typesOf(items: TranscriptItem[]): string[] {
  return items.map((item) => (item.kind === "group" ? `group:${item.group.subTurn}` : "block"));
}

// A minimal but realistic session: opening, one sub-turn with a tool call
// and result, a usage, then run_finished — mirroring fold.test.ts's sample.
function sampleEvents(): StoreEvent[] {
  return [
    ev(1, "session_started", { opening_message: "do the task" }),
    ev(2, "turn_started", { sub_turn: 1 }),
    ev(3, "reasoning_delta", { text: "thinking" }),
    ev(4, "content_delta", { text: "ok" }),
    ev(5, "tool_call", { index: 0, id: "call_1", name: "Bash", arguments: '{"command":"echo hi"}' }),
    ev(6, "turn_finished", { finish_reason: "tool_calls", elapsed_ms: 4100 }),
    ev(7, "tool_result", { tool_call_id: "call_1", name: "Bash", content: "hi\n" }),
    usage(1, { cost_usd: 0.00035 }),
    ev(8, "run_finished", { reason: "complete", status: "ok", text: "done" }),
  ];
}

describe("groupBySubTurn", () => {
  it("groups each sub-turn's assistant, tool results, and usage into one card; opening and run_finished stay top-level", () => {
    const items = groupBySubTurn(foldEvents(sampleEvents()));
    expect(typesOf(items)).toEqual(["block", "group:1", "block"]);
    expect(items[0].kind).toBe("block");
    expect(items[0].kind === "block" && items[0].block.type).toBe("opening");
    expect(items[2].kind === "block" && items[2].block.type).toBe("run_finished");

    const group = items[1].kind === "group" ? items[1].group : null;
    expect(group).not.toBeNull();
    expect(group!.subTurn).toBe(1);
    expect(group!.blocks.map((b) => b.type)).toEqual(["assistant", "tool_result"]);
    // The usage block is absorbed into the group's header, not a sibling.
    expect(group!.usage).toMatchObject({ sub_turn: 1, cost_usd: 0.00035 });
    expect(items.some((item) => item.kind === "block" && item.block.type === "usage")).toBe(false);
  });

  it("starts a new group on every assistant block", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "turn_started", { sub_turn: 2 }),
      ev(5, "turn_finished", { finish_reason: "stop" }),
      usage(2),
    ];
    const items = groupBySubTurn(foldEvents(events));
    expect(typesOf(items)).toEqual(["block", "group:1", "group:2"]);
  });

  it("absorbs a tool_denied block into its sub-turn's group", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "tool_call", { index: 0, id: "call_d", name: "Read", arguments: '{"file_path":"a"}' }),
      ev(4, "turn_finished", { finish_reason: "tool_calls" }),
      ev(5, "tool_denied", { tool_call_id: "call_d", name: "Read", rule: "read-only", content: "denied" }),
      usage(1),
    ];
    const items = groupBySubTurn(foldEvents(events));
    expect(typesOf(items)).toEqual(["block", "group:1"]);
    const group = items[1].kind === "group" ? items[1].group : null;
    expect(group!.blocks.map((b) => b.type)).toEqual(["assistant", "tool_denied"]);
  });

  it("keeps error blocks top-level, outside any group", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "error", { message: "stream dropped" }),
    ];
    const items = groupBySubTurn(foldEvents(events));
    expect(typesOf(items)).toEqual(["block", "group:1", "block"]);
    expect(items[2].kind === "block" && items[2].block.type).toBe("error");
  });

  it("does not absorb a starved retry's first usage into the previous turn's header", () => {
    // The first attempt's usage arrives before turn_finished, so it sits
    // before the assistant block; the retry's own usage follows it. The
    // first one must stay a loose block, not leak into turn 0's header —
    // and the group's assistant must carry the API's reasoning_tokens,
    // amended by the fold on the retry's usage event.
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      usage(1, { attempt: 1, reasoning_tokens: 4000 }),
      ev(3, "reasoning_delta", { text: "hmm" }),
      ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }),
      usage(1, { attempt: 2, reasoning_tokens: 6820 }),
    ];
    const items = groupBySubTurn(foldEvents(events));
    expect(typesOf(items)).toEqual(["block", "block", "group:1"]);

    const looseUsage = items[1].kind === "block" ? items[1].block : null;
    expect(looseUsage).toMatchObject({ type: "usage", attempt: 1 });

    const group = items[2].kind === "group" ? items[2].group : null;
    expect(group!.usage).toMatchObject({ attempt: 2 });
    const assistant = group!.blocks[0];
    expect(assistant.type).toBe("assistant");
    if (assistant.type === "assistant") {
      expect(assistant.reasoningTokens).toBe(6820);
    }
  });

  it("leaves a usage for a closed turn loose rather than absorbing it into the wrong group", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "turn_started", { sub_turn: 2 }),
      ev(5, "turn_finished", { finish_reason: "stop" }),
      usage(2),
      usage(1), // late duplicate for turn 1, after turn 2 froze
    ];
    const items = groupBySubTurn(foldEvents(events));
    expect(typesOf(items)).toEqual(["block", "group:1", "group:2", "block"]);
    const group2 = items[2].kind === "group" ? items[2].group : null;
    expect(group2!.usage).toMatchObject({ sub_turn: 2 });
    expect(items[3].kind === "block" && items[3].block.type).toBe("usage");
  });
});

describe("SubTurnGroupState incremental sync", () => {
  it("returns the same items reference across live-only updates (same blocks array)", () => {
    const state = new SubTurnGroupState();
    const blocks = foldEvents([ev(1, "session_started", { opening_message: "x" })]);
    const first = state.sync(blocks);
    // A delta-only update passes the same array back; the grouped view must
    // not be recomputed or replaced — this is the token-rate hot path.
    expect(state.sync(blocks)).toBe(first);
    expect(state.sync(blocks)).toBe(first);
  });

  it("appends incrementally: a frozen group's object identity survives later appends", () => {
    const state = new SubTurnGroupState();
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "turn_started", { sub_turn: 2 }),
      ev(5, "turn_finished", { finish_reason: "stop" }),
      ev(6, "tool_call", { index: 0, id: "c2", name: "Bash", arguments: "{}" }),
      ev(7, "tool_result", { tool_call_id: "c2", name: "Bash", content: "out" }),
      usage(2),
      ev(8, "error", { message: "x" }),
    ];
    const all = foldEvents(events);
    const oneBlockAtATime = all.map((_, i) => all.slice(0, i + 1));

    // Group 1 freezes the moment turn 2's assistant opens a new group: from
    // then on every further append must leave group 1's object — and
    // therefore its children array — reference-identical, which is what the
    // SubTurnCard memo bails out on.
    let frozenGroup1: SubTurnGroup | undefined;
    for (const blocks of oneBlockAtATime) {
      const items = state.sync(blocks);
      const turn2Exists = items.some(
        (item): item is Extract<TranscriptItem, { kind: "group" }> => item.kind === "group" && item.group.subTurn === 2,
      );
      const group1 = items.find(
        (item): item is Extract<TranscriptItem, { kind: "group" }> => item.kind === "group" && item.group.subTurn === 1,
      );
      if (group1 && turn2Exists) {
        if (frozenGroup1) expect(group1.group).toBe(frozenGroup1);
        frozenGroup1 = group1.group;
      }
    }
    expect(frozenGroup1).toBeDefined();
    expect(frozenGroup1!.usage).toMatchObject({ sub_turn: 1 });
    expect(frozenGroup1!.blocks.map((b) => b.type)).toEqual(["assistant"]);
  });

  it("produces the same items whether folded incrementally or all at once", () => {
    const events = sampleEvents();
    const blocks = foldEvents(events);

    const state = new SubTurnGroupState();
    for (let n = 1; n <= blocks.length; n++) state.sync(blocks.slice(0, n));
    expect(state.sync(blocks)).toEqual(groupBySubTurn(blocks));
  });
});
