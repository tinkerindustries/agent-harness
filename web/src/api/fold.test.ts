import { describe, expect, it } from "vitest";
import { FoldState, foldEvents } from "./fold";
import type { StoreEvent } from "./types";

function ev(seq: number, kind: StoreEvent["kind"], payload: unknown): StoreEvent {
  return { session_id: "s", seq, kind, payload, created_at: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z` };
}

// A minimal but realistic sub-turn: a tool call that completes with a
// result, mirroring the shape internal/session actually commits.
function sampleEvents(): StoreEvent[] {
  return [
    ev(1, "session_started", { opening_message: "do the task" }),
    ev(2, "turn_started", { sub_turn: 1 }),
    ev(3, "reasoning_delta", { text: "thinking " }),
    ev(4, "reasoning_delta", { text: "more" }),
    ev(5, "content_delta", { text: "ok" }),
    ev(6, "tool_call", { index: 0, id: "call_1", name: "Bash", arguments: '{"command":"echo hi"}' }),
    ev(7, "turn_finished", { finish_reason: "tool_calls" }),
    ev(8, "tool_stdout", { tool_call_id: "call_1", text: "hi\n" }),
    ev(9, "tool_result", { tool_call_id: "call_1", name: "Bash", content: "hi\n" }),
    ev(10, "usage", { prompt_tokens: 10, prompt_cache_hit_tokens: 5, prompt_cache_miss_tokens: 5, completion_tokens: 3, reasoning_tokens: 0, cost_usd: 0, expected_miss_tokens: 5 }),
    ev(11, "run_finished", { reason: "complete", status: "ok", text: "done" }),
  ];
}

describe("foldEvents", () => {
  it("produces one block per completed unit: opening, assistant, tool_result, usage, run_finished", () => {
    const blocks = foldEvents(sampleEvents());
    expect(blocks.map((b) => b.type)).toEqual(["opening", "assistant", "tool_result", "usage", "run_finished"]);
  });

  it("accumulates reasoning and content deltas onto the frozen assistant block", () => {
    const blocks = foldEvents(sampleEvents());
    const assistant = blocks.find((b) => b.type === "assistant");
    expect(assistant).toMatchObject({ reasoning: "thinking more", content: "ok" });
  });

  it("carries the diff and child_session_id fields through onto a tool_result block when present", () => {
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "tool_call", { index: 0, id: "call_e", name: "Edit", arguments: '{"file_path":"a.go"}' }),
      ev(4, "turn_finished", { finish_reason: "tool_calls" }),
      ev(5, "tool_result", {
        tool_call_id: "call_e",
        name: "Edit",
        content: "edited",
        diff: [{ kind: "remove", text: "old", old_line: 1 }],
      }),
    ];
    const blocks = foldEvents(events);
    const result = blocks.find((b) => b.type === "tool_result");
    expect(result).toMatchObject({ diff: [{ kind: "remove", text: "old", old_line: 1 }] });
  });

  it("attaches the originating tool_call's arguments to the frozen tool_result block", () => {
    const blocks = foldEvents(sampleEvents());
    const result = blocks.find((b) => b.type === "tool_result");
    expect(result && "call" in result ? result.call?.name : undefined).toBe("Bash");
  });

  it("is append-only: folding a prefix of the log is a prefix of folding the whole log", () => {
    const events = sampleEvents();
    for (let n = 1; n <= events.length; n++) {
      const prefixBlocks = foldEvents(events.slice(0, n));
      const fullBlocks = foldEvents(events);
      for (let i = 0; i < prefixBlocks.length; i++) {
        expect(prefixBlocks[i]).toEqual(fullBlocks[i]);
      }
    }
  });
});

describe("FoldState live view", () => {
  it("holds an in-progress turn in live.turn until turn_finished, then clears it", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(3, "reasoning_delta", { text: "hmm" }));
    expect(state.live.turn).toMatchObject({ reasoning: "hmm", subTurn: 1 });
    expect(state.blocks).toHaveLength(1); // only the opening block is frozen so far

    state.ingest(ev(4, "turn_finished", { finish_reason: "stop" }));
    expect(state.live.turn).toBeNull();
    expect(state.blocks.map((b) => b.type)).toEqual(["opening", "assistant"]);
  });

  it("tracks a pending tool call's live stdout until its result arrives", () => {
    const state = new FoldState();
    for (const e of sampleEvents().slice(0, 7)) state.ingest(e); // through turn_finished
    expect(state.live.pendingTools.has("call_1")).toBe(true);
    expect(state.live.pendingTools.get("call_1")?.stdout).toBe("");

    state.ingest(ev(8, "tool_stdout", { tool_call_id: "call_1", text: "hi\n" }));
    expect(state.live.pendingTools.get("call_1")?.stdout).toBe("hi\n");

    state.ingest(ev(9, "tool_result", { tool_call_id: "call_1", name: "Bash", content: "hi\n" }));
    expect(state.live.pendingTools.has("call_1")).toBe(false);
  });

  it("keeps the blocks array reference stable across events that do not freeze a new block", () => {
    // This is the property PLAN.md phase 5 depends on: FrozenBlocks is
    // memoised on the blocks array reference, so a reasoning/content/
    // tool_stdout delta must not produce a new array — only a block
    // actually freezing should.
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    const afterOpening = state.blocks;

    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(3, "reasoning_delta", { text: "a" }));
    state.ingest(ev(4, "content_delta", { text: "b" }));
    expect(state.blocks).toBe(afterOpening);

    state.ingest(ev(5, "turn_finished", { finish_reason: "stop" }));
    expect(state.blocks).not.toBe(afterOpening);
  });

  it("parses the latest TodoWrite call's arguments into latestTodos", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(3, "tool_call", {
        index: 0,
        id: "call_t",
        name: "TodoWrite",
        arguments: JSON.stringify({ todos: [{ content: "a", status: "pending", activeForm: "doing a" }] }),
      }),
    );
    expect(state.latestTodos).toEqual([{ content: "a", status: "pending", activeForm: "doing a" }]);
  });

  it("leaves latestTodos unchanged when a TodoWrite call's arguments are not yet valid JSON", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(2, "tool_call", { index: 0, id: "call_t", name: "TodoWrite", arguments: '{"todos":[{"content":' }));
    expect(state.latestTodos).toEqual([]);
  });
});

// The skills catalogue is a substring of the opening message. It gets its own
// block so the transcript does not print it twice (internal/skills).
describe("skills catalogue", () => {
  const catalogue = "Skills available in this workspace.\n\n- test-runner (repo/.claude/skills/test-runner/SKILL.md): Run the suite.\n";
  const opening = `Workspace: /ws\n\n${catalogue}\nTask:\ndo the thing\n`;

  it("splits the catalogue out of the opening message into its own block", () => {
    const blocks = foldEvents([ev(1, "session_started", { opening_message: opening, skill_catalogue: catalogue })]);
    expect(blocks.map((b) => b.type)).toEqual(["skills", "opening"]);

    const skills = blocks.find((b) => b.type === "skills");
    expect(skills).toMatchObject({ text: catalogue });

    const openingBlock = blocks.find((b) => b.type === "opening");
    expect(openingBlock && "text" in openingBlock && openingBlock.text).not.toContain("test-runner");
    expect(openingBlock && "text" in openingBlock && openingBlock.text).toContain("do the thing");
  });

  it("pushes only an opening block when the run found no skills", () => {
    const blocks = foldEvents([ev(1, "session_started", { opening_message: "Workspace: /ws\n\nTask:\ndo the thing\n" })]);
    expect(blocks.map((b) => b.type)).toEqual(["opening"]);
  });

  it("leaves the opening message intact when the catalogue is not a substring of it", () => {
    const blocks = foldEvents([ev(1, "session_started", { opening_message: opening, skill_catalogue: "something else" })]);
    expect(blocks.map((b) => b.type)).toEqual(["opening"]);
    const openingBlock = blocks[0];
    expect("text" in openingBlock && openingBlock.text).toEqual(opening);
  });
});
