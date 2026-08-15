package queue

import (
	"encoding/json"
	"time"
)

// Result statuses (docs/DESIGN.md §4.10). "timeout" covers both budgets a run
// can exhaust, the wall-clock deadline and the sub-turn limit, distinguished
// by error.code. "cancelled" is an operator's decision — a run stopped on
// purpose — while "timeout" is a budget running out with no operator involved
// (docs/RUN-CONTROL.md "Half two"). Nothing produces a cancelled status today
// because the stop path is not wired in yet; it lands with run control.
const (
	StatusOK        = "ok"
	StatusFailed    = "failed"
	StatusDenied    = "denied"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
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

// Result is the terminal outcome of a work request, stored as the
// work_requests.result column (docs/DESIGN.md §4.10) — the same JSON the
// worker used to publish to the RESULTS stream, so the wire shape is
// unchanged for every caller.
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
	// CompleteStatus is the status argument to Complete, when the model
	// called it: "done" or "gave_up". Empty whenever Complete was never
	// called, which does not by itself mean the run failed — Status is
	// still what says whether the run finished, this only says how the
	// model itself characterised finishing it. Additive: it does not
	// change the meaning of any of the four Status values above.
	CompleteStatus string `json:"complete_status,omitempty"`
}
