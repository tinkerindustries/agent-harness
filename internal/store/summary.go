package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GetEventsAfter returns sessionID's events with seq strictly greater than
// afterSeq, in seq order — the shape both the paged /events endpoint and an
// SSE stream's replay need: afterSeq is the client's Last-Event-ID or, for
// the paged endpoint, one less than the requested "from". limit caps the
// number of rows; limit <= 0 means unlimited, which the SSE replay path
// relies on to hand back a whole session's history in one call.
func (s *Store) GetEventsAfter(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = -1 // SQLite: a negative LIMIT means no limit.
	}
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT session_id, seq, kind, payload, created_at FROM events
		 WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		sessionID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var kind, createdAt, payload string
		if err := rows.Scan(&e.SessionID, &e.Seq, &kind, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.Kind = EventKind(kind)
		e.Payload = json.RawMessage(payload)
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("store: decode event created_at: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SessionUsageSummary is a session's running progress: how many sub-turns
// have committed and the accumulated token and cost totals from every usage
// event so far — the "sub-turns" and "running cost" columns of the session
// list (docs/DESIGN.md §5.8).
type SessionUsageSummary struct {
	SubTurns              int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	CompletionTokens      int
	ReasoningTokens       int
	CostUSD               float64
}

func (a SessionUsageSummary) add(p UsagePayload) SessionUsageSummary {
	a.PromptCacheHitTokens += p.PromptCacheHitTokens
	a.PromptCacheMissTokens += p.PromptCacheMissTokens
	a.CompletionTokens += p.CompletionTokens
	a.ReasoningTokens += p.ReasoningTokens
	a.CostUSD += p.CostUSD
	return a
}

// sqlPlaceholders returns "?,?,...", n copies, for building an IN clause.
// The caller supplies the matching arguments in the same order.
func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SessionUsageSummaries computes SessionUsageSummary for every id in
// sessionIDs in one query. It reads only the turn_started and usage event
// kinds rather than the full log — reasoning and content deltas can be
// large, and this recomputes on every session-list request and every live
// state publish (docs/DESIGN.md §5.8).
func (s *Store) SessionUsageSummaries(ctx context.Context, sessionIDs []string) (map[string]SessionUsageSummary, error) {
	out := make(map[string]SessionUsageSummary, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(sessionIDs)+2)
	for _, id := range sessionIDs {
		args = append(args, id)
	}
	args = append(args, string(KindTurnStarted), string(KindUsage))

	query := fmt.Sprintf(
		`SELECT session_id, kind, payload FROM events WHERE session_id IN (%s) AND kind IN (?, ?)`,
		sqlPlaceholders(len(sessionIDs)))
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, kind, payload string
		if err := rows.Scan(&sessionID, &kind, &payload); err != nil {
			return nil, err
		}
		sum := out[sessionID]
		switch EventKind(kind) {
		case KindTurnStarted:
			sum.SubTurns++
		case KindUsage:
			var p UsagePayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return nil, fmt.Errorf("store: decode usage payload: %w", err)
			}
			sum = sum.add(p)
		}
		out[sessionID] = sum
	}
	return out, rows.Err()
}

// RequestIDsForSessions maps each session id a work request named as its
// session_id back to that request's request_id. A session absent from the
// result was never created from a work request — a CLI-driven run, most
// often.
func (s *Store) RequestIDsForSessions(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT session_id, request_id FROM work_requests WHERE session_id IN (%s)`, sqlPlaceholders(len(sessionIDs)))
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, requestID string
		if err := rows.Scan(&sessionID, &requestID); err != nil {
			return nil, err
		}
		out[sessionID] = requestID
	}
	return out, rows.Err()
}
