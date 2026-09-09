package hub

import (
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// SessionState is a session's full metadata row (docs/DESIGN.md §5.8):
// status, model, workspace, sub-turn count, running cost, and the
// originating request id where a work request created the session. It is
// the wire shape for GET /api/sessions, for GET /api/sessions/{id}, and for
// the `state` frames on one session's own transcript stream — the three
// places a caller has asked about a session and wants everything the row
// holds.
//
// It is NOT what GET /api/stream carries. That feed serves the session list,
// which renders a fraction of these fields, and it re-sends a whole row on
// every sub-turn to every open list; ListRow is the projection it sends
// instead, and ListRowOf is the one place the two shapes meet.
type SessionState struct {
	ID              string `json:"id"`
	ParentID        string `json:"parent_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	JobType         string `json:"job_type,omitempty"`
	ParentAgentType string `json:"parent_agent_type,omitempty"`
	ParentAgentID   string `json:"parent_agent_id,omitempty"`
	// ParentIsUser records that a person started this session directly. It is
	// producer-stamped on the request; absent or false covers a pre-migration
	// row, and the browser falls back to the legacy parent_agent_type "user"
	// encoding when rendering.
	ParentIsUser   bool   `json:"parent_is_user,omitempty"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	Workspace      string `json:"workspace"`
	PermissionMode string `json:"permission_mode"`
	Status         string `json:"status"`
	// CompleteStatus is the status argument the model gave Complete ("done"
	// or "gave_up"), so the session list can tell a finished task from one
	// the model gave up on. Empty covers a
	// pre-migration row and a session that ended without calling Complete;
	// the browser renders it as the plain terminal status rather than
	// guessing.
	CompleteStatus string `json:"complete_status,omitempty"`
	// Task is the job's description — the launching instruction of the run,
	// frozen on the row at creation (the same value the session_started
	// payload carries). The in-flight card renders it as its description.
	// Absent covers a pre-migration row and a run created with no prompt (a
	// browser start waits for its first message); the card renders no
	// description rather than an empty one.
	Task string `json:"task,omitempty"`
	// Title is the run's name, at most agentmeta.MaxTitleWords words, shown
	// bold on the main page in place of the raw prompt. Absent covers a
	// pre-migration row and a producer that left it blank; the browser then
	// renders the task line without a bold title rather than an empty one.
	Title string `json:"title,omitempty"`
	// Description is what change the agent is making, at most
	// agentmeta.MaxDescriptionWords words, shown under the title on the main
	// page. Absent covers a pre-migration row and a producer that left it
	// blank; the browser falls back to the task as the description line.
	Description string `json:"description,omitempty"`
	// Phase is this run's 1-based position in a multi-phase chain. Absent
	// together with total_phases absent means the run is not part of a
	// chain; the browser shows no phase chip then.
	Phase int `json:"phase,omitempty"`
	// TotalPhases is how many phases the chain has. When present, Phase is
	// 1-based and at most TotalPhases; the browser renders a "phase N/M"
	// chip next to the title.
	TotalPhases int `json:"total_phases,omitempty"`
	// Plan is the session's working plan: the todos array of the most
	// recent TodoWrite call, verbatim.
	// Absent covers a pre-migration row and a session that never called
	// TodoWrite; the browser renders the card without a plan section rather
	// than an empty one.
	Plan json.RawMessage `json:"plan,omitempty"`
	// RecentToolCalls is the rolling roll of the last few tool calls the
	// session made, carried on the wire for the same reason it is stored:
	// it rides the plan's store write. The in-flight card no longer renders
	// it. Absent when the session made none yet.
	RecentToolCalls []store.RecentToolCall `json:"recent_tool_calls,omitempty"`
	// Summary is the summary argument the model gave Complete, its own
	// one-line account of the run, shown under the finished table's session
	// id. Absent when Complete was never
	// called.
	Summary    string     `json:"summary,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Version is the row's optimistic-concurrency counter
	// : the value a mutating write must echo back in
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

// MaxListTaskChars is how much of a run's task the session list feed
// carries. The list renders the task in two places, and neither wants the
// whole thing: as the description line when a run has no title or
// description of its own — a line CSS-clamped to three lines — and as the
// card's hover tooltip. Measured against a live harness the stored task
// averages 4KB and reaches 20KB, and it is immutable, so an uncapped task is
// the same kilobytes re-sent on every sub-turn for a line that shows two
// hundred characters of it. The full task stays one GET
// /api/sessions/{id} away.
const MaxListTaskChars = 300

// ListRow is one row of the session list feed (docs/DESIGN.md §5.8): the
// projection of SessionState down to the fields the list screen actually
// renders — the badge, the provenance line, the title/description lines, the
// plan, elapsed, sub-turns, and the running cost the stat strip sums.
//
// The projection exists because of the rate, not the size of any one row.
// GET /api/stream re-sends a whole row on every sub-turn and every plan
// write, to every browser with the list open, for every running session; the
// fields left out below are the ones no pixel on that screen depends on.
// Three are worth naming:
//
//   - RecentToolCalls, the largest field on the row by some distance (9.9KB
//     average, 35KB at the top end, measured over a live harness), carries
//     each call's raw arguments — a Write's whole file body among them. The
//     in-flight card stopped rendering it and the field stayed on the wire
//     because it rides the plan's store write.
//   - Summary and the cache-token counts belong to the Finished table, which
//     is server-paged off GET /api/sessions and has never read them from
//     this feed.
//   - PermissionMode, Version, ParentID and PriceTableDate are read on the
//     session detail screen and by the operations screen, both of which go
//     to the REST row.
//
// The JSON names are SessionState's own, so a row from this feed is a subset
// of the full row rather than a translation of it, and one browser type
// covers both.
type ListRow struct {
	ID              string `json:"id"`
	RequestID       string `json:"request_id,omitempty"`
	JobType         string `json:"job_type,omitempty"`
	ParentAgentType string `json:"parent_agent_type,omitempty"`
	ParentAgentID   string `json:"parent_agent_id,omitempty"`
	ParentIsUser    bool   `json:"parent_is_user,omitempty"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Workspace       string `json:"workspace"`
	Status          string `json:"status"`
	CompleteStatus  string `json:"complete_status,omitempty"`
	// Task is capped at MaxListTaskChars; see the constant for why the list
	// feed does not carry the whole prompt.
	Task        string          `json:"task,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Phase       int             `json:"phase,omitempty"`
	TotalPhases int             `json:"total_phases,omitempty"`
	Plan        json.RawMessage `json:"plan,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	SubTurns    int             `json:"sub_turns"`
	Usage       ListUsage       `json:"usage"`
}

// ListUsage is the one figure the session list reads off a row's usage: the
// running cost, which the stat strip sums into "Spend today". The token
// counts behind it are a Finished-table column, served by GET /api/sessions.
// It is a struct rather than a bare field so the JSON stays `usage.cost_usd`
// and a list row keeps reading like the full row it came from.
type ListUsage struct {
	CostUSD float64 `json:"cost_usd"`
}

// ListRowOf projects a full row down to the list feed's shape. It is the
// only place the two shapes meet: every path that publishes to the list
// stream goes through it, so a field added to SessionState reaches the list
// only by being added here on purpose.
func ListRowOf(s SessionState) ListRow {
	return ListRow{
		ID:              s.ID,
		RequestID:       s.RequestID,
		JobType:         s.JobType,
		ParentAgentType: s.ParentAgentType,
		ParentAgentID:   s.ParentAgentID,
		ParentIsUser:    s.ParentIsUser,
		Model:           s.Model,
		Effort:          s.Effort,
		Workspace:       s.Workspace,
		Status:          s.Status,
		CompleteStatus:  s.CompleteStatus,
		Task:            truncateChars(s.Task, MaxListTaskChars),
		Title:           s.Title,
		Description:     s.Description,
		Phase:           s.Phase,
		TotalPhases:     s.TotalPhases,
		Plan:            s.Plan,
		CreatedAt:       s.CreatedAt,
		FinishedAt:      s.FinishedAt,
		SubTurns:        s.SubTurns,
		Usage:           ListUsage{CostUSD: s.Usage.CostUSD},
	}
}

// truncateChars cuts s to at most n runes, marking the cut with an ellipsis
// so a reader can tell a capped task from one that was always this short. It
// counts runes rather than bytes so a multi-byte character is never split
// into invalid UTF-8 on the way out.
func truncateChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
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
func BuildSessionState(sess store.Session, summary store.SessionUsageSummary, priceTableDate string) SessionState {
	return SessionState{
		ID:              sess.ID,
		ParentID:        sess.ParentID,
		JobType:         sess.JobType,
		ParentAgentType: sess.ParentAgentType,
		ParentAgentID:   sess.ParentAgentID,
		ParentIsUser:    sess.ParentIsUser,
		Model:           sess.Model,
		Effort:          sess.Effort,
		Workspace:       sess.Workspace,
		PermissionMode:  sess.PermissionMode,
		Status:          sess.Status,
		CompleteStatus:  sess.CompleteStatus,
		Task:            sess.Task,
		Title:           sess.Title,
		Description:     sess.Description,
		Phase:           sess.Phase,
		TotalPhases:     sess.TotalPhases,
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
