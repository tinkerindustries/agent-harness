import type {
  ContentDeltaPayload,
  ErrorPayload,
  ReasoningDeltaPayload,
  RunFinishedPayload,
  SessionStartedPayload,
  SteerAppliedPayload,
  SteerMessagePayload,
  StoreEvent,
  Todo,
  ToolCallPayload,
  ToolDeniedPayload,
  ToolResultPayload,
  ToolStdoutPayload,
  TurnFinishedPayload,
  TurnStartedPayload,
  UsagePayload,
} from "./types";

// The event-to-view-model fold, mirroring internal/fold.Fold in shape
// (docs/DESIGN.md §4.1): a pure reducer over the event log, one event at a
// time. It does not re-derive the whole block
// list on every call — FoldState.ingest folds a single event into either a
// completed Block (pushed once, never touched again) or the in-progress
// live view (docs/DESIGN.md §5.2). Folding the same event log through
// ingest() one event at a time or all at once (see foldEvents below) always
// produces the same blocks in the same order — the append-only property
// internal/fold's Go counterpart guarantees, and for the same reason: a
// sub-turn or a tool call contributes nothing to `blocks` until the event
// that completes it has been seen.

export type Block =
  | { type: "opening"; seq: number; text: string }
  | { type: "skills"; seq: number; text: string }
  // The launching agent's own instruction (design/session-watch.html's
  // .msg-user "from claude-code · delivered · sub-turn 1"): the task tail of
  // the opening message, carried separately by session_started so the watch
  // page can render it as its own message attributed to the launcher without
  // parsing the message text. Absent for a run created without a task (a
  // browser start waits for its first message) and for a resume
  // continuation.
  | { type: "instruction"; seq: number; text: string }
  | {
      type: "assistant";
      seq: number;
      subTurn: number;
      reasoning: string;
      content: string;
      toolCalls: ToolCallPayload[];
      finishReason: string;
      reasoningElapsedMs?: number;
      reasoningTokens?: number;
    }
  | ({ type: "tool_result"; seq: number; call?: ToolCallPayload } & ToolResultPayload)
  | ({ type: "tool_denied"; seq: number; call?: ToolCallPayload } & ToolDeniedPayload)
  | ({ type: "usage"; seq: number } & UsagePayload)
  | ({ type: "run_finished"; seq: number } & RunFinishedPayload)
  | ({ type: "error"; seq: number } & ErrorPayload)
  // A steer: an operator instruction appended to a running session
  // (docs/RUN-CONTROL.md "Steering"). Emitted in the *pending* state by
  // steer_message and flipped to *delivered* by the matching steer_applied
  // (matched by source_seq) — the one place this fold completes a block it
  // has already emitted, which the Go fold must never do but display blocks
  // can: there is no prompt cache here, and a steer that sits pending for
  // minutes is exactly the operator's signal that the run is wedged.
  //
  // appliedSubTurn is the boundary the message became a user message at —
  // steer_applied's own sub_turn, stamped by the producer — set when the
  // block flips to delivered. Absent on a block folded before the field
  // existed, or still pending; the chat page's message then renders without
  // naming a sub-turn rather than guessing.
  | { type: "steer"; seq: number; text: string; state: "pending" | "delivered"; appliedSubTurn?: number };

// LiveTurn is the sub-turn currently streaming: reasoning and content grow
// by concatenation as reasoning_delta/content_delta events arrive, and the
// whole thing is discarded (folded into a frozen Block) the moment
// turn_finished lands.
export interface LiveTurn {
  seq: number;
  subTurn: number;
  reasoning: string;
  content: string;
  toolCalls: ToolCallPayload[];
  startedAt: string;
}

// PendingTool is a tool call whose turn has frozen but whose result hasn't
// arrived yet. stdout accumulates only for Bash calls that stream; every
// other tool just sits here as a "running" placeholder until its
// tool_result or tool_denied event removes it.
export interface PendingTool {
  call: ToolCallPayload;
  stdout: string;
  // startedAt is when the call began running: the created_at of the
  // turn_finished event that moved the call from the streaming turn into
  // the pending set — the closest the log gets to the moment the tool
  // started executing (the calls run right after the turn froze). The
  // watch page's footer counts the running tool's own age from it
  // (design/session-watch.html's "1m 04s" under the tool name); the
  // tool_call event's created_at cannot express that, because a whole
  // batch of events shares one instant and the calls run after it.
  startedAt: string;
}

