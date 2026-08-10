import { describe, expect, it } from "vitest";
import { FoldState, foldEvents } from "./fold";
import { groupBySubTurn, groupMatchesFilter, phaseFromTodos, SubTurnGroupState, type SubTurnGroup, type TranscriptItem } from "./groups";
import type { StoreEvent, Todo } from "./types";

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

// A session whose filter families are all represented: turn 1 a clean Bash,
// turn 2 an Edit followed by a failing Bash, turn 3 a churned usage.
function filterSampleEvents(): StoreEvent[] {
  return [
    ev(1, "session_started", { opening_message: "x" }),
    // turn 1: one clean Bash call
    ev(2, "turn_started", { sub_turn: 1 }),
    ev(3, "tool_call", { index: 0, id: "c1", name: "Bash", arguments: '{"command":"npm test"}' }),
    ev(4, "turn_finished", { finish_reason: "tool_calls" }),
    ev(5, "tool_result", { tool_call_id: "c1", name: "Bash", content: "ok\n" }),
    usage(1),
    // turn 2: an Edit and a failing Bash
    ev(6, "turn_started", { sub_turn: 2 }),
    ev(7, "tool_call", { index: 0, id: "c2", name: "Edit", arguments: '{"file_path":"a.go"}' }),
    ev(8, "tool_call", { index: 1, id: "c3", name: "Bash", arguments: '{"command":"go build ./..."}' }),
    ev(9, "turn_finished", { finish_reason: "tool_calls" }),
    ev(10, "tool_result", {
      tool_call_id: "c2",
      name: "Edit",
      content: "edited",
      diff: [
        { kind: "remove", text: "a", old_line: 1 },
        { kind: "add", text: "b", new_line: 1 },
      ],
    }),
    ev(11, "tool_result", { tool_call_id: "c3", name: "Bash", content: "failed\n[exit code 2]", is_error: true }),
    usage(2),
    // turn 3: a churned usage
    ev(12, "turn_started", { sub_turn: 3 }),
    ev(13, "turn_finished", { finish_reason: "stop" }),
    usage(3, { prompt_cache_miss_tokens: 500, expected_miss_tokens: 100, churn_point_index: 88 }),
  ];
}

describe("filter counts", () => {
  it("maintains per-card counts from the same pass that builds the groups", () => {
    const state = new SubTurnGroupState();
    state.sync(foldEvents(filterSampleEvents()));
    // Two Bash cards (turns 1 and 2), one Edit card, one error card (turn 2's
    // failing Bash), one churned card — the families count cards, not calls.
    expect(state.counts).toEqual({ total: 3, edits: 1, bash: 2, errors: 1, churn: 1 });
  });

  it("counts are identical whether folded incrementally or all at once", () => {
    const blocks = foldEvents(filterSampleEvents());
    const state = new SubTurnGroupState();
    for (let n = 1; n <= blocks.length; n++) state.sync(blocks.slice(0, n));
    const once = new SubTurnGroupState();
    once.sync(blocks);
    expect(state.counts).toEqual(once.counts);
    expect(state.churnPoint).toEqual(once.churnPoint);
  });

  it("matches a group against its filter family", () => {
    const items = groupBySubTurn(foldEvents(filterSampleEvents()));
    const byTurn = new Map<number, SubTurnGroup>();
    for (const item of items) {
      if (item.kind === "group") byTurn.set(item.group.subTurn, item.group);
    }
    const turn1 = byTurn.get(1)!;
    const turn2 = byTurn.get(2)!;
    const turn3 = byTurn.get(3)!;
    expect(groupMatchesFilter(turn1, "bash")).toBe(true);
    expect(groupMatchesFilter(turn1, "edits")).toBe(false);
    expect(groupMatchesFilter(turn2, "edits")).toBe(true);
    expect(groupMatchesFilter(turn2, "errors")).toBe(true);
    expect(groupMatchesFilter(turn2, "bash")).toBe(true);
    expect(groupMatchesFilter(turn3, "churn")).toBe(true);
    expect(groupMatchesFilter(turn3, "errors")).toBe(false);
    for (const group of [turn1, turn2, turn3]) expect(groupMatchesFilter(group, "all")).toBe(true);
  });

  it("records the first churn point with the tokens re-sent above the expected miss", () => {
    const state = new SubTurnGroupState();
    state.sync(foldEvents(filterSampleEvents()));
    expect(state.churnPoint).toEqual({ subTurn: 3, excessTokens: 400 });
  });

  it("records a churn carried by a loose usage (starved retry) too", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      // First attempt's usage arrives before turn_finished, so it stays a
      // loose block — but it still names the churn the banner must show.
      usage(1, { attempt: 1, prompt_cache_miss_tokens: 600, expected_miss_tokens: 100, churn_point_index: 12 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1, { attempt: 2 }),
    ];
    const state = new SubTurnGroupState();
    state.sync(foldEvents(events));
    expect(state.churnPoint).toEqual({ subTurn: 1, excessTokens: 500 });
  });
});

