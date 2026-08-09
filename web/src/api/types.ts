// Wire types mirroring the Go structs that produce them: internal/hub's
// SessionState/Usage and internal/store's Event and event payloads. Keeping
// field names identical to the JSON tags on the Go side is what lets a
// payload be used directly after JSON.parse, with no translation layer.

export interface SessionState {
  id: string;
  parent_id?: string;
  request_id?: string;
  model: string;
  effort: string;
  workspace: string;
  permission_mode: string;
  status: string;
  created_at: string;
  finished_at?: string;
  sub_turns: number;
  usage: Usage;
}

export interface Usage {
  cache_hit_tokens: number;
  cache_miss_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
  cost_usd: number;
}

export type EventKind =
  | "session_started"
  | "turn_started"
  | "reasoning_delta"
  | "content_delta"
  | "tool_call"
  | "tool_denied"
  | "tool_stdout"
  | "tool_result"
  | "usage"
  | "turn_finished"
  | "run_finished"
  | "error";

// StoreEvent mirrors internal/store.Event: one row of a session's
// append-only log. payload's shape depends on kind; see the *Payload
// interfaces below, which mirror internal/store/events.go one for one.
export interface StoreEvent {
  session_id: string;
  seq: number;
  kind: EventKind;
  payload: unknown;
  created_at: string;
}

export interface SessionStartedPayload {
  opening_message: string;
}

export interface TurnStartedPayload {
  sub_turn: number;
}

export interface ReasoningDeltaPayload {
  text: string;
}

export interface ContentDeltaPayload {
  text: string;
}

export interface ToolCallPayload {
  index: number;
  id: string;
  name: string;
  arguments: string;
}

export interface ToolResultPayload {
  tool_call_id: string;
  name: string;
  content: string;
  is_error?: boolean;
  truncated?: boolean;
}

export interface ToolDeniedPayload {
  tool_call_id: string;
  name: string;
  rule: string;
  content: string;
}

export interface UsagePayload {
  prompt_tokens: number;
  prompt_cache_hit_tokens: number;
  prompt_cache_miss_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
  cost_usd: number;
  expected_miss_tokens: number;
  churn_point_index?: number;
}

export interface TurnFinishedPayload {
  finish_reason: string;
}

export interface RunFinishedPayload {
  reason: string;
  status?: string;
  summary?: string;
  result?: unknown;
  text: string;
}

export interface ErrorPayload {
  message: string;
}

export interface EventsPage {
  events: StoreEvent[];
  from: number;
  limit: number;
  next?: number;
}
