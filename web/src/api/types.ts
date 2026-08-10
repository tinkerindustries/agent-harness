// Wire types mirroring the Go structs that produce them: internal/hub's
// SessionState/Usage and internal/store's Event and event payloads. Keeping
// field names identical to the JSON tags on the Go side is what lets a
// payload be used directly after JSON.parse, with no translation layer.

export interface SessionState {
  id: string;
  parent_id?: string;
  request_id?: string;
  job_type?: string;
  parent_agent_type?: string;
  parent_agent_id?: string;
  model: string;
  effort: string;
  workspace: string;
  permission_mode: string;
  status: string;
  // complete_status is the status argument the model gave Complete ("done"
  // or "gave_up"), mirroring internal/hub's SessionState. Empty covers both
  // a pre-migration row and a session that ended without calling Complete;
  // the badge renders it as the plain terminal status rather than guessing
  // (docs/WEB-REDESIGN.md phase 2).
  complete_status?: string;
  // plan is the session's working plan: the todos array of the most recent
  // TodoWrite call, verbatim, mirroring internal/hub's SessionState
  // (docs/WEB-REDESIGN.md phase 3). Absent covers a pre-migration row and a
  // session that never called TodoWrite; the in-flight card renders no plan
  // section rather than an empty one.
  plan?: Todo[];
  // recent_tool_calls is the last few tool calls the session made, for the
  // in-flight card's activity panel (docs/WEB-REDESIGN.md phase 3). Absent
  // when the session made none yet.
  recent_tool_calls?: RecentToolCall[];
  // summary is the summary argument the model gave Complete, its own
  // one-line account of the run, shown under the finished table's session
  // id (docs/WEB-REDESIGN.md phase 3). Absent when Complete was never
  // called.
  summary?: string;
  created_at: string;
  finished_at?: string;
  // Version is the row's optimistic-concurrency counter (docs/DATA-API.md):
  // the value a mutating write must echo back in If-Match, bumped by every
  // change to the row. It rides on every representation — list, single, and
  // the stream feed — so a client can always read a fresh version before
  // writing.
  version: number;
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
  | "error"
  | "steer_message"
  | "steer_applied";

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
  // Wall time of the sub-turn's request(s), measured server-side around the
  // stream calls. Absent on sessions committed before the field existed.
  elapsed_ms?: number;
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

// SteerMessagePayload mirrors internal/store.SteerMessagePayload: an
// operator instruction accepted for a running session (docs/RUN-CONTROL.md
// "Steering"). It carries no messages-array content — the loop decides where
// the model sees it and records that with a steer_applied event — so the
// browser shows it as a pending steer block until the matching steer_applied
// arrives.
export interface SteerMessagePayload {
  text: string;
  source?: string;
}

// SteerAppliedPayload mirrors internal/store.SteerAppliedPayload: the point
// in the log where a steer_message became a user message. source_seq links it
// to the steer_message it applies; the browser uses that to flip the pending
// steer block to delivered.
export interface SteerAppliedPayload {
  source_seq: number;
  text: string;
  sub_turn: number;
}

export interface EventsPage {
  events: StoreEvent[];
  from: number;
  limit: number;
  // has_more is whether more events follow this page, decided exactly by the
  // server peeking one row past it; next, present only when has_more, is the
  // from to ask for the next page with (docs/DATA-API.md "events").
  has_more: boolean;
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

// RecentToolCall mirrors internal/store.RecentToolCall: one entry of the
// session row's rolling roll of the last few tool calls, carried on the
// session list so an in-flight card can show what a running session is
// doing without opening its transcript (docs/WEB-REDESIGN.md phase 3).
// arguments is the raw text the model produced, kept only so the browser
// can shape a one-line target (file path, command, pattern) out of it.
export interface RecentToolCall {
  name: string;
  arguments: string;
  created_at: string;
}
