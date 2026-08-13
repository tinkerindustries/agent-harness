import type { StoreEvent } from "../api/types";

// syntheticFeed builds event sequences that exercise the same wire shape a
// live session produces (docs/DESIGN.md §5.1's "two text channels arrive as
// deltas at token rate"), for PerfHarnessScreen to feed into a real
// TranscriptStore. Nothing here talks to the network; it exists so the
// virtualisation measurement can be re-run without a live DeepSeek call
// (web/CLAUDE.md: re-measure rather than arguing from first principles).

export interface SeqSource {
  next(): number;
}

export function makeSeqSource(): SeqSource {
  let n = 0;
  return { next: () => ++n };
}

const LOREM =
  "the harness folds the event log into blocks reasoning about the task before making a tool call to inspect the workspace and then editing the relevant file to fix the bug and running the test suite to confirm the change holds".split(
    " ",
  );

function words(n: number): string {
  const out: string[] = [];
  for (let i = 0; i < n; i++) out.push(LOREM[i % LOREM.length]);
  return out.join(" ") + " ";
}

function nowISO(): string {
  return new Date().toISOString();
}

const BASH_OUTPUT = Array.from({ length: 60 }, (_, i) => `line ${i}: build step ${i} ok`).join("\n");
const READ_OUTPUT = Array.from({ length: 220 }, (_, i) => `${String(i + 1).padStart(6)}\tconst x${i} = ${i};`).join("\n");

type TurnKind = "plain" | "bash" | "edit" | "read";
const TURN_KINDS: TurnKind[] = ["plain", "bash", "edit", "read"];

// The synthetic session's plan, for the TaskCreate/TaskUpdate calls that
// phase the history (docs/WEB-REDESIGN.md phase 6 needs plan boundaries to
// group the rail's sub-turns under). Seven items, the shape of the
// measured session's plan.
const PLAN_ITEMS = [
  "Fix retained-body leak",
  "Add httplog test",
  "Config: HTTPLogRoot",
  "Session: Recorder lifecycle",
  "docs/DESIGN.md §4.8",
  "Tests: config and session",
  "Verification and PR",
];

// planTodos is the plan as of a phase boundary: everything up to `boundary`
// completed, the boundary item in_progress, the rest pending. Only boundary 0
// is materialized by a TaskCreate (below); later boundaries advance the same
// items by taskId with TaskUpdate pairs, so the ids TaskCreate mints (1..7,
// in call order) are exactly the ids those updates name. The payload speaks
// the fold's vocabulary — subject/description/activeForm — because
// applyTaskEvent validates those fields and a payload that does not name
// them would be silently dropped, leaving the plan (and the plan rail) empty.
function planTodos(boundary: number): unknown {
  return PLAN_ITEMS.map((content, i) => ({
    subject: content,
    description: content,
    status: i < boundary ? "completed" : i === boundary ? "in_progress" : "pending",
    activeForm: i === boundary ? `working on ${content}` : content,
  }));
}

