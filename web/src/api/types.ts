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
  // price_table_date is the config price table's own capture date
  // (internal/pricing.Table.CapturedAt), carried alongside usage so a cost
  // figure is never shown without saying how current it is (docs/DESIGN.md
  // §4.9: "A cost figure computed from a stale table is worse than no
  // figure"). Empty when the session predates this field or the server
  // omitted it.
  price_table_date?: string;
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
  // The skills catalogue embedded in opening_message, when the workspace had
  // any. Sent separately so the fold can lift it into its own block without
  // parsing the message text.
  skill_catalogue?: string;
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

// DiffLine mirrors internal/store.DiffLine: one line of an Edit's computed
// diff, tagged context/add/remove with 1-based line numbers on whichever
// side it belongs to.
export interface DiffLine {
  kind: "context" | "add" | "remove";
  text: string;
  old_line?: number;
  new_line?: number;
}

export interface ToolResultPayload {
  tool_call_id: string;
  name: string;
  content: string;
  is_error?: boolean;
  truncated?: boolean;
  diff?: DiffLine[];
  child_session_id?: string;
}

export interface ToolDeniedPayload {
  tool_call_id: string;
  name: string;
  rule: string;
  content: string;
}

// ToolStdoutPayload is one chunk of a running Bash call's live output
// (internal/store.ToolStdoutPayload), coalesced server-side by
// liveStdoutWriter rather than forwarded write-for-write.
export interface ToolStdoutPayload {
  tool_call_id: string;
  text: string;
}

// One event per request to the API, not per sub-turn: a sub-turn that hit the
// reasoning-starved retry commits two, sharing a sub_turn and distinguished by
// attempt. attempt is absent on the ordinary one-request path.
export interface UsagePayload {
  sub_turn: number;
  attempt?: number;
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
  sub_turn: number;
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

// QueueHealth mirrors internal/httpapi's queueHealth: GET /api/queue's
// response, the queue health the session list shows. Available
// is false whenever there is nothing to report — no queue wired up, or the
// live NATS call itself failed (Error then says why) — which the session
// list treats as "say nothing" rather than an error state of its own.
export interface QueueHealth {
  available: boolean;
  consumer_lag?: number;
  in_flight?: number;
  redelivered?: number;
  halted: boolean;
  halt_reason?: string;
  error?: string;
}

// Todo mirrors internal/tools.Todo: one entry of the model's working plan,
// parsed client-side from the arguments of the latest TodoWrite call rather
// than carried on its own event (docs/TOOLS.md "TodoWrite").
export interface Todo {
  content: string;
  status: "pending" | "in_progress" | "completed";
  activeForm: string;
}