export interface LiveView {
  turn: LiveTurn | null;
  pendingTools: Map<string, PendingTool>;
}

// applyTaskEvent patches a plan with one TaskCreate or TaskUpdate tool-call
// event, mirroring internal/store/status.go's applyTaskEvent (the same
// replay the store runs over the event log to recover RequestStatus.Todos):
// TaskCreate mints ids from nextId (increment then use, decimal string) and
// appends, with status defaulting to "pending"; TaskUpdate finds by taskId
// and patches only the fields present, or removes the task when status is
// "deleted". Events the live handler would have rejected — malformed JSON (an
// arguments payload can arrive mid-assembly while streaming), an empty
// subject, description, or activeForm, an invalid status, a "deleted"
// combined with a patch, an update with nothing to set, an unknown id — leave
// the plan unchanged. This folds a live event stream and must never throw, so
// nothing here raises.
export function applyTaskEvent(
  todos: Todo[],
  nextId: number,
  name: string,
  argumentsJSON: string,
): { todos: Todo[]; nextId: number } {
  let args: unknown;
  try {
    args = JSON.parse(argumentsJSON);
  } catch {
    return { todos, nextId };
  }
  // A JSON literal (null, a number, a string) is never a valid task payload;
  // the property reads below would throw on null, so reject it up front.
  if (typeof args !== "object" || args === null) return { todos, nextId };
  switch (name) {
    case "TaskCreate": {
      const tasks = (args as { tasks?: unknown }).tasks;
      if (!Array.isArray(tasks) || tasks.length === 0) return { todos, nextId };
      const items = tasks as Partial<Todo>[];
      for (const t of items) {
        if (!t || typeof t !== "object" || !t.subject?.trim() || !t.description?.trim() || !t.activeForm?.trim()) return { todos, nextId };
        if (t.status && !isValidTaskStatus(t.status)) return { todos, nextId };
      }
      let n = nextId;
      const out = [...todos];
      for (const t of items) {
        n++;
        out.push({
          taskId: String(n),
          subject: t.subject as string,
          description: t.description as string,
          status: (t.status ?? "pending") as Todo["status"],
          activeForm: t.activeForm as string,
        });
      }
      return { todos: out, nextId: n };
    }
    case "TaskUpdate": {
      const a = args as { taskId?: unknown; status?: unknown; subject?: unknown; description?: unknown; activeForm?: unknown };
      if (typeof a.taskId !== "string" || a.taskId === "") return { todos, nextId };
      const status = typeof a.status === "string" ? a.status : "";
      const subject = typeof a.subject === "string" ? a.subject : "";
      const description = typeof a.description === "string" ? a.description : "";
      const activeForm = typeof a.activeForm === "string" ? a.activeForm : "";
      if (status === "deleted" && (subject !== "" || description !== "" || activeForm !== "")) return { todos, nextId };
      if (status === "" && subject === "" && description === "" && activeForm === "") return { todos, nextId };
      if (status !== "" && status !== "deleted" && !isValidTaskStatus(status)) return { todos, nextId };
      const idx = todos.findIndex((t) => t.taskId === a.taskId);
      if (idx < 0) return { todos, nextId };
      if (status === "deleted") return { todos: todos.filter((_, i) => i !== idx), nextId };
      const out = [...todos];
      if (status !== "") out[idx] = { ...out[idx], status: status as Todo["status"] };
      if (subject !== "") out[idx] = { ...out[idx], subject };
      if (description !== "") out[idx] = { ...out[idx], description };
      if (activeForm !== "") out[idx] = { ...out[idx], activeForm };
      return { todos: out, nextId };
    }
    default:
      return { todos, nextId };
  }
}

// isValidTaskStatus mirrors internal/tools' status enum, the same check
// internal/store/status.go's validTaskStatus applies in its replay.
function isValidTaskStatus(status: string): boolean {
  return status === "pending" || status === "in_progress" || status === "completed";
}

export class FoldState {
  blocks: Block[] = [];
  live: LiveView = { turn: null, pendingTools: new Map() };
  latestTodos: Todo[] = [];
  // nextTaskId is the counter TaskCreate mints ids from, in call order,
  // mirroring the executor's nextTaskID so the fold's plan matches the
  // backend's (internal/store/status.go applyTaskEvent replays the same
  // counter over the event log).
  private nextTaskId = 0;

