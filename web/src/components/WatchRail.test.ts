import { describe, expect, it } from "vitest";
import { FoldState } from "../api/fold";
import { SubTurnGroupState, type TranscriptItem } from "../api/groups";
import type { StoreEvent, Todo } from "../api/types";
import { buildWatchPhases, type WatchPhasesView } from "./WatchRail";

// The watch rail's view model (buildWatchPhases) is pure and unit-testable;
// the component itself stays untested, per TESTING.md ("There is no DOM
// harness and the components are not unit-tested"). It pins the two things
// the rail is for: one phase per plan item with one coloured tick per
// sub-turn, and the "not started" rows for plan items the run never
// reached.

function ev(seq: number, kind: StoreEvent["kind"], payload: unknown): StoreEvent {
  return { session_id: "s", seq, kind, payload, created_at: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z` };
}

function usage(subTurn: number): StoreEvent {
  return ev(99 + subTurn, "usage", {
    sub_turn: subTurn,
    prompt_tokens: 10,
    prompt_cache_hit_tokens: 5,
    prompt_cache_miss_tokens: 5,
    completion_tokens: 3,
    reasoning_tokens: 0,
    cost_usd: 0.0001,
    expected_miss_tokens: 5,
  });
}

function churnUsage(subTurn: number): StoreEvent {
  const u = usage(subTurn).payload as Record<string, unknown>;
  return ev(99 + subTurn, "usage", { ...u, prompt_cache_miss_tokens: 600, expected_miss_tokens: 100, churn_point_index: 1 });
}

function taskCreate(seq: number, id: string, tasks: Todo[]): StoreEvent {
  const stripped = tasks.map(({ subject, description, status, activeForm }) => ({ subject, description, activeForm, status }));
  return ev(seq, "tool_call", { index: 0, id, name: "TaskCreate", arguments: JSON.stringify({ tasks: stripped }) });
}

function taskUpdate(
  seq: number,
  id: string,
  args: { taskId: string; status?: Todo["status"] | "deleted" },
): StoreEvent {
  return ev(seq, "tool_call", { index: 0, id, name: "TaskUpdate", arguments: JSON.stringify(args) });
}

function todo(id: string, subject: string, status: Todo["status"]): Todo {
  return { taskId: id, subject, description: `${subject} in detail`, status, activeForm: `working on ${subject}` };
}

// foldedItems runs the store's pipeline (fold each event, sync groups with
// the fold's latestTodos) so buildWatchPhases gets items with real phase
// refs, exactly as the rail reads them off a snapshot.
function foldedItems(events: StoreEvent[]): TranscriptItem[] {
  const fold = new FoldState();
  const groups = new SubTurnGroupState();
  for (const event of events) {
    fold.ingest(event);
    groups.sync(fold.blocks, fold.latestTodos);
  }
  return groups.sync(fold.blocks, fold.latestTodos);
}

// turn is the minimal turn lifecycle: start, a tool call, finish, usage.
function turn(seq: number, subTurn: number, calls: StoreEvent[] = []): StoreEvent[] {
  return [ev(seq, "turn_started", { sub_turn: subTurn }), ...calls, ev(seq + 1, "turn_finished", { sub_turn: subTurn, finish_reason: "stop" }), usage(subTurn)];
}

const NOOP_GET_TOOL_CALL = (): undefined => undefined;

describe("buildWatchPhases", () => {
  it("groups consecutive sub-turns under one phase, one tick each, coloured from the group's tags", () => {
    const plan1 = [todo("1", "Fix retained-body leak", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      // phase 1: sub-turns 1-2 — sub-turn 2 edits
      ...turn(2, 1, [taskCreate(3, "p1", plan1)]),
      ev(5, "turn_started", { sub_turn: 2 }),
      ev(6, "tool_call", { index: 0, id: "e1", name: "Edit", arguments: '{"file_path":"a.go"}' }),
      ev(7, "turn_finished", { sub_turn: 2, finish_reason: "tool_calls" }),
      ev(8, "tool_result", { tool_call_id: "e1", name: "Edit", content: "edited", diff: [] }),
      usage(2),
      // phase 2: sub-turn 3 — a Bash failure
      ev(9, "turn_started", { sub_turn: 3 }),
      taskUpdate(10, "u1", { taskId: "1", status: "completed" }),
      taskCreate(11, "p2", [todo("2", "Add httplog test", "in_progress")]),
      ev(12, "tool_call", { index: 0, id: "b1", name: "Bash", arguments: '{"command":"go build"}' }),
      ev(13, "turn_finished", { sub_turn: 3, finish_reason: "tool_calls" }),
      ev(14, "tool_result", { tool_call_id: "b1", name: "Bash", content: "failed", is_error: true }),
      usage(3),
    ];
    const view = buildWatchPhases(foldedItems(events), null, [todo("2", "Add httplog test", "in_progress")], false, NOOP_GET_TOOL_CALL);
    expect(view.phases.map((p) => [p.index, p.label, p.ticks.length])).toEqual([
      [1, "Fix retained-body leak", 2],
      [2, "Add httplog test", 1],
    ]);
    expect(view.phases[0].ticks.map((t) => t.cls)).toEqual(["", "tick-edit"]);
    expect(view.phases[1].ticks.map((t) => t.cls)).toEqual(["tick-err"]);
    expect(view.nowPhaseId).toBeNull();
  });

  it("pulses the streaming sub-turn's tick in the tail phase, which is now", () => {
    const plan = [todo("1", "Fix retained-body leak", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", plan)]),
      ev(6, "turn_started", { sub_turn: 2 }),
    ];
    const view = buildWatchPhases(foldedItems(events), 2, plan, true, NOOP_GET_TOOL_CALL);
    expect(view.phases).toHaveLength(1);
    expect(view.phases[0].ticks.map((t) => [t.subTurn, t.cls])).toEqual([
      [1, ""],
      [2, "tick-now"],
    ]);
    expect(view.nowPhaseId).toBe(view.phases[0].id);
  });

  it("keeps the tail phase highlighted while a tool round is pending", () => {
    const plan = [todo("1", "Fix retained-body leak", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      taskCreate(3, "p1", plan),
      ev(4, "turn_finished", { sub_turn: 1, finish_reason: "tool_calls" }),
    ];
    const view = buildWatchPhases(foldedItems(events), null, plan, true, NOOP_GET_TOOL_CALL);
    expect(view.phases).toHaveLength(1);
    // No streaming turn, so no pulsing tick — but the phase is still now.
    expect(view.phases[0].ticks).toHaveLength(1);
    expect(view.nowPhaseId).toBe(view.phases[0].id);
  });

  it("renders plan items the run never reached as not-started, past the last phase's position", () => {
    // The plan at the first TaskCreate already has its second item
    // in_progress, so the one phase is named "Fix the bug" at position 2;
    // "Add tests" sits past it and never runs.
    const plan = [todo("1", "Read the docs", "completed"), todo("2", "Fix the bug", "in_progress"), todo("3", "Add tests", "pending")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", plan)]),
    ];
    const view = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    expect(view.phases.map((p) => [p.index, p.label])).toEqual([[2, "Fix the bug"]]);
    expect(view.notStarted).toEqual([{ index: 3, label: "Add tests" }]);
  });

  it("keeps every sub-turn in one unnamed phase, with no not-started rows, when no plan mutation ever happened", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1),
      ...turn(5, 2),
    ];
    const view = buildWatchPhases(foldedItems(events), null, [], false, NOOP_GET_TOOL_CALL);
    expect(view.phases).toHaveLength(1);
    expect(view.phases[0]).toMatchObject({ index: 0, label: "" });
    expect(view.phases[0].ticks).toHaveLength(2);
    expect(view.notStarted).toEqual([]);
  });

  it("colours a churn sub-turn amber, churn losing only to an error", () => {
    const plan = [todo("1", "Do the thing", "in_progress")];
    const churnOnly = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", plan)]),
    ];
    // swap the usage for a churn one
    const events = churnOnly.map((e) => (e.kind === "usage" ? churnUsage(1) : e));
    const view = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    expect(view.phases[0].ticks[0].cls).toBe("tick-churn");
  });

  it("skips the top-level blocks (opening, run_finished) that are not sub-turn cards", () => {
    const plan = [todo("1", "Do the thing", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", plan)]),
      ev(6, "run_finished", { reason: "complete", status: "done", text: "done" }),
    ];
    const view: WatchPhasesView = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    expect(view.phases).toHaveLength(1);
    expect(view.phases[0].ticks).toHaveLength(1);
  });

  it("lists no not-started rows when every plan item is completed", () => {
    // The observed 7/7 session: each item completed by its own TaskUpdate,
    // the last one leaving the plan with nothing in_progress. Every item
    // ran, so none of them may reappear as "not started".
    const plan = [todo("1", "a", "completed"), todo("2", "b", "completed")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", [todo("1", "a", "in_progress"), todo("2", "b", "pending")])]),
      ...turn(6, 2, [taskUpdate(7, "u1", { taskId: "1", status: "completed" })]),
      ...turn(9, 3, [taskUpdate(10, "u2", { taskId: "2", status: "completed" })]),
    ];
    const view = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    expect(view.notStarted).toEqual([]);
  });

  it("names the trailing phase after the last plan item once every item is completed", () => {
    // The final TaskUpdate leaves the plan with nothing in_progress and
    // nothing non-completed; the phase the following sub-turns land in is
    // still the last item's, not an unlabelled one.
    const plan = [todo("1", "a", "completed")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", [todo("1", "a", "pending")])]),
      ...turn(6, 2, [taskUpdate(7, "u1", { taskId: "1", status: "completed" })]),
    ];
    const view = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    const tail = view.phases[view.phases.length - 1];
    expect(tail).toMatchObject({ index: 1, label: "a" });
    expect(tail.ticks.map((t) => t.subTurn)).toEqual([1, 2]);
  });

  it("merges consecutive phases that name the same plan item even when their phase ids differ", () => {
    // The create-then-mark-in_progress pair mints two phase ids naming the
    // same item; the groups collapse into one phase keyed on the first id,
    // carrying both sub-turns' ticks.
    const plan = [todo("1", "a", "in_progress"), todo("2", "b", "pending")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ...turn(2, 1, [taskCreate(3, "p1", [todo("1", "a", "pending"), todo("2", "b", "pending")])]),
      ...turn(6, 2, [taskUpdate(7, "u1", { taskId: "1", status: "in_progress" })]),
    ];
    const view = buildWatchPhases(foldedItems(events), null, plan, false, NOOP_GET_TOOL_CALL);
    expect(view.phases).toHaveLength(1);
    expect(view.phases[0]).toMatchObject({ id: 1, index: 1, label: "a" });
    expect(view.phases[0].ticks.map((t) => t.subTurn)).toEqual([1, 2]);
  });
});
