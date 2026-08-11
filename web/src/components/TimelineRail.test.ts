import { describe, expect, it } from "vitest";
import { FoldState } from "../api/fold";
import { SubTurnGroupState, type TranscriptItem } from "../api/groups";
import type { StoreEvent, Todo } from "../api/types";
import { buildRail } from "./TimelineRail";

// The rail's view model (buildRail) is pure and unit-testable; the component
// itself stays untested, per TESTING.md ("There is no DOM harness and the
// components are not unit-tested"). The observer-count constraint is checked
// at runtime by web/src/perf/RailHarness, not asserted here.

function ev(seq: number, kind: StoreEvent["kind"], payload: unknown): StoreEvent {
  return { session_id: "s", seq, kind, payload, created_at: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z` };
}

function usage(subTurn: number): StoreEvent {
  return ev(99, "usage", {
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

type CreateTask = { subject: string; description: string; activeForm: string; status?: Todo["status"] };

function taskCreate(seq: number, id: string, tasks: Todo[]): StoreEvent {
  const stripped: CreateTask[] = tasks.map(({ subject, description, status, activeForm }) => ({ subject, description, activeForm, status }));
  return ev(seq, "tool_call", { index: 0, id, name: "TaskCreate", arguments: JSON.stringify({ tasks: stripped }) });
}

function taskUpdate(
  seq: number,
  id: string,
  args: { taskId: string; status?: Todo["status"] | "deleted"; subject?: string; description?: string; activeForm?: string },
): StoreEvent {
  return ev(seq, "tool_call", { index: 0, id, name: "TaskUpdate", arguments: JSON.stringify(args) });
}

function todo(id: string, subject: string, status: Todo["status"]): Todo {
  return { taskId: id, subject, description: `${subject} in detail`, status, activeForm: `working on ${subject}` };
}

// foldedItems runs the store's pipeline (fold each event, sync groups with
// the fold's latestTodos) so buildRail gets items with real phase refs.
function foldedItems(events: StoreEvent[]): TranscriptItem[] {
  const fold = new FoldState();
  const groups = new SubTurnGroupState();
  for (const event of events) {
    fold.ingest(event);
    groups.sync(fold.blocks, fold.latestTodos);
  }
  return groups.sync(fold.blocks, fold.latestTodos);
}

// NOOP_GET_TOOL_CALL mirrors BlockList's default: buildRail falls back to
// the assistant block's own copy of a call when the registry returns nothing.
const NOOP_GET_TOOL_CALL = (): undefined => undefined;

describe("buildRail", () => {
  it("groups consecutive sub-turns under one phase, with the phase's range", () => {
    const plan1 = [todo("1", "Fix retained-body leak", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      // phase 1: sub-turns 1-2
      ev(2, "turn_started", { sub_turn: 1 }),
      taskCreate(3, "p1", plan1),
      ev(4, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(5, "turn_started", { sub_turn: 2 }),
      ev(6, "turn_finished", { finish_reason: "stop" }),
      usage(2),
      // phase 2: sub-turn 3
      ev(7, "turn_started", { sub_turn: 3 }),
      taskUpdate(8, "u1", { taskId: "1", status: "completed" }),
      taskCreate(9, "p2", [todo("2", "Add httplog test", "in_progress")]),
      ev(10, "turn_finished", { finish_reason: "stop" }),
      usage(3),
    ];
    const phases = buildRail(foldedItems(events), NOOP_GET_TOOL_CALL);
    expect(phases.map((p) => [p.index, p.label, p.firstSubTurn, p.lastSubTurn])).toEqual([
      [1, "Fix retained-body leak", 1, 2],
      [2, "Add httplog test", 3, 3],
    ]);
    expect(phases[0].entries.map((e) => e.subTurn)).toEqual([1, 2]);
    expect(phases[1].entries.map((e) => e.subTurn)).toEqual([3]);
  });

  it("collects one glyph per tool call in call order, with a failed result overriding to red", () => {
    const plan = [todo("1", "Edit and verify", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      taskCreate(3, "p1", plan),
      ev(4, "tool_call", { index: 1, id: "c1", name: "Edit", arguments: '{"file_path":"a.go"}' }),
      ev(5, "tool_call", { index: 2, id: "c2", name: "Bash", arguments: '{"command":"go build"}' }),
      ev(6, "turn_finished", { finish_reason: "tool_calls" }),
      ev(7, "tool_result", { tool_call_id: "c1", name: "Edit", content: "edited" }),
      ev(8, "tool_result", { tool_call_id: "c2", name: "Bash", content: "failed\n[exit code 2]", is_error: true }),
      usage(1),
    ];
    const phases = buildRail(foldedItems(events), NOOP_GET_TOOL_CALL);
    expect(phases[0].entries[0].glyphs).toEqual([
      // The TaskCreate that opened the phase is a tool call too — its P
      // stands beside the turn's own calls (design/components.html).
      { letter: "P", family: "other" },
      { letter: "E", family: "write" },
      { letter: "!", family: "err" },
    ]);
  });

  it("treats a denied call as a failure and a call without a result as its plain glyph", () => {
    const plan = [todo("1", "Read carefully", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      taskCreate(3, "p1", plan),
      ev(4, "tool_call", { index: 1, id: "c1", name: "Read", arguments: '{"file_path":"secret"}' }),
      ev(5, "tool_call", { index: 2, id: "c2", name: "Grep", arguments: '{"pattern":"TODO"}' }),
      ev(6, "turn_finished", { finish_reason: "tool_calls" }),
      ev(7, "tool_denied", { tool_call_id: "c1", name: "Read", rule: "read-only", content: "denied" }),
      // c2 has no result: its plain glyph stands
      usage(1),
    ];
    const phases = buildRail(foldedItems(events), NOOP_GET_TOOL_CALL);
    expect(phases[0].entries[0].glyphs).toEqual([
      { letter: "P", family: "other" },
      { letter: "!", family: "err" },
      { letter: "G", family: "other" },
    ]);
  });

  it("keeps the whole session in one unnamed phase when no plan mutation ever happened", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(4, "turn_started", { sub_turn: 2 }),
      ev(5, "turn_finished", { finish_reason: "stop" }),
      usage(2),
    ];
    const phases = buildRail(foldedItems(events), NOOP_GET_TOOL_CALL);
    expect(phases).toHaveLength(1);
    expect(phases[0]).toMatchObject({ id: 0, index: 0, label: "", firstSubTurn: 1, lastSubTurn: 2 });
    expect(phases[0].entries).toHaveLength(2);
  });

  it("skips the top-level blocks (opening, run_finished) that are not sub-turn cards", () => {
    const plan = [todo("1", "Do the thing", "in_progress")];
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      taskCreate(3, "p1", plan),
      ev(4, "turn_finished", { finish_reason: "stop" }),
      usage(1),
      ev(5, "run_finished", { reason: "complete", status: "ok", text: "done" }),
    ];
    const phases = buildRail(foldedItems(events), NOOP_GET_TOOL_CALL);
    expect(phases).toHaveLength(1);
    expect(phases[0].entries).toHaveLength(1);
  });
});