  // toolCallsById is kept for the session's whole life, not cleared on
  // result, so a frozen tool_result block can still be shaped by the
  // arguments (file_path, command, pattern, ...) that produced it.
  private toolCallsById = new Map<string, ToolCallPayload>();

  // getToolCall is the deliberate read path into the registry for the
  // display layer (docs/WEB-REDESIGN.md phase 5): the sub-turn card builds
  // its tool headers from the call the fold keeps here rather than the
  // display layer re-parsing the arguments string or keeping its own copy.
  // Reading through this method is all the exposure the registry needs —
  // nothing outside the fold ever mutates it.
  getToolCall(id: string): ToolCallPayload | undefined {
    return this.toolCallsById.get(id);
  }

  // pushBlock replaces `blocks` with a new array rather than mutating in
  // place. That copy is the whole reason components/BlockList.tsx can wrap
  // the frozen list in a single React.memo keyed on the blocks array
  // reference: a live-only update (a delta, a tool_stdout chunk) never
  // touches this method, so the array reference — and therefore every
  // existing Block object inside it — is unchanged, and React skips
  // re-rendering and re-reconciling all of them. Skipping that per-block
  // React.memo bailout is not enough on its own; measurement showed commit
  // cost still scaling with block count when the array reference changed
  // (or was un-memoised) on every flush, because mapping and reconciling N
  // list children costs O(n) even when each child bails out immediately.
  private pushBlock(block: Block) {
    this.blocks = [...this.blocks, block];
  }

  // attachReasoningTokens amends the sub-turn's frozen assistant block with
  // the API's own reasoning_tokens count, once per sub-turn. A starved
  // retry's first usage arrives before its turn_finished and is skipped here;
  // the retry's own usage, which follows turn_finished, attaches instead.
  private attachReasoningTokens(p: UsagePayload): void {
    if (this.live.turn && this.live.turn.subTurn === p.sub_turn) return;
    for (let i = this.blocks.length - 1; i >= 0; i--) {
      const b = this.blocks[i];
      if (b.type === "assistant" && b.subTurn === p.sub_turn && b.reasoningTokens === undefined) {
        this.blocks = [...this.blocks];
        this.blocks[i] = { ...b, reasoningTokens: p.reasoning_tokens };
        return;
      }
    }
  }

