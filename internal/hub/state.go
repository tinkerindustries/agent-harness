package hub

import (
	"encoding/json"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// SessionState is one row of the session list (docs/DESIGN.md §5.8):
// status, model, workspace, sub-turn count, running cost, and the
// originating request id where a work request created the session. It is
// the wire shape for both GET /api/sessions and the session_state events on
// GET /api/stream, so the browser's fold treats a row the same way
// regardless of which one delivered it.
type SessionState struct {
	ID              string `json:"id"`
	ParentID        string `json:"parent_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	JobType         string `json:"job_type,omitempty"`
	ParentAgentType string `json:"parent_agent_type,omitempty"`
	ParentAgentID   string `json:"parent_agent_id,omitempty"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Workspace       string `json:"workspace"`
	PermissionMode  string `json:"permission_mode"`
	Status          string `json:"status"`
	// CompleteStatus is the status argument the model gave Complete ("done"
	// or "gave_up"), so the session list can tell a finished task from one
	// the model gave up on (docs/WEB-REDESIGN.md phase 2). Empty covers a
	// pre-migration row and a session that ended without calling Complete;
	// the browser renders it as the plain terminal status rather than
	// guessing.
	CompleteStatus string `json:"complete_status,omitempty"`
	// Plan is the session's working plan: the todos array of the most
	// recent TodoWrite call, verbatim (docs/WEB-REDESIGN.md phase 3).
	// Absent covers a pre-migration row and a session that never called
	// TodoWrite; the browser renders the card without a plan section rather
	// than an empty one.
	Plan json.RawMessage `json:"plan,omitempty"`
	// RecentToolCalls is the last few tool calls the session made, for the
	// in-flight card's activity panel (docs/WEB-REDESIGN.md phase 3).
	// Absent when the session made none yet.
	RecentToolCalls []store.RecentToolCall `json:"recent_tool_calls,omitempty"`
	// Summary is the summary argument the model gave Complete, its own
	// one-line account of the run, shown under the finished table's session
	// id (docs/WEB-REDESIGN.md phase 3). Absent when Complete was never
	// called.
	Summary    string     `json:"summary,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Version is the row's optimistic-concurrency counter
	// (docs/DATA-API.md): the value a mutating write must echo back in
	// If-Match, bumped by every change to the row. It rides on every
	// representation — list, single, and the stream feed — so a client can
	// always read a fresh version before writing.
	Version  int   `json:"version"`
	SubTurns int   `json:"sub_turns"`
	Usage    Usage `json:"usage"`
	// PriceTableDate is the price table's own capture date, carried
	// alongside Usage so a cost figure never appears without saying how
	// current it is (docs/DESIGN.md §4.9). Empty when the caller building
	// this row has no price table to read a date from.
	PriceTableDate string `json:"price_table_date,omitempty"`
}

// Usage is a session's running token and cost totals, summed from every
// usage event committed so far (docs/CACHE.md's per-turn figures,
// accumulated).
type Usage struct {
	CacheHitTokens   int     `json:"cache_hit_tokens"`
	CacheMissTokens  int     `json:"cache_miss_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// BuildSessionState assembles the wire row from a session's stored
// metadata, its usage summary, its originating request id (if any), and the
// price table date its cost figure was computed under. It has no dependency
// on how the caller obtained those things, so both the live publish path
// (session.Runner, after every commit) and the REST list handler (a batch
// store query) build the identical shape from it.
func BuildSessionState(sess store.Session, summary store.SessionUsageSummary, requestID, priceTableDate string) SessionState {
	return SessionState{
		ID:              sess.ID,
		ParentID:        sess.ParentID,
		RequestID:       requestID,
		JobType:         sess.JobType,
		ParentAgentType: sess.ParentAgentType,
		ParentAgentID:   sess.ParentAgentID,
		Model:           sess.Model,
		Effort:          sess.Effort,
		Workspace:       sess.Workspace,
		PermissionMode:  sess.PermissionMode,
		Status:          sess.Status,
		CompleteStatus:  sess.CompleteStatus,
		Plan:            json.RawMessage(sess.Plan),
		RecentToolCalls: sess.RecentToolCalls,
		Summary:         sess.Summary,
		CreatedAt:       sess.CreatedAt,
		FinishedAt:      sess.FinishedAt,
		Version:         sess.Version,
		SubTurns:        summary.SubTurns,
		Usage: Usage{
			CacheHitTokens:   summary.PromptCacheHitTokens,
			CacheMissTokens:  summary.PromptCacheMissTokens,
			CompletionTokens: summary.CompletionTokens,
			ReasoningTokens:  summary.ReasoningTokens,
			CostUSD:          summary.CostUSD,
		},
		PriceTableDate: priceTableDate,
	}
}
