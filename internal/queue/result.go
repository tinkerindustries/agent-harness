package queue

import (
	"encoding/json"
	"time"
)

// Result statuses (docs/DESIGN.md §4.10). "timeout" covers both budgets a run
// can exhaust, the wall-clock deadline and the sub-turn limit, distinguished
// by error.code. Nothing produces a "cancelled" status because nothing can
// cancel a run: the browser is read-only and shutdown drains in-flight work.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusDenied  = "denied"
	StatusTimeout = "timeout"
)

// ResultError is the machine-readable failure detail on a non-ok Result.
type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResultUsage mirrors session.Usage in the wire shape docs/DESIGN.md §4.9
// promises a requester: the same figures the database holds, so a caller
// can price its own job without reading it.
type ResultUsage struct {
	CacheHitTokens  int     `json:"cache_hit_tokens"`
	CacheMissTokens int     `json:"cache_miss_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	ReasoningTokens int     `json:"reasoning_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	PriceTableDate  string  `json:"price_table_date"`
}

// Result is the terminal message published to
// harness.work.result.<request_id>.final (docs/DESIGN.md §4.10).
type Result struct {
	RequestID  string          `json:"request_id"`
	SessionID  string          `json:"session_id,omitempty"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Text       string          `json:"text"`
	Error      *ResultError    `json:"error,omitempty"`
	Usage      *ResultUsage    `json:"usage,omitempty"`
	SubTurns   int             `json:"sub_turns"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
}

// Accepted is published once to harness.work.result.<request_id>.accepted
// when a worker begins a request, before it is known whether the run will
// succeed.
type Accepted struct {
	RequestID string    `json:"request_id"`
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
}

// Progress is published to harness.work.result.<request_id>.progress at
// most once a second (docs/DESIGN.md §4.10). It carries turn-level
// summaries only — never content deltas — because full fidelity already
// lives in the event log, on disk, and (in a later phase) on the SSE
// stream.
//
// ExpectedMissTokens, Churned, and ChurnPointIndex are the churn diagnostic
// (docs/CACHE.md) surfaced to a queue consumer, not just the CLI and the
// browser: a requester watching progress can tell a healthy sub-turn from a
// churned one without reading the database.
type Progress struct {
	RequestID          string       `json:"request_id"`
	SessionID          string       `json:"session_id"`
	SubTurn            int          `json:"sub_turn"`
	ToolCalls          []string     `json:"tool_calls,omitempty"`
	Usage              *ResultUsage `json:"usage,omitempty"`
	ExpectedMissTokens int          `json:"expected_miss_tokens"`
	Churned            bool         `json:"churned,omitempty"`
	ChurnPointIndex    *int         `json:"churn_point_index,omitempty"`
	Timestamp          time.Time    `json:"timestamp"`
}

// FinalMsgID is the Nats-Msg-Id set on a final-result publish. Deriving it
// from request_id, rather than from anything about this particular attempt,
// is what makes a redelivered or republished final result deduplicate
// inside the stream's window instead of producing a second message
// (docs/DESIGN.md §4.10).
func FinalMsgID(requestID string) string {
	return requestID + ".final"
}