  ingest(ev: StoreEvent): void {
    switch (ev.kind) {
      case "session_started": {
        const p = ev.payload as SessionStartedPayload;
        // The catalogue is a substring of the opening message, so showing
        // both verbatim would print it twice. It gets its own block and the
        // opening block keeps the rest. Removing it by exact substring
        // rather than by pattern is why the payload carries it separately.
        const catalogue = p.skill_catalogue ?? "";
        const hasCatalogue = catalogue !== "" && p.opening_message.includes(catalogue);
        if (hasCatalogue) {
          this.pushBlock({ type: "skills", seq: ev.seq, text: catalogue });
        }
        // The renderer writes a newline after the catalogue; take it too, so
        // lifting the block out does not leave a gap behind.
        const text = hasCatalogue
          ? p.opening_message.replace(catalogue + "\n", "").replace(catalogue, "")
          : p.opening_message;
        this.pushBlock({ type: "opening", seq: ev.seq, text });
        // The launching agent's instruction, when the run was created with
        // one: its own block after the opening message, exactly where the
        // design draws it (design/session-watch.html) — the launcher's words
        // rendered as a message, not a summary line inside the opening card.
        if (p.task) this.pushBlock({ type: "instruction", seq: ev.seq, text: p.task });
        break;
      }
      case "turn_started": {
        const p = ev.payload as TurnStartedPayload;
        this.live.turn = { seq: ev.seq, subTurn: p.sub_turn, reasoning: "", content: "", toolCalls: [], startedAt: ev.created_at };
        break;
      }
      case "reasoning_delta": {
        const p = ev.payload as ReasoningDeltaPayload;
        if (this.live.turn) this.live.turn.reasoning += p.text;
        break;
      }
      case "content_delta": {
        const p = ev.payload as ContentDeltaPayload;
        if (this.live.turn) this.live.turn.content += p.text;
        break;
      }
      case "tool_call": {
        const p = ev.payload as ToolCallPayload;
        if (this.live.turn) this.live.turn.toolCalls.push(p);
        this.toolCallsById.set(p.id, p);
        // The plan tools' calls are the event-stream source of latestTodos
        // (docs/DESIGN.md §5.8): TaskCreate and TaskUpdate mutate the plan,
        // patched in call order exactly as the backend replay does; the
        // TaskGet/TaskList reads never touch it.
        if (p.name === "TaskCreate" || p.name === "TaskUpdate") {
          const next = applyTaskEvent(this.latestTodos, this.nextTaskId, p.name, p.arguments);
          this.latestTodos = next.todos;
          this.nextTaskId = next.nextId;
        }
        break;
      }
      case "turn_finished": {
        const p = ev.payload as TurnFinishedPayload;
        const turn = this.live.turn;
        if (turn) {
          this.pushBlock({
            type: "assistant",
            seq: turn.seq,
            subTurn: turn.subTurn,
            reasoning: turn.reasoning,
            content: turn.content,
            toolCalls: turn.toolCalls,
            finishReason: p.finish_reason,
            // The figure is measured server-side around the request and
            // carried in the payload; every event of a batch shares one
            // created_at, which cannot express it. Absent on old sessions.
            reasoningElapsedMs: p.elapsed_ms,
          });
          for (const call of turn.toolCalls) {
            this.live.pendingTools.set(call.id, { call, stdout: "", startedAt: ev.created_at });
          }
          this.live.turn = null;
        }
        break;
      }
      case "tool_stdout": {
        const p = ev.payload as ToolStdoutPayload;
        const pending = this.live.pendingTools.get(p.tool_call_id);
        if (pending) pending.stdout += p.text;
        break;
      }
      case "tool_result": {
        const p = ev.payload as ToolResultPayload;
        this.live.pendingTools.delete(p.tool_call_id);
        this.pushBlock({ type: "tool_result", seq: ev.seq, call: this.toolCallsById.get(p.tool_call_id), ...p });
        break;
      }
      case "tool_denied": {
        const p = ev.payload as ToolDeniedPayload;
        this.live.pendingTools.delete(p.tool_call_id);
        this.pushBlock({ type: "tool_denied", seq: ev.seq, call: this.toolCallsById.get(p.tool_call_id), ...p });
        break;
      }
      case "usage": {
        const p = ev.payload as UsagePayload;
        this.pushBlock({ type: "usage", seq: ev.seq, ...p });
        this.attachReasoningTokens(p);
        break;
      }
      case "run_finished": {
        const p = ev.payload as RunFinishedPayload;
        this.pushBlock({ type: "run_finished", seq: ev.seq, ...p });
        break;
      }
      case "error": {
        const p = ev.payload as ErrorPayload;
        this.pushBlock({ type: "error", seq: ev.seq, ...p });
        break;
      }
      case "steer_message": {
        const p = ev.payload as SteerMessagePayload;
        this.pushBlock({ type: "steer", seq: ev.seq, text: p.text, state: "pending" });
        break;
      }
      case "steer_applied": {
        // The one place this fold completes a block it has already emitted
        // (see the steer variant's comment): the Go fold's append-only rule
        // protects the prompt cache, which display blocks have no stake in,
        // and the two-state rendering is the whole reason the log carries two
        // kinds — a steer that never flips to delivered is the operator's
        // signal that the run is wedged. The block keeps its position (where
        // the operator sent it) and only its state changes — and, with it,
        // the producer-stamped sub-turn the message landed in, carried on the
        // block so the display never has to derive it from transcript order.
        const p = ev.payload as SteerAppliedPayload;
        for (let i = this.blocks.length - 1; i >= 0; i--) {
          const b = this.blocks[i];
          if (b.type === "steer" && b.seq === p.source_seq && b.state === "pending") {
            this.blocks = [...this.blocks];
            this.blocks[i] = { ...b, state: "delivered", appliedSubTurn: p.sub_turn };
            break;
          }
        }
        break;
      }
    }
  }
}

// foldEvents folds a whole event log at once, for callers that have no need
// for incremental updates — tests, and rebuilding a one-off view. Live
// sessions use FoldState.ingest directly (see api/transcriptStore.ts) so a
// burst of events costs at most one re-render, not one fold per event.
export function foldEvents(events: StoreEvent[]): Block[] {
  const state = new FoldState();
  for (const ev of events) state.ingest(ev);
  return state.blocks;
}