// buildSyntheticHistory generates events for roughly targetBlocks frozen
// blocks, cycling through plain answers and Bash/Edit/Read tool calls so the
// mix looks like a real session: sizable Bash output, a real diff, and a
// large enough Read to trigger the collapse threshold. A TaskCreate call
// opens sub-turn 1 (minting the seven plan ids) and TaskUpdate pairs advance
// the plan every twentieth sub-turn after it, so the fold's latestTodos — and
// therefore the timeline rail's phase groups — have real plan boundaries to
// work with. This is the "few hundred blocks mounted" half of the
// measurement. seq is shared with whatever live feed follows it
// (PerfHarnessScreen passes the same source to both) so seq numbers — which
// double as React keys — stay unique across the whole synthetic session
// instead of each phase restarting its own count.
export function buildSyntheticHistory(targetBlocks: number, sessionId: string, seq: SeqSource): StoreEvent[] {
  const events: StoreEvent[] = [];
  const push = (kind: StoreEvent["kind"], payload: unknown) => {
    events.push({ session_id: sessionId, seq: seq.next(), kind, payload, created_at: nowISO() });
  };

  push("session_started", { opening_message: "Synthetic perf-harness session: " + words(40) });
  let blockCount = 1;
  let turn = 0;

  while (blockCount < targetBlocks) {
    turn++;
    const kind = TURN_KINDS[turn % TURN_KINDS.length];
    push("turn_started", { sub_turn: turn });
    push("reasoning_delta", { text: words(30) });
    if (kind === "plain") push("content_delta", { text: words(20) });

    let callId: string | null = null;
    let callIndex = 0;
    if (turn === 1 || turn % 20 === 1) {
      // Every plan-mutating call in the event stream marks a rail phase
      // boundary (docs/WEB-REDESIGN.md phase 6). The first marks the plan
      // itself: TaskCreate mints ids 1..7 in call order. Later boundaries
      // advance the plan with TaskUpdate pairs — the item that was
      // in_progress completes and the next one starts — so both mutating
      // tools exercise the boundary logic.
      const boundary = Math.floor((turn - 1) / 20);
      if (boundary === 0) {
        push("tool_call", {
          index: callIndex++,
          id: `plan_${turn}`,
          name: "TaskCreate",
          arguments: JSON.stringify({ tasks: planTodos(0) }),
        });
      } else {
        push("tool_call", {
          index: callIndex++,
          id: `plan_${turn}_a`,
          name: "TaskUpdate",
          arguments: JSON.stringify({ taskId: String(boundary), status: "completed" }),
        });
        if (boundary < PLAN_ITEMS.length) {
          push("tool_call", {
            index: callIndex++,
            id: `plan_${turn}_b`,
            name: "TaskUpdate",
            arguments: JSON.stringify({ taskId: String(boundary + 1), status: "in_progress" }),
          });
        }
      }
    }
    if (kind === "bash") {
      callId = `call_${turn}`;
      push("tool_call", { index: callIndex, id: callId, name: "Bash", arguments: JSON.stringify({ command: "npm test" }) });
    } else if (kind === "edit") {
      callId = `call_${turn}`;
      push("tool_call", {
        index: callIndex,
        id: callId,
        name: "Edit",
        arguments: JSON.stringify({ file_path: "src/app.ts", old_string: "const a = 1", new_string: "const a = 2" }),
      });
    } else if (kind === "read") {
      callId = `call_${turn}`;
      push("tool_call", { index: callIndex, id: callId, name: "Read", arguments: JSON.stringify({ file_path: "src/big.ts" }) });
    }
    push("turn_finished", { finish_reason: callId ? "tool_calls" : "stop", elapsed_ms: 9900 });
    blockCount++; // the assistant block

    if (callId) {
      if (kind === "bash") {
        push("tool_result", { tool_call_id: callId, name: "Bash", content: BASH_OUTPUT });
      } else if (kind === "edit") {
        push("tool_result", {
          tool_call_id: callId,
          name: "Edit",
          content: "Edited src/app.ts (1 replacement(s))",
          diff: [
            { kind: "remove", text: "const a = 1", old_line: 1 },
            { kind: "add", text: "const a = 2", new_line: 1 },
          ],
        });
      } else if (kind === "read") {
        push("tool_result", { tool_call_id: callId, name: "Read", content: READ_OUTPUT });
      }
      blockCount++; // the tool_result block
    }

    push("usage", {
      sub_turn: turn,
      prompt_tokens: 1000 + turn * 50,
      prompt_cache_hit_tokens: 900 + turn * 45,
      prompt_cache_miss_tokens: 100 + turn * 5,
      completion_tokens: 40,
      reasoning_tokens: 30,
      cost_usd: 0.0001,
      expected_miss_tokens: 100 + turn * 5,
    });
    blockCount++; // the usage block
  }

  return events;
}

// liveEventGenerator yields one event per call, looping forever through a
// realistic sub-turn: reasoning and content deltas, a Bash call, live
// tool_stdout chunks, its result, then usage — the same event kinds and
// ordering a real session commits, just paced by the caller instead of
// arriving from the network. PerfHarnessScreen drives this at a configured
// rate to simulate token-rate delivery.
export function liveEventGenerator(sessionId: string, seq: SeqSource): Generator<StoreEvent, never, void> {
  function* gen(): Generator<StoreEvent, never, void> {
    let turn = 1_000_000;
    for (;;) {
      const push = (kind: StoreEvent["kind"], payload: unknown): StoreEvent => ({
        session_id: sessionId,
        seq: seq.next(),
        kind,
        payload,
        created_at: nowISO(),
      });

      yield push("turn_started", { sub_turn: turn });
      for (let i = 0; i < 20; i++) yield push("reasoning_delta", { text: words(3) });
      for (let i = 0; i < 8; i++) yield push("content_delta", { text: words(3) });

      // Every live sub-turn mutates the plan, so the rail's phase grouping is
      // exercised on the live append path too — each live turn opens its own
      // phase in the rail. TaskCreate appends the next batch (fresh ids the
      // fold mints in call order), and a TaskUpdate re-marks the session's
      // first plan item, so both mutating tools run on the live path; the
      // update names a history item, never a minted id, so the generator
      // needs no knowledge of the fold's counter.
      yield push("tool_call", {
        index: 0,
        id: `live_plan_${turn}`,
        name: "TaskCreate",
        arguments: JSON.stringify({
          tasks: [
            { subject: `live item ${turn % 3}`, description: "live item detail", status: "in_progress", activeForm: "working" },
            { subject: "next", description: "next detail", status: "pending", activeForm: "next" },
          ],
        }),
      });
      yield push("tool_call", {
        index: 1,
        id: `live_update_${turn}`,
        name: "TaskUpdate",
        arguments: JSON.stringify({ taskId: "1", status: "in_progress" }),
      });
      const callId = `live_call_${turn}`;
      yield push("tool_call", { index: 0, id: callId, name: "Bash", arguments: JSON.stringify({ command: "npm run build" }) });
      yield push("turn_finished", { finish_reason: "tool_calls", elapsed_ms: 4200 });
      for (let i = 0; i < 15; i++) yield push("tool_stdout", { tool_call_id: callId, text: `build output line ${i}\n` });
      yield push("tool_result", { tool_call_id: callId, name: "Bash", content: BASH_OUTPUT });
      yield push("usage", {
        sub_turn: turn,
        prompt_tokens: 1000,
        prompt_cache_hit_tokens: 900,
        prompt_cache_miss_tokens: 100,
        completion_tokens: 40,
        reasoning_tokens: 30,
        cost_usd: 0.0001,
        expected_miss_tokens: 100,
      });
      turn++;
    }
  }
  return gen();
}
