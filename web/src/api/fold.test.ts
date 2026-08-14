import { describe, expect, it } from "vitest";
import { applyTaskEvent, FoldState, foldEvents } from "./fold";
import type { StoreEvent, Todo } from "./types";

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

// The exported pure reducer mirrors internal/store/status.go's applyTaskEvent
// one for one — the same patch semantics the store replay applies, so the
// fold's live plan can never drift from the backend's recovered one.
describe("applyTaskEvent", () => {
  it("appends TaskCreate tasks with minted ids, status defaulting to pending", () => {
    const { todos, nextId } = applyTaskEvent(
      [],
      0,
      "TaskCreate",
      JSON.stringify({ tasks: [{ subject: "a", description: "do a", activeForm: "doing a" }] }),
    );
    expect(todos).toEqual([{ taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" }]);
    expect(nextId).toBe(1);
  });

  it("patches one task by taskId and removes it on status deleted", () => {
    const seed: Todo[] = [
      { taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" },
      { taskId: "2", subject: "b", description: "do b", status: "pending", activeForm: "doing b" },
    ];
    const patched = applyTaskEvent(seed, 2, "TaskUpdate", JSON.stringify({ taskId: "2", status: "completed", subject: "done", description: "did it" }));
    expect(patched.todos[1]).toEqual({ taskId: "2", subject: "done", description: "did it", status: "completed", activeForm: "doing b" });
    expect(patched.nextId).toBe(2);

    const deleted = applyTaskEvent(seed, 2, "TaskUpdate", JSON.stringify({ taskId: "1", status: "deleted" }));
    expect(deleted.todos).toEqual([seed[1]]);
  });

  it("returns the input unchanged for any other tool name, malformed JSON, or a rejected update", () => {
    const seed: Todo[] = [{ taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" }];
    expect(applyTaskEvent(seed, 1, "TaskGet", JSON.stringify({ taskId: "1" }))).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskList", JSON.stringify({}))).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskCreate", '{"tasks":[{"subject":')).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", '{"taskId":"1","status":')).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", JSON.stringify({ taskId: "99", status: "completed" }))).toEqual({ todos: seed, nextId: 1 });
    // No-op update (nothing to set) and "deleted" combined with a patch.
    expect(applyTaskEvent(seed, 1, "TaskUpdate", JSON.stringify({ taskId: "1" }))).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", JSON.stringify({ taskId: "1", status: "deleted", subject: "renamed" }))).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", JSON.stringify({ taskId: "1", status: "deleted", description: "changed" }))).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", JSON.stringify({ taskId: "1", status: "deleted", activeForm: "renaming" }))).toEqual({ todos: seed, nextId: 1 });
    // Invalid create items: empty subject, missing description, missing
    // activeForm, bad status.
    expect(applyTaskEvent([], 0, "TaskCreate", JSON.stringify({ tasks: [{ subject: "", description: "d", activeForm: "x" }] }))).toEqual({ todos: [], nextId: 0 });
    expect(applyTaskEvent([], 0, "TaskCreate", JSON.stringify({ tasks: [{ subject: "a", activeForm: "x" }] }))).toEqual({ todos: [], nextId: 0 });
    expect(applyTaskEvent([], 0, "TaskCreate", JSON.stringify({ tasks: [{ subject: "a", description: "d" }] }))).toEqual({ todos: [], nextId: 0 });
    expect(applyTaskEvent([], 0, "TaskCreate", JSON.stringify({ tasks: [{ subject: "a", description: "d", activeForm: "x", status: "bogus" }] }))).toEqual({ todos: [], nextId: 0 });
    // A JSON literal is not a task payload and must not throw.
    expect(applyTaskEvent(seed, 1, "TaskCreate", "null")).toEqual({ todos: seed, nextId: 1 });
    expect(applyTaskEvent(seed, 1, "TaskUpdate", "42")).toEqual({ todos: seed, nextId: 1 });
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

  it("carries the image_url data URI through onto a tool_result block when the payload has one", () => {
    // Phase 8 of the Kimi work: Read returns an image to a vision provider
    // and the bytes ride the tool_result payload as image_url
    // (docs/KIMI-INTEGRATION.md §4.5), so the transcript can render the
    // picture the model was looking at.
    const events = [
      ev(1, "session_started", { opening_message: "x" }),
      ev(2, "turn_started", { sub_turn: 1 }),
      ev(3, "tool_call", { index: 0, id: "call_r", name: "Read", arguments: '{"file_path":"shot.png"}' }),
      ev(4, "turn_finished", { finish_reason: "tool_calls" }),
      ev(5, "tool_result", {
        tool_call_id: "call_r",
        name: "Read",
        content: "Image: shot.png",
        image_url: "data:image/png;base64,iVBORw0KGgo=",
      }),
    ];
    const blocks = foldEvents(events);
    const result = blocks.find((b) => b.type === "tool_result");
    expect(result && "image_url" in result ? result.image_url : undefined).toBe("data:image/png;base64,iVBORw0KGgo=");
  });

  it("leaves image_url absent on a tool_result block whose payload has none", () => {
    const blocks = foldEvents(sampleEvents());
    const result = blocks.find((b) => b.type === "tool_result");
    expect(result && "image_url" in result ? result.image_url : undefined).toBeUndefined();
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
    // The pending tool's start stamp is the turn_finished event's
    // created_at — the moment the turn froze and its calls began running —
    // which is what the watch page's footer ages the running tool from
    // ("1m 04s" under the tool name).
    expect(state.live.pendingTools.get("call_1")?.startedAt).toBe("2026-01-01T00:00:07Z");

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

  it("applies TaskCreate in call order, minting string ids from the fold's counter", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(3, "tool_call", {
        index: 0,
        id: "call_c1",
        name: "TaskCreate",
        arguments: JSON.stringify({ tasks: [{ subject: "a", description: "do a", activeForm: "doing a" }] }),
      }),
    );
    expect(state.latestTodos).toEqual([{ taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" }]);

    // A second call mints the next id in call order, and an explicit status
    // is kept rather than defaulted.
    state.ingest(
      ev(4, "tool_call", {
        index: 0,
        id: "call_c2",
        name: "TaskCreate",
        arguments: JSON.stringify({ tasks: [{ subject: "b", description: "do b", activeForm: "doing b", status: "in_progress" }] }),
      }),
    );
    expect(state.latestTodos).toEqual([
      { taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" },
      { taskId: "2", subject: "b", description: "do b", status: "in_progress", activeForm: "doing b" },
    ]);
  });

  it("applies TaskUpdate by taskId: patches only the fields present, and deletes on status deleted", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(2, "tool_call", {
        index: 0,
        id: "call_c",
        name: "TaskCreate",
        arguments: JSON.stringify({
          tasks: [
            { subject: "a", description: "do a", activeForm: "doing a" },
            { subject: "b", description: "do b", activeForm: "doing b" },
          ],
        }),
      }),
    );
    state.ingest(
      ev(3, "tool_call", {
        index: 0,
        id: "call_u",
        name: "TaskUpdate",
        arguments: JSON.stringify({ taskId: "2", status: "in_progress" }),
      }),
    );
    expect(state.latestTodos).toEqual([
      { taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" },
      { taskId: "2", subject: "b", description: "do b", status: "in_progress", activeForm: "doing b" },
    ]);

    // Patch subject and description only — the other fields stay.
    state.ingest(
      ev(4, "tool_call", {
        index: 0,
        id: "call_u2",
        name: "TaskUpdate",
        arguments: JSON.stringify({ taskId: "1", subject: "renamed", description: "renamed too" }),
      }),
    );
    expect(state.latestTodos[0]).toEqual({ taskId: "1", subject: "renamed", description: "renamed too", status: "pending", activeForm: "doing a" });

    // Delete the other one with status "deleted".
    state.ingest(
      ev(5, "tool_call", {
        index: 0,
        id: "call_d",
        name: "TaskUpdate",
        arguments: JSON.stringify({ taskId: "2", status: "deleted" }),
      }),
    );
    expect(state.latestTodos).toEqual([{ taskId: "1", subject: "renamed", description: "renamed too", status: "pending", activeForm: "doing a" }]);
  });

  it("leaves latestTodos unchanged when a TaskCreate call's arguments are not yet valid JSON", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(2, "tool_call", { index: 0, id: "call_t", name: "TaskCreate", arguments: '{"tasks":[{"subject":' }));
    expect(state.latestTodos).toEqual([]);
  });

  it("leaves latestTodos unchanged when a TaskUpdate call's arguments are not yet valid JSON", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(2, "tool_call", {
        index: 0,
        id: "call_c",
        name: "TaskCreate",
        arguments: JSON.stringify({ tasks: [{ subject: "a", description: "do a", activeForm: "doing a" }] }),
      }),
    );
    state.ingest(ev(3, "tool_call", { index: 0, id: "call_u", name: "TaskUpdate", arguments: '{"taskId":"1","status":' }));
    expect(state.latestTodos).toEqual([{ taskId: "1", subject: "a", description: "do a", status: "pending", activeForm: "doing a" }]);
  });

  it("leaves latestTodos unchanged when a plan mutation would have been rejected", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(2, "tool_call", {
        index: 0,
        id: "call_c",
        name: "TaskCreate",
        arguments: JSON.stringify({ tasks: [{ subject: "a", description: "do a", activeForm: "doing a" }] }),
      }),
    );
    const before = state.latestTodos;

    // Unknown id: no-op. "deleted" combined with a patch: no-op. Invalid
    // status on create: no-op, and the counter must not advance.
    state.ingest(ev(3, "tool_call", { index: 0, id: "u1", name: "TaskUpdate", arguments: JSON.stringify({ taskId: "99", status: "completed" }) }));
    state.ingest(ev(4, "tool_call", { index: 0, id: "u2", name: "TaskUpdate", arguments: JSON.stringify({ taskId: "1", status: "deleted", subject: "renamed" }) }));
    state.ingest(ev(5, "tool_call", { index: 0, id: "u3", name: "TaskUpdate", arguments: JSON.stringify({ taskId: "1", status: "bogus" }) }));
    state.ingest(ev(6, "tool_call", { index: 0, id: "c2", name: "TaskCreate", arguments: JSON.stringify({ tasks: [{ subject: "", description: "d", activeForm: "x" }] }) }));
    expect(state.latestTodos).toEqual(before);

    // A later valid create still mints the id one past the last minted one —
    // rejected events never advanced the counter.
    state.ingest(ev(7, "tool_call", { index: 0, id: "c3", name: "TaskCreate", arguments: JSON.stringify({ tasks: [{ subject: "b", description: "do b", activeForm: "doing b" }] }) }));
    expect(state.latestTodos.map((t) => t.taskId)).toEqual(["1", "2"]);
  });

  it("never lets TaskGet or TaskList mutate latestTodos", () => {
    const state = new FoldState();
    state.ingest(ev(1, "turn_started", { sub_turn: 1 }));
    state.ingest(
      ev(2, "tool_call", {
        index: 0,
        id: "call_c",
        name: "TaskCreate",
        arguments: JSON.stringify({ tasks: [{ subject: "a", description: "do a", activeForm: "doing a" }] }),
      }),
    );
    const before = state.latestTodos;
    state.ingest(ev(3, "tool_call", { index: 0, id: "call_g", name: "TaskGet", arguments: JSON.stringify({ taskId: "1" }) }));
    state.ingest(ev(4, "tool_call", { index: 0, id: "call_l", name: "TaskList", arguments: JSON.stringify({}) }));
    expect(state.latestTodos).toEqual(before);
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
      { type: "opening", seq: 1, text: "x", attachments: [] },
      { type: "steer", seq: 2, text: "be terse", state: "pending" },
    ]);

    state.ingest(ev(3, "steer_applied", { source_seq: 2, text: "be terse", sub_turn: 1 }));
    expect(state.blocks).toEqual([
      { type: "opening", seq: 1, text: "x", attachments: [] },
      { type: "steer", seq: 2, text: "be terse", state: "delivered", appliedSubTurn: 1 },
    ]);
  });

  it("matches by source_seq, so one steer_applied flips only its own steer", () => {
    const state = new FoldState();
    state.ingest(ev(1, "steer_message", { text: "first" }));
    state.ingest(ev(2, "steer_message", { text: "second" }));
    state.ingest(ev(3, "steer_applied", { source_seq: 2, text: "second", sub_turn: 1 }));
    expect(state.blocks).toEqual([
      { type: "steer", seq: 1, text: "first", state: "pending" },
      { type: "steer", seq: 2, text: "second", state: "delivered", appliedSubTurn: 1 },
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
    expect(steer).toEqual({ type: "steer", seq: 2, text: "be terse", state: "delivered", appliedSubTurn: 1 });
    expect(state.blocks.map((b) => b.type)).toEqual(["opening", "steer", "assistant"]);
  });

  it("stamps the producer's own sub_turn onto the delivered block, not a derived one", () => {
    // The boundary the message became a user message at is steer_applied's
    // sub_turn, stamped by the producer: a steer sent while a turn was
    // streaming lands in the turn that was already running, which counting
    // groups after the steer block cannot recover. The display reads the
    // block's appliedSubTurn instead of deriving it.
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingest(ev(3, "steer_message", { text: "be terse" }));
    state.ingest(ev(4, "turn_finished", { sub_turn: 1, finish_reason: "stop" }));
    state.ingest(ev(5, "steer_applied", { source_seq: 3, text: "be terse", sub_turn: 2 }));

    const steer = state.blocks.find((b) => b.type === "steer");
    expect(steer).toEqual({ type: "steer", seq: 3, text: "be terse", state: "delivered", appliedSubTurn: 2 });
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
    expect(full[1]).toEqual({ type: "steer", seq: 2, text: "be terse", state: "delivered", appliedSubTurn: 1 });
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

// The task's image attachments: the workspace paths they were materialised
// under (scratch/attachments/<name>), carried separately by session_started
// so the opening block renders them through the screenshot endpoint without
// parsing the message text.
describe("task attachments", () => {
  it("reaches the opening block, exactly as the payload carried them", () => {
    const blocks = foldEvents([
      ev(1, "session_started", {
        opening_message: "Workspace: /ws\n\nImage files attached to this task, materialised into scratch/attachments/:\n- scratch/attachments/mockup.png\n- scratch/attachments/light.webp\n\nTask:\nmatch the mockup\n",
        attachments: ["scratch/attachments/mockup.png", "scratch/attachments/light.webp"],
      }),
    ]);
    expect(blocks.map((b) => b.type)).toEqual(["opening"]);
    const openingBlock = blocks[0];
    expect(openingBlock && "attachments" in openingBlock ? openingBlock.attachments : undefined).toEqual([
      "scratch/attachments/mockup.png",
      "scratch/attachments/light.webp",
    ]);
  });

  it("defaults to an empty list when the payload carries none (older sessions)", () => {
    const blocks = foldEvents([ev(1, "session_started", { opening_message: "Workspace: /ws\n\nTask:\ndo the thing\n" })]);
    const openingBlock = blocks[0];
    expect(openingBlock && "attachments" in openingBlock ? openingBlock.attachments : undefined).toEqual([]);
  });
});

// The launching agent's instruction (.msg-user): the task tail of the
// opening message, carried separately by
// session_started so the watch page renders it as its own message without
// parsing the message text.
describe("launching instruction block", () => {
  it("pushes an instruction block after the opening when the run was created with a task", () => {
    const blocks = foldEvents([
      ev(1, "session_started", {
        opening_message: "Workspace: /ws\n\nTask:\nFlip Stateless to false\n",
        task: "Flip Stateless to false",
      }),
    ]);
    expect(blocks.map((b) => b.type)).toEqual(["opening", "instruction"]);
    const instruction = blocks.find((b) => b.type === "instruction");
    expect(instruction).toMatchObject({ seq: 1, text: "Flip Stateless to false" });
  });

  it("pushes no instruction block when the run was created without a task", () => {
    const blocks = foldEvents([
      ev(1, "session_started", { opening_message: "Workspace: /ws\n\nTask:\n" }),
    ]);
    expect(blocks.map((b) => b.type)).toEqual(["opening"]);
  });

  it("sits after the skills catalogue when both exist", () => {
    const blocks = foldEvents([
      ev(1, "session_started", {
        opening_message: "Workspace: /ws\n\nskills here\nTask:\ndo it\n",
        skill_catalogue: "skills here",
        task: "do it",
      }),
    ]);
    expect(blocks.map((b) => b.type)).toEqual(["skills", "opening", "instruction"]);
  });
});

// Live deltas are model output the backend has not committed yet
// (hub.LiveDelta): they stream ahead of the sub-turn's commit so a watching
// operator sees the text being written, and the committed
// reasoning_delta/content_delta events carry the same text moments later.
// Keeping the two in separate fields is what stops that from doubling — the
// hazard that decides this whole design.
describe("live deltas", () => {
  it("accumulates into the live pair, leaving the committed pair alone", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));

    state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "weigh" });
    state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "ing it" });
    state.ingestLive({ sub_turn: 1, channel: "content", text: "the answer" });

    expect(state.live.turn?.liveReasoning).toBe("weighing it");
    expect(state.live.turn?.liveContent).toBe("the answer");
    expect(state.live.turn?.reasoning).toBe("");
    expect(state.live.turn?.content).toBe("");
  });

  // The streaming reveal (components/ui/StreamText.tsx) animates one span per
  // entry here, and a CSS animation only runs when its element mounts. So the
  // property that matters is not what the array contains but that entries are
  // only ever appended: rewrite one and it replays its animation on the next
  // flush, sixty times a second on the text a reader is looking at.
  it("keeps content frames as an append-only array, never rewriting an entry", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));

    state.ingestLive({ sub_turn: 1, channel: "content", text: "the " });
    const afterFirst = [...state.live.turn!.liveContentChunks];
    state.ingestLive({ sub_turn: 1, channel: "content", text: "answer" });

    expect(state.live.turn?.liveContentChunks).toEqual(["the ", "answer"]);
    // The prefix the first frame produced is untouched by the second.
    expect(state.live.turn?.liveContentChunks.slice(0, 1)).toEqual(afterFirst);
    // And it stays the same text as the accumulated string it mirrors.
    expect(state.live.turn?.liveContentChunks.join("")).toBe(state.live.turn?.liveContent);
  });

  it("keeps reasoning frames out of the content chunks, and drops empty frames", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));

    state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "thinking" });
    state.ingestLive({ sub_turn: 1, channel: "content", text: "" });
    state.ingestLive({ sub_turn: 1, channel: "content", text: "said" });

    // An empty frame would mount a span that reveals nothing.
    expect(state.live.turn?.liveContentChunks).toEqual(["said"]);
  });

  it("starts each sub-turn's chunks empty, so a new turn never replays the last one's", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingestLive({ sub_turn: 1, channel: "content", text: "first turn" });
    state.ingest(ev(3, "tool_call", { index: 0, id: "c1", name: "Bash", arguments: "{}" }));
    state.ingest(ev(4, "turn_finished", { finish_reason: "tool_calls" }));

    state.ingest(ev(5, "turn_started", { sub_turn: 2 }));

    expect(state.live.turn?.liveContentChunks).toEqual([]);
  });

  it("freezes the block from the committed text, not the streamed preview", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 1 }));
    state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "weighing it" });
    state.ingestLive({ sub_turn: 1, channel: "content", text: "the answer" });
    // The same text arrives again, now as committed events.
    state.ingest(ev(3, "reasoning_delta", { text: "weighing it" }));
    state.ingest(ev(4, "content_delta", { text: "the answer" }));
    state.ingest(ev(5, "turn_finished", { sub_turn: 1, finish_reason: "stop" }));

    const assistant = state.blocks.find((b) => b.type === "assistant");
    expect(assistant && "reasoning" in assistant ? assistant.reasoning : "").toBe("weighing it");
    expect(assistant && "content" in assistant ? assistant.content : "").toBe("the answer");
  });

  it("drops a delta for a sub-turn that is not the one in flight", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    state.ingest(ev(2, "turn_started", { sub_turn: 2 }));
    state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "from the frozen turn" });
    expect(state.live.turn?.liveReasoning).toBe("");
  });

  it("drops a delta that arrives before any turn is live", () => {
    const state = new FoldState();
    state.ingest(ev(1, "session_started", { opening_message: "x" }));
    expect(() => state.ingestLive({ sub_turn: 1, channel: "reasoning", text: "early" })).not.toThrow();
    expect(state.live.turn).toBeNull();
  });
});