// The timeline rail's phase grouping (docs/WEB-REDESIGN.md phase 6): every
// TodoWrite call in the event stream starts a phase, and the phase is named
// after the plan item that was in_progress when it ran — read from the fold's
// latestTodos, which the store records per block (TranscriptStore.ingest)
// and passes into sync. foldedItems below folds one event at a time, which
// exercises the same naming the store's per-block record produces; the
// batch test further down checks the all-at-once flush path the store
// actually uses for a burst.
function todoWrite(seq: number, id: string, todos: Todo[]): StoreEvent {
  return ev(seq, "tool_call", { index: 0, id, name: "TodoWrite", arguments: JSON.stringify({ todos }) });
}

function todo(content: string, status: Todo["status"]): Todo {
  return { content, status, activeForm: `working on ${content}` };
}

// foldedItems runs the store's exact pipeline — fold each event, then sync
// the groups with the fold's current latestTodos — so phase labels are
// captured at the moment each group freezes, exactly as in production.
function foldedItems(events: StoreEvent[]): TranscriptItem[] {
  const fold = new FoldState();
  const groups = new SubTurnGroupState();
  for (const event of events) {
    fold.ingest(event);
    groups.sync(fold.blocks, fold.latestTodos);
  }
  return groups.sync(fold.blocks, fold.latestTodos);
}

function groupsByTurn(items: TranscriptItem[]): Map<number, SubTurnGroup> {
  const byTurn = new Map<number, SubTurnGroup>();
  for (const item of items) {
    if (item.kind === "group") byTurn.set(item.group.subTurn, item.group);
  }
  return byTurn;
}

