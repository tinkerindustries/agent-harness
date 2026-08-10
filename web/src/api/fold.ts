import type {
  ContentDeltaPayload,
  ErrorPayload,
  ReasoningDeltaPayload,
  RunFinishedPayload,
  SessionStartedPayload,
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
  | ({ type: "error"; seq: number } & ErrorPayload);

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
}

export interface LiveView {
  turn: LiveTurn | null;
  pendingTools: Map<string, PendingTool>;
}

function parseTodos(argumentsJSON: string): Todo[] | null {
  try {
    const parsed = JSON.parse(argumentsJSON) as { todos?: unknown };
    if (Array.isArray(parsed.todos)) return parsed.todos as Todo[];
  } catch {
    // Arguments can still be mid-assembly when this fires; the panel just
    // keeps showing the last plan it successfully parsed.
  }
  return null;
}

export class FoldState {
  blocks: Block[] = [];
  live: LiveView = { turn: null, pendingTools: new Map() };
  latestTodos: Todo[] = [];

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
        if (p.name === "TodoWrite") {
          const todos = parseTodos(p.arguments);
          if (todos) this.latestTodos = todos;
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
            this.live.pendingTools.set(call.id, { call, stdout: "" });
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
