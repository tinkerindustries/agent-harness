package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// StatusTodo is one entry of a run's working plan, recovered by replaying
// TaskCreate/TaskUpdate tool-call events in event order. The field names
// mirror internal/tools.Todo so a todo survives the trip unchanged.
type StatusTodo struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

// applyTaskEvent patches current with one TaskCreate or TaskUpdate tool-call
// event in event order, the way the live handlers mutate the executor's plan:
// TaskCreate mints ids from *nextID (increment then use, decimal string) and
// appends; TaskUpdate finds by id and patches the fields present or removes
// the task. Events that the live handler would have rejected — malformed
// JSON, an invalid status, an empty content, a delete combined with a patch,
// an unknown id — are skipped with current returned unchanged. This replay
// runs over already-committed data and must never fail loudly, so nothing
// here produces an error.
func applyTaskEvent(current []StatusTodo, nextID *int, name, argsJSON string) []StatusTodo {
	switch name {
	case "TaskCreate":
		var args struct {
			Tasks []StatusTodo `json:"tasks"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args.Tasks) == 0 {
			return current
		}
		for _, t := range args.Tasks {
			if strings.TrimSpace(t.Content) == "" || strings.TrimSpace(t.ActiveForm) == "" {
				return current
			}
			if t.Status != "" && !validTaskStatus(t.Status) {
				return current
			}
		}
		out := make([]StatusTodo, len(current), len(current)+len(args.Tasks))
		copy(out, current)
		for _, t := range args.Tasks {
			*nextID++
			status := t.Status
			if status == "" {
				status = "pending"
			}
			out = append(out, StatusTodo{
				ID:         strconv.Itoa(*nextID),
				Content:    t.Content,
				Status:     status,
				ActiveForm: t.ActiveForm,
			})
		}
		return out
	case "TaskUpdate":
		var args struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			Content    string `json:"content"`
			ActiveForm string `json:"activeForm"`
			Delete     bool   `json:"delete"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) != nil || args.ID == "" {
			return current
		}
		if args.Delete && (args.Status != "" || args.Content != "" || args.ActiveForm != "") {
			return current
		}
		if !args.Delete && args.Status == "" && args.Content == "" && args.ActiveForm == "" {
			return current
		}
		if args.Status != "" && !validTaskStatus(args.Status) {
			return current
		}
		for i, t := range current {
			if t.ID != args.ID {
				continue
			}
			if args.Delete {
				out := make([]StatusTodo, 0, len(current)-1)
				out = append(out, current[:i]...)
				out = append(out, current[i+1:]...)
				return out
			}
			out := make([]StatusTodo, len(current))
			copy(out, current)
			if args.Status != "" {
				out[i].Status = args.Status
			}
			if args.Content != "" {
				out[i].Content = args.Content
			}
			if args.ActiveForm != "" {
				out[i].ActiveForm = args.ActiveForm
			}
			return out
		}
		return current
	default:
		return current
	}
}

// validTaskStatus mirrors internal/tools' status enum for the replay above.
func validTaskStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed":
		return true
	}
	return false
}

// StatusToolCall is one tool call the run is still waiting on: it has a
// tool_call event and no tool_result or tool_denied with the same id yet.
type StatusToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// RequestStatus is everything a caller needs to know about one work request
// at the moment the query runs: the idempotency row's own fields, plus what
// the event log (if any session has started) says the run is doing right
// now. Usage is the most recent usage event, not an accumulated total; this
// is a polled snapshot, and the full accounting lives in
// SessionUsageSummaries.
type RequestStatus struct {
	RequestID    string
	SessionID    string
	Status       string
	SubTurn      int
	Todos        []StatusTodo
	ActiveForm   string
	ToolCalls    []StatusToolCall
	Usage        *UsagePayload
	StartedAt    time.Time
	FinishedAt   *time.Time
	ErrorCode    string
	ErrorMessage string
}

// RequestStatus assembles RequestStatus for one request_id, anchored on the
// work_requests row because that row exists from the moment the message is
// claimed, before any workspace or session row does. A terminal row with no
// session is a normal answer: the request was refused or invalid, and the
// result JSON it stored carries why. When a session exists, progress is
// derived from the event log.
//
// The events query reads only the kinds this status needs — turn_started,
// tool_call, tool_result, tool_denied, usage — never reasoning_delta or
// content_delta, whose payloads are large and would defeat the point of a
// cheap polled status call. This mirrors SessionUsageSummaries, which says
// why in its own comment.
func (s *Store) RequestStatus(ctx context.Context, requestID string) (RequestStatus, error) {
	wr, err := s.GetWorkRequest(ctx, requestID)
	if err != nil {
		return RequestStatus{}, err
	}

	st := RequestStatus{
		RequestID:  wr.RequestID,
		SessionID:  wr.SessionID,
		Status:     wr.Status,
		StartedAt:  wr.ReceivedAt,
		FinishedAt: wr.FinishedAt,
	}
	if wr.Status != WorkRequestStatusRunning && len(wr.Result) > 0 {
		var res struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(wr.Result, &res) == nil && res.Error != nil {
			st.ErrorCode = res.Error.Code
			st.ErrorMessage = res.Error.Message
		}
	}

	if wr.SessionID == "" {
		return st, nil
	}

	rows, err := s.readDB.QueryContext(ctx,
		`SELECT seq, kind, payload FROM events
		 WHERE session_id = ? AND kind IN (?, ?, ?, ?, ?) ORDER BY seq ASC`,
		wr.SessionID, KindTurnStarted, KindToolCall, KindToolResult, KindToolDenied, KindUsage)
	if err != nil {
		return RequestStatus{}, err
	}
	defer rows.Close()

	var subTurn int
	var todos []StatusTodo
	var usage *UsagePayload
	resolved := make(map[string]bool)
	calls := make([]StatusToolCall, 0)
	nextID := 0

	for rows.Next() {
		var seq int64
		var kind, payload string
		if err := rows.Scan(&seq, &kind, &payload); err != nil {
			return RequestStatus{}, err
		}
		switch EventKind(kind) {
		case KindTurnStarted:
			var p TurnStartedPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				subTurn = p.SubTurn
			}
		case KindToolCall:
			var p ToolCallPayload
			if json.Unmarshal([]byte(payload), &p) != nil {
				continue
			}
			if p.Name == "TaskCreate" || p.Name == "TaskUpdate" {
				todos = applyTaskEvent(todos, &nextID, p.Name, p.Arguments)
			}
			calls = append(calls, StatusToolCall{ID: p.ID, Name: p.Name, Arguments: p.Arguments})
		case KindToolResult:
			var p ToolResultPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				resolved[p.ToolCallID] = true
			}
		case KindToolDenied:
			var p ToolDeniedPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				resolved[p.ToolCallID] = true
			}
		case KindUsage:
			var p UsagePayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				usage = &p
			}
		}
	}
	if err := rows.Err(); err != nil {
		return RequestStatus{}, err
	}

	st.SubTurn = subTurn
	st.Todos = todos
	var inFlight []StatusToolCall
	for _, c := range calls {
		if !resolved[c.ID] {
			inFlight = append(inFlight, c)
		}
	}
	st.ToolCalls = inFlight
	st.Usage = usage
	for _, t := range todos {
		if t.Status == "in_progress" {
			st.ActiveForm = t.ActiveForm
			break
		}
	}
	return st, nil
}