describe("rail phase assignment", () => {
  it("starts a phase on every TodoWrite call and names it from the fold's latestTodos at that moment", () => {
    const plan1 = [todo("Fix retained-body leak", "in_progress"), todo("Add httplog test", "pending")];
    const plan2 = [todo("Fix retained-body leak", "completed"), todo("Add httplog test", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      // turn 1 writes the plan: phase 1 = item 1
      ev(2, "turn_started", { sub_turn: 1 }),
      todoWrite(3, "p1", plan1),
      ev(4, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      // turn 2 makes no TodoWrite: still phase 1
      ev(5, "turn_started", { sub_turn: 2 }),
      ev(6, "turn_finished", { finish_reason: "stop" }),
      usage(2),
      // turn 3 writes the next plan: phase 2 = item 2
      ev(7, "turn_started", { sub_turn: 3 }),
      todoWrite(8, "p2", plan2),
      ev(9, "turn_finished", { finish_reason: "stop" }),
      usage(3),
    ];
    const byTurn = groupsByTurn(foldedItems(events));
    expect(byTurn.get(1)!.phase).toEqual({ id: 1, index: 1, label: "Fix retained-body leak" });
    expect(byTurn.get(2)!.phase).toEqual({ id: 1, index: 1, label: "Fix retained-body leak" });
    expect(byTurn.get(3)!.phase).toEqual({ id: 2, index: 2, label: "Add httplog test" });
    // Groups in the same phase share the phase ref object; a phase change
    // is a new object.
    expect(byTurn.get(1)!.phase).toBe(byTurn.get(2)!.phase);
    expect(byTurn.get(3)!.phase).not.toBe(byTurn.get(1)!.phase);
  });

  it("names the first phase from the first TodoWrite even when it arrives after earlier sub-turns", () => {
    const plan = [todo("Survey", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      // sub-turns 1-2 ran before any plan existed: phase 0, no label
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "turn_started", { sub_turn: 2 }),
      ev(5, "turn_finished", { finish_reason: "stop" }),
      usage(2),
      // sub-turn 3 writes the first plan: the boundary sub-turn belongs to
      // the phase its own TodoWrite opens
      ev(6, "turn_started", { sub_turn: 3 }),
      todoWrite(7, "p1", plan),
      ev(8, "turn_finished", { finish_reason: "stop" }),
      usage(3),
    ];
    const byTurn = groupsByTurn(foldedItems(events));
    expect(byTurn.get(1)!.phase).toEqual({ id: 0, index: 0, label: "" });
    expect(byTurn.get(2)!.phase).toEqual({ id: 0, index: 0, label: "" });
    expect(byTurn.get(3)!.phase).toEqual({ id: 1, index: 1, label: "Survey" });
  });

  it("names each phase from the boundary sub-turn's own plan when a flush folds a whole batch at once", () => {
    const plan1 = [todo("Fix retained-body leak", "in_progress"), todo("Add httplog test", "pending")];
    const plan2 = [todo("Fix retained-body leak", "completed"), todo("Add httplog test", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      // phase 1 = item 1, phase 2 = item 2 — the same history the per-event
      // test above uses, but folded in ONE sync call as the store does when
      // a burst (a replay, or the perf harness seeding) lands in one flush.
      ev(2, "turn_started", { sub_turn: 1 }),
      todoWrite(3, "p1", plan1),
      ev(4, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(5, "turn_started", { sub_turn: 2 }),
      ev(6, "turn_finished", { finish_reason: "stop" }),
      usage(2),
      ev(7, "turn_started", { sub_turn: 3 }),
      todoWrite(8, "p2", plan2),
      ev(9, "turn_finished", { finish_reason: "stop" }),
      usage(3),
    ];
    // What TranscriptStore.ingest records: the fold's latestTodos as of each
    // block it froze — never a re-parse of the TodoWrite arguments.
    const fold = new FoldState();
    const todosAtBlock: Todo[][] = [];
    for (const event of events) {
      const before = fold.blocks.length;
      fold.ingest(event);
      for (let i = before; i < fold.blocks.length; i++) todosAtBlock.push(fold.latestTodos);
    }
    const groups = new SubTurnGroupState();
    const items = groups.sync(fold.blocks, fold.latestTodos, todosAtBlock);
    const byTurn = groupsByTurn(items);
    // Named from each boundary sub-turn's own plan — with only the batch-end
    // plan (fold.latestTodos), phase 1 would be named "Add httplog test"
    // and phase 2 would collapse into it.
    expect(byTurn.get(1)!.phase).toEqual({ id: 1, index: 1, label: "Fix retained-body leak" });
    expect(byTurn.get(2)!.phase).toEqual({ id: 1, index: 1, label: "Fix retained-body leak" });
    expect(byTurn.get(3)!.phase).toEqual({ id: 2, index: 2, label: "Add httplog test" });
  });

  it("keeps a group's phase through its own tool-result append (the tail-group replacement path)", () => {
    const plan = [todo("Edit config", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      todoWrite(3, "p1", plan),
      ev(4, "tool_call", { index: 1, id: "c1", name: "Edit", arguments: '{"file_path":"a.go"}' }),
      ev(5, "turn_finished", { finish_reason: "tool_calls" }),
      ev(6, "tool_result", { tool_call_id: "c1", name: "Edit", content: "edited" }),
      usage(1),
    ];
    const items = foldedItems(events);
    const group = groupsByTurn(items).get(1)!;
    // The group was created with the phase; the tool-result append replaced
    // the group object but must not re-assign its phase.
    expect(group.phase).toEqual({ id: 1, index: 1, label: "Edit config" });
    expect(group.blocks.map((b) => b.type)).toEqual(["assistant", "tool_result"]);
  });

  it("falls back to the first non-completed item when nothing is in_progress, and to empty when the plan is empty", () => {
    expect(phaseFromTodos([todo("a", "pending"), todo("b", "completed")], 3)).toEqual({ id: 3, index: 1, label: "a" });
    expect(phaseFromTodos([todo("a", "completed"), todo("b", "completed")], 3)).toEqual({ id: 3, index: 0, label: "" });
    expect(phaseFromTodos([], 3)).toEqual({ id: 3, index: 0, label: "" });
  });
});
