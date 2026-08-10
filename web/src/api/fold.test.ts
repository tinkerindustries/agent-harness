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

// The reasoning panel figures: elapsed_ms rides the turn_finished payload
// (created_at is one instant for a whole batch and cannot express it) and the
// token count is the API's reasoning_tokens from the sub-turn's usage event.
describe("reasoning panel figures", () => {
  function usage(subTurn: number, reasoningTokens: number, attempt?: number): StoreEvent {
    return ev(99, "usage", {
      sub_turn: subTurn,
      attempt,
      prompt_tokens: 10,
      prompt_cache_hit_tokens: 5,
      prompt_cache_miss_tokens: 5,
      completion_tokens: 3,
      reasoning_tokens: reasoningTokens,
      cost_usd: 0,
      expected_miss_tokens: 5,
    });
  }

  it("carries the measured elapsed_ms from turn_finished onto the assistant block", () => {
    const blocks = foldEvents([
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "reasoning_delta", { text: "hmm" }),
      ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop", elapsed_ms: 124200 }),
    ]);
    const assistant = blocks.find((b) => b.type === "assistant");
    expect(assistant && "reasoningElapsedMs" in assistant ? assistant.reasoningElapsedMs : undefined).toBe(124200);
  });

  it("leaves the assistant block with no elapsed segment and no tokens when the events carry neither field", () => {
    // A session committed before elapsed_ms existed: no duration segment and
    // the ~chars/4 estimate retained, rather than a made-up 0.0s.
    const blocks = foldEvents([
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "reasoning_delta", { text: "hmm" }),
      ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }),
    ]);
    const assistant = blocks.find((b) => b.type === "assistant");
    expect(assistant && "reasoningElapsedMs" in assistant ? assistant.reasoningElapsedMs : undefined).toBeUndefined();
    expect(assistant && "reasoningTokens" in assistant ? assistant.reasoningTokens : undefined).toBeUndefined();
  });

  it("takes the reasoning token count from the usage event rather than estimating", () => {
    const blocks = foldEvents([
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "reasoning_delta", { text: "hmm" }),
      ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }),
      usage(1, 14293),
    ]);
    const assistant = blocks.find((b) => b.type === "assistant");
    expect(assistant && "reasoningTokens" in assistant ? assistant.reasoningTokens : undefined).toBe(14293);
  });

  it("attaches tokens once, from the usage event that follows turn_finished", () => {
    // The starved retry commits its first attempt's usage before
    // turn_finished; the turn is still live then, so that one must not
    // attach. The retry's own usage, after turn_finished, does.
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(usage(1, 4000, 1));
    state.ingest(ev(3, "reasoning_delta", { text: "hmm" }));
    state.ingest(ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }));
    const assistant = state.blocks.find((b) => b.type === "assistant");
    expect(assistant && "reasoningTokens" in assistant ? assistant.reasoningTokens : undefined).toBeUndefined();

    state.ingest(usage(1, 6820, 2));
    const amended = state.blocks.find((b) => b.type === "assistant");
    expect(amended && "reasoningTokens" in amended ? amended.reasoningTokens : undefined).toBe(6820);
  });
});

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
    // This is the property the freeze depends on: FrozenBlocks is
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

// The steer block (docs/RUN-CONTROL.md "The frontend"): emitted by
// steer_message in the *pending* state and flipped to *delivered* by the
// matching steer_applied (matched by source_seq). This is the one place the
// browser fold completes a block it has already emitted — the Go fold's
// append-only rule protects the prompt cache, which display blocks have no
// stake in, and the two states are exactly how an operator tells a wedged
// run from a busy one.
describe("steer block", () => {
  it("emits a pending steer block on steer_message and flips it to delivered on the matching steer_applied", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "steer_message", { text: "be terse", source: "web" }));
    expect(state.blocks).toEqual([
      { type: "opening", seq: 1, text: "x" },
      { type: "steer", seq: 2, text: "be terse", state: "pending" },
    ]);

    state.ingest(ev(3, "steer_applied", { source_seq: 2, text: "be terse", sub_turn: 1 }));
    expect(state.blocks).toEqual([
      { type: "opening", seq: 1, text: "x" },
      { type: "steer", seq: 2, text: "be terse", state: "delivered" },
    ]);
  });

  it("matches by source_seq, so one steer_applied flips only its own steer", () => {
    const state = new FoldState();
    state.ingest(ev(1, "steer_message", { text: "first" }));
    state.ingest(ev(2, "steer_message", { text: "second" }));
    state.ingest(ev(3, "steer_applied", { source_seq: 2, text: "second", sub_turn: 1 }));
    expect(state.blocks).toEqual([
      { type: "steer", seq: 1, text: "first", state: "pending" },
      { type: "steer", seq: 2, text: "second", state: "delivered" },
    ]);
  });

  it("keeps the block at the position where the steer was sent — the flip never moves it", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "steer_message", { text: "be terse" }));
    state.ingest(ev(3, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }));
    state.ingest(ev(5, "steer_applied", { source_seq: 2, text: "be terse", sub_turn: 1 }));

    const steer = state.blocks.find((b) => b.type === "steer");
    expect(steer).toEqual({ type: "steer", seq: 2, text: "be terse", state: "delivered" });
    expect(state.blocks.map((b) => b.type)).toEqual(["opening", "steer", "assistant"]);
  });

  it("is the one deliberate divergence from the append-only property", () => {
    // A prefix that ends at the steer_message folds to a *pending* steer; the
    // full log folds to *delivered*. The Go fold would be broken by such a
    // rewrite (the prompt cache depends on append-only); display blocks are
    // not, and the pending state is the feature (web/src/api/fold.ts).
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "steer_message", { text: "be terse" }),
      ev(3, "steer_applied", { source_seq: 2, text: "be terse", sub_turn: 1 }),
    ];
    const prefix = foldEvents(events.slice(0, 2));
    const full = foldEvents(events);
    expect(prefix[1]).toEqual({ type: "steer", seq: 2, text: "be terse", state: "pending" });
    expect(full[1]).toEqual({ type: "steer", seq: 2, text: "be terse", state: "delivered" });
    // Everything else still agrees: the opening block is untouched.
    expect(prefix[0]).toEqual(full[0]);
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
