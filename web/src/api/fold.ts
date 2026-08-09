import type {
  ContentDeltaPayload,
  ErrorPayload,
  ReasoningDeltaPayload,
  RunFinishedPayload,
  SessionStartedPayload,
  StoreEvent,
  ToolCallPayload,
  ToolDeniedPayload,
  ToolResultPayload,
  TurnFinishedPayload,
  TurnStartedPayload,
  UsagePayload,
} from "./types";

// The event-to-view-model fold, mirroring internal/fold.Fold in shape
// (docs/DESIGN.md §4.1, §5.2): a pure function of the event log, called
// fresh over the whole log on every update rather than mutated in place.
// Phase 5 is what makes that cheap at scale (frozen, memoised blocks); this
// is deliberately the simple version.

export type Block =
  | { type: "opening"; seq: number; text: string }
  | {
      type: "assistant";
      seq: number;
      subTurn: number;
      reasoning: string;
      content: string;
      toolCalls: ToolCallPayload[];
      finishReason: string;
    }
  | ({ type: "tool_result"; seq: number } & ToolResultPayload)
  | ({ type: "tool_denied"; seq: number } & ToolDeniedPayload)
  | ({ type: "usage"; seq: number } & UsagePayload)
  | ({ type: "run_finished"; seq: number } & RunFinishedPayload)
  | ({ type: "error"; seq: number } & ErrorPayload);

interface InProgressTurn {
  seq: number;
  subTurn: number;
  reasoning: string;
  content: string;
  toolCalls: ToolCallPayload[];
}

// foldEvents turns a session's ordered event log into blocks. Folding
// events.slice(0, n) and events.slice(0, n+1) never disagrees on a block
// both include — the same append-only property internal/fold's Go
// counterpart guarantees, and for the same reason: a sub-turn contributes
// nothing until its turn_finished event has been seen.
export function foldEvents(events: StoreEvent[]): Block[] {
  const blocks: Block[] = [];
  let turn: InProgressTurn | null = null;

  for (const ev of events) {
    switch (ev.kind) {
      case "session_started": {
        const p = ev.payload as SessionStartedPayload;
        blocks.push({ type: "opening", seq: ev.seq, text: p.opening_message });
        break;
      }
      case "turn_started": {
        const p = ev.payload as TurnStartedPayload;
        turn = { seq: ev.seq, subTurn: p.sub_turn, reasoning: "", content: "", toolCalls: [] };
        break;
      }
      case "reasoning_delta": {
        const p = ev.payload as ReasoningDeltaPayload;
        if (turn) turn.reasoning += p.text;
        break;
      }
      case "content_delta": {
        const p = ev.payload as ContentDeltaPayload;
        if (turn) turn.content += p.text;
        break;
      }
      case "tool_call": {
        const p = ev.payload as ToolCallPayload;
        if (turn) turn.toolCalls.push(p);
        break;
      }
      case "turn_finished": {
        const p = ev.payload as TurnFinishedPayload;
        if (turn) {
          blocks.push({ type: "assistant", ...turn, finishReason: p.finish_reason });
          turn = null;
        }
        break;
      }
      case "tool_result": {
        const p = ev.payload as ToolResultPayload;
        blocks.push({ type: "tool_result", seq: ev.seq, ...p });
        break;
      }
      case "tool_denied": {
        const p = ev.payload as ToolDeniedPayload;
        blocks.push({ type: "tool_denied", seq: ev.seq, ...p });
        break;
      }
      case "usage": {
        const p = ev.payload as UsagePayload;
        blocks.push({ type: "usage", seq: ev.seq, ...p });
        break;
      }
      case "run_finished": {
        const p = ev.payload as RunFinishedPayload;
        blocks.push({ type: "run_finished", seq: ev.seq, ...p });
        break;
      }
      case "error": {
        const p = ev.payload as ErrorPayload;
        blocks.push({ type: "error", seq: ev.seq, ...p });
        break;
      }
      case "tool_stdout":
        // Incremental command output; nothing renders it yet (phase 5:
        // "Streaming command output").
        break;
    }
  }

  return blocks;
}
