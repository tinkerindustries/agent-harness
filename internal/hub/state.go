package hub

import (
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
	ID             string     `json:"id"`
	ParentID       string     `json:"parent_id,omitempty"`
	RequestID      string     `json:"request_id,omitempty"`
	Model          string     `json:"model"`
	Effort         string     `json:"effort"`
	Workspace      string     `json:"workspace"`
	PermissionMode string     `json:"permission_mode"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	SubTurns       int        `json:"sub_turns"`
	Usage          Usage      `json:"usage"`
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
// metadata, its usage summary, and its originating request id, if any. It
// has no dependency on how the caller obtained those three things, so both
// the live publish path (session.Runner, after every commit) and the REST
// list handler (a batch store query) build the identical shape from it.
func BuildSessionState(sess store.Session, summary store.SessionUsageSummary, requestID string) SessionState {
	return SessionState{
		ID:             sess.ID,
		ParentID:       sess.ParentID,
		RequestID:      requestID,
		Model:          sess.Model,
		Effort:         sess.Effort,
		Workspace:      sess.Workspace,
		PermissionMode: sess.PermissionMode,
		Status:         sess.Status,
		CreatedAt:      sess.CreatedAt,
		FinishedAt:     sess.FinishedAt,
		SubTurns:       summary.SubTurns,
		Usage: Usage{
			CacheHitTokens:   summary.PromptCacheHitTokens,
			CacheMissTokens:  summary.PromptCacheMissTokens,
			CompletionTokens: summary.CompletionTokens,
			ReasoningTokens:  summary.ReasoningTokens,
			CostUSD:          summary.CostUSD,
		},
	}
}
