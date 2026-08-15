// Wire types mirroring the Go structs that produce them: internal/hub's
// SessionState/Usage and internal/store's Event and event payloads. Keeping
// field names identical to the JSON tags on the Go side is what lets a
// payload be used directly after JSON.parse, with no translation layer.

// SessionListRow is what GET /api/stream carries, mirroring internal/hub's
// ListRow: the projection of a session row down to the fields the session
// list renders. That feed re-sends a whole row on every sub-turn of every
// running session to every open list, so the fields it leaves out — the
// tool-call roll above all, which is the biggest thing on the row and which
// this screen has no pixel for — are ones nobody was drawing. A screen that
// wants the rest asks for the whole row: GET /api/sessions for the Finished
// table, GET /api/sessions/{id} and the session stream's `state` frames for
// the detail screens.
export interface SessionListRow {
  id: string;
  request_id?: string;
  job_type?: string;
  parent_agent_type?: string;
  parent_agent_id?: string;
  // parent_is_user records that a person started this session directly,
  // mirroring internal/hub's SessionState. Absent or false covers a
  // pre-migration row; the display helper falls back to the legacy
  // parent_agent_type === "user" encoding when rendering.
  parent_is_user?: boolean;
  model: string;
  effort: string;
  workspace: string;
  status: string;
  // complete_status is the status argument the model gave Complete ("done"
  // or "gave_up"), mirroring internal/hub's SessionState. Empty covers both
  // a pre-migration row and a session that ended without calling Complete;
  // the badge renders it as the plain terminal status rather than guessing.
  complete_status?: string;
  // task is the job's description — the launching instruction of the run,
  // frozen on the row at creation (the same value the session_started
  // payload carries), mirroring internal/hub's SessionState. Absent covers a
  // pre-migration row and a run created with no prompt (a browser start
  // waits for its first message); the in-flight card renders no description
  // rather than an empty one.
  //
  // On the list feed alone it is capped at internal/hub's MaxListTaskChars
  // and marked with a trailing ellipsis when it was cut: the task is
  // immutable but re-sent on every sub-turn, and this screen shows it in a
  // three-line clamp and a tooltip. The whole task is on the REST row.
  task?: string;
  // title is the run's name (at most 10 words), shown bold on the main page
  // in place of the raw prompt, mirroring internal/hub's SessionState.
  // Absent covers a pre-migration row and a producer that left it blank; the
  // list then renders the task line without a bold title.
  title?: string;
  // description is what change the agent is making (at most 50 words),
  // shown under the title on the main page, mirroring internal/hub's
  // SessionState. Absent covers a pre-migration row and a producer that
  // left it blank; the list then shows no description line when the run has
  // a title, and falls back to the task only when there is no title either.
  description?: string;
  // phase is this run's 1-based position in a multi-phase chain, mirroring
  // internal/hub's SessionState. Absent together with total_phases absent
  // means the run is not part of a chain; the list shows no phase chip.
  phase?: number;
  // total_phases is how many phases the chain has, mirroring
  // internal/hub's SessionState. When present, the list renders a small
  // "phase N/M" chip next to the title.
  total_phases?: number;
  // plan is the session's working plan: the todos array as of the most
  // recent TaskCreate/TaskUpdate call, verbatim, mirroring internal/hub's
  // SessionState. Absent covers a
  // pre-migration row and a session that never wrote a plan; the in-flight
  // card renders no plan section rather than an empty one.
  plan?: Todo[];
  created_at: string;
  finished_at?: string;
  sub_turns: number;
  usage: ListUsage;
}

// SessionState is a session's whole metadata row, mirroring internal/hub's
// SessionState: what GET /api/sessions and GET /api/sessions/{id} return, and
// what the session stream's `state` frames carry. It extends the list row
// rather than restating it, so a full row is usable anywhere a list row is —
// which is what lets the display helpers (statusBadge, sessionListTitle,
// provenance) serve the list and the detail screens from one implementation.
export interface SessionState extends SessionListRow {
  parent_id?: string;
  permission_mode: string;
  // recent_tool_calls is the rolling roll of the last few tool calls the
  // session made, mirroring internal/hub's SessionState. Nothing renders it;
  // the field stays on the row because it rides the plan's store write. It is
  // off the list feed entirely (SessionListRow) — it was the largest field
  // there and carries each call's raw arguments. Absent when the session made
  // none yet.
  recent_tool_calls?: RecentToolCall[];
  // summary is the summary argument the model gave Complete, its own
  // one-line account of the run, shown under the finished table's session
  // id. Absent when Complete was never called.
  summary?: string;
  // Version is the row's optimistic-concurrency counter (docs/DATA-API.md):
  // the value a mutating write must echo back in If-Match, bumped by every
  // change to the row. It rides on every full representation — list, single,
  // and the session stream's state frames — so a client can always read a
  // fresh version before writing.
  version: number;
  usage: Usage;
  // price_table_date is the config price table's own capture date
  // (internal/pricing.Table.CapturedAt), carried alongside usage so a cost
  // figure is never shown without saying how current it is (docs/DESIGN.md
  // §4.9: "A cost figure computed from a stale table is worse than no
  // figure"). Empty when the session predates this field or the server
  // omitted it.
  price_table_date?: string;
}

// ListUsage is the one usage figure the session list reads: the running cost,
// which the stat strip sums into "Spend today". The token counts behind it
// are Finished-table columns, and that table reads the full row.
export interface ListUsage {
  cost_usd: number;
}

export interface Usage extends ListUsage {
  cache_hit_tokens: number;
  cache_miss_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
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
  // The paths the request's attachments were materialised under —
  // scratch/attachments/<name> inside the session workspace — when the run
  // carried any. Sent separately, the way skill_catalogue is, so the opening
  // block can render the images through GET /api/sessions/{id}/screenshot
  // without parsing the message text.
  attachments?: string[];
  // task is the launching agent's own instruction — the "Task:\n..." tail of
  // opening_message — when the run was created with one (an agent-started
  // run; a browser start is created empty and waits for its first message).
  // Carried separately, the way skill_catalogue is, so the watch page can
  // render the launcher's instruction as its own message without parsing
  // the message text. Only the run-creating session_started carries it;
  // a resume instruction is a continuation, not the launch instruction.
  task?: string;
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
  // image_url is the base64 data URI of an image Read returned as an
  // image_url part to a vision provider (docs/KIMI-INTEGRATION.md §4.5),
  // mirroring internal/store.ToolResultPayload.ImageURL. Present only for
  // that case; the transcript renders it inline above the result text, so
  // the picture the model was looking at is part of the record.
  image_url?: string;
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
  /**
   * Set only when the request was not the session's own — a Glance, Ground
   * or Detect call bills a vision model and commits a second usage event on
   * the same sub-turn (Crop makes no model call and commits none). Absent
   * means the session's own model, so the transcript names the model only
   * when there is something to distinguish.
   */
  model?: string;
  prompt_tokens: number;
  prompt_cache_hit_tokens: number;
  prompt_cache_miss_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
  cost_usd: number;
  /**
   * Which set of rates cost_usd was computed at — "flat", "peak" or
   * "off_peak" (internal/pricing) — mirroring internal/store's UsagePayload.
   * From 2026-08-16 DeepSeek bills peak hours at twice off-peak, so two
   * sub-turns with identical token counts can differ threefold in cost for
   * a reason invisible in the figures beside it; this is that reason.
   * Absent on a session committed before the split, and on any usage the
   * price table could not cost at all.
   */
  rate_tier?: string;
  /**
   * How many provider requests this one event accounts for, mirroring
   * internal/store's UsagePayload. Set only where that is not one:
   * Transcribe fans a tall image out over a call per chunk and sums them
   * into a single event, because this card shows one usage block per
   * sub-turn and a later one replaces an earlier one, so separate events
   * would display the price of one chunk. Absent means one call, which is
   * every other usage event.
   */
  calls?: number;
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

// Page is the envelope every paginated list endpoint returns
// (docs/DATA-API.md "Pagination"): the items of one page, the total number
// of rows matching the filter, the limit and offset actually applied, and
// has_more/next for paging forward. It is the one shape GET /api/sessions
// answers with — never a bare array.
export interface Page<T> {
  items: T[];
  total: number;
  limit: number;
  offset: number;
  has_more: boolean;
  next?: number;
}

// QueueHealth mirrors internal/httpapi's queueHealth: GET /api/queue's
// response, the queue health the session list shows. Available
// is false whenever there is nothing to report — no queue wired up, or the
// stats read itself failed (Error then says why) — which the session
// list treats as "say nothing" rather than an error state of its own.
export interface QueueHealth {
  available: boolean;
  queue_depth?: number;
  scheduled?: number;
  in_flight?: number;
  redelivered?: number;
  halted: boolean;
  halt_reason?: string;
  error?: string;
}

// Todo mirrors internal/tools.Todo: one entry of the model's working plan,
// kept client-side by folding TaskCreate/TaskUpdate tool-call events in call
// order (web/src/api/fold.ts applyTaskEvent), the same replay the store runs
// over the event log (internal/store/status.go). taskId is minted by
// TaskCreate in call order and stable for the task's whole life, so a later
// TaskUpdate can name one task cheaply. subject is the brief actionable
// title and description the longer explanation, matching Claude Code's own
// Task tools.
export interface Todo {
  taskId: string;
  subject: string;
  description: string;
  status: "pending" | "in_progress" | "completed";
  activeForm: string;
}

// RecentToolCall mirrors internal/store.RecentToolCall: one entry of the
// session row's rolling roll of the last few tool calls. The roll used to
// feed the in-flight card's activity panel, which is gone; the field stays
// because it rides the same store write as the plan. arguments is the raw
// text the model produced, kept only so a consumer can shape a one-line
// target (file path, command, pattern) out of it.
export interface RecentToolCall {
  name: string;
  arguments: string;
  created_at: string;
}
