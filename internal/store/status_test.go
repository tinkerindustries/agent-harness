package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRequestStatusClaimedNoSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	claimedAt := time.Now().UTC().Add(-time.Minute)
	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, claimedAt); err != nil {
		t.Fatalf("claim: %v", err)
	}

	st, err := s.RequestStatus(ctx, "req-1")
	if err != nil {
		t.Fatalf("request status: %v", err)
	}
	if st.SessionID != "" {
		t.Fatalf("expected no session id yet, got %q", st.SessionID)
	}
	if st.Status != WorkRequestStatusRunning {
		t.Fatalf("expected status %q, got %q", WorkRequestStatusRunning, st.Status)
	}
	if st.SubTurn != 0 || len(st.Todos) != 0 || len(st.ToolCalls) != 0 || st.Usage != nil {
		t.Fatalf("expected an empty in-flight picture, got %+v", st)
	}
	if !st.StartedAt.Equal(claimedAt) {
		t.Fatalf("expected started_at to be the claim time, got %v", st.StartedAt)
	}
	if st.FinishedAt != nil {
		t.Fatalf("expected no finished_at, got %v", st.FinishedAt)
	}
}

func TestRequestStatusSetupFailureNoSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The worker attaches the attempt's session id before the workspace is
	// prepared, but a refused clone means no session row and no events ever
	// follow; FinishWorkRequest still records the terminal failure.
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-attempt"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	result := json.RawMessage(`{"request_id":"req-1","session_id":"sess-attempt","status":"failed",
		"error":{"code":"workspace_setup","message":"clone refused: branch does not exist"},
		"started_at":"2026-01-01T00:00:00Z","finished_at":"2026-01-01T00:01:00Z"}`)
	matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-attempt", "failed", result, time.Now().UTC())
	if err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	st, err := s.RequestStatus(ctx, "req-1")
	if err != nil {
		t.Fatalf("request status: %v", err)
	}
	if st.Status != "failed" {
		t.Fatalf("expected status failed, got %q", st.Status)
	}
	if st.ErrorCode != "workspace_setup" {
		t.Fatalf("expected error code workspace_setup, got %q", st.ErrorCode)
	}
	if !strings.Contains(st.ErrorMessage, "clone refused") {
		t.Fatalf("expected the clone error in the message, got %q", st.ErrorMessage)
	}
	if st.SubTurn != 0 || len(st.Todos) != 0 || len(st.ToolCalls) != 0 || st.Usage != nil {
		t.Fatalf("expected no event-derived state without a session, got %+v", st)
	}
}

func TestRequestStatusMidFlight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	mustCreateSession(t, s, "sess-1")

	// A previous sub-turn whose tool call was resolved, then the current
	// sub-turn: a TaskCreate plan and a TaskUpdate patch on one task, and a
	// Bash call still awaiting its result. Delta events sit between everything
	// to pin that the query ignores them (they are filtered by kind in SQL,
	// the way SessionUsageSummaries does — reasoning and content deltas are
	// the large payloads this polled query must never drag out of SQLite).
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-done", Name: "Read", Arguments: `{"file_path":"a.txt"}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-done", Name: "Read", Content: "contents"}},
		{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: strings.Repeat("x", 1<<20)}},
		{Kind: KindReasoningDelta, Payload: ReasoningDeltaPayload{Text: strings.Repeat("r", 1<<20)}},
		{Kind: KindTurnFinished, Payload: TurnFinishedPayload{SubTurn: 1, FinishReason: "tool_calls"}},
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 2}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-create", Name: "TaskCreate", Arguments: `{"tasks":[
			{"subject":"read the task","description":"read it","status":"completed","activeForm":"Reading the task"},
			{"subject":"fix the bug","description":"fix it","activeForm":"Fixing the bug"},
			{"subject":"push","description":"push it","activeForm":"Pushing"}]}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-create", Name: "TaskCreate", Content: "[ ] #1 read the task\n[ ] #2 fix the bug\n[ ] #3 push\n"}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-update", Name: "TaskUpdate", Arguments: `{"taskId":"2","status":"in_progress"}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-update", Name: "TaskUpdate", Content: "[~] #2 fix the bug"}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-bash", Name: "Bash", Arguments: `{"command":"go test ./..."}`}},
		{Kind: KindUsage, Payload: UsagePayload{SubTurn: 2, PromptCacheHitTokens: 100, PromptCacheMissTokens: 20, CompletionTokens: 30, ReasoningTokens: 5, CostUSD: 0.0012}},
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	st, err := s.RequestStatus(ctx, "req-1")
	if err != nil {
		t.Fatalf("request status: %v", err)
	}
	if st.SessionID != "sess-1" || st.Status != WorkRequestStatusRunning {
		t.Fatalf("unexpected identity or status: %+v", st)
	}
	if st.SubTurn != 2 {
		t.Fatalf("expected sub-turn 2 (the last turn_started), got %d", st.SubTurn)
	}
	if len(st.Todos) != 3 {
		t.Fatalf("expected the 3 tasks created by TaskCreate, got %+v", st.Todos)
	}
	if st.Todos[0].TaskID != "1" || st.Todos[1].TaskID != "2" || st.Todos[2].TaskID != "3" {
		t.Fatalf("expected ids minted in event order, got %+v", st.Todos)
	}
	if st.Todos[1].Status != "in_progress" || st.ActiveForm != "Fixing the bug" {
		t.Fatalf("expected the TaskUpdate-patched in-progress task's activeForm, got todos=%+v activeForm=%q", st.Todos, st.ActiveForm)
	}
	if len(st.ToolCalls) != 1 || st.ToolCalls[0].ID != "call-bash" || st.ToolCalls[0].Name != "Bash" {
		t.Fatalf("expected only the unresolved Bash call in flight, got %+v", st.ToolCalls)
	}
	if st.Usage == nil || st.Usage.SubTurn != 2 || st.Usage.CostUSD != 0.0012 || st.Usage.PromptCacheHitTokens != 100 {
		t.Fatalf("expected the latest usage event, got %+v", st.Usage)
	}
}

func TestRequestStatusFinishedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	mustCreateSession(t, s, "sess-1")
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-1", Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"do it","description":"do the thing","status":"completed","activeForm":"Doing it"}]}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-1", Name: "TaskCreate", Content: "[x] #1 do it"}},
		{Kind: KindUsage, Payload: UsagePayload{SubTurn: 1, PromptCacheMissTokens: 500, CompletionTokens: 40, CostUSD: 0.01}},
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}
	finished := time.Now().UTC()
	result := json.RawMessage(`{"request_id":"req-1","session_id":"sess-1","status":"ok","text":"all done","sub_turns":1}`)
	matched, err := s.FinishWorkRequest(ctx, "req-1", "sess-1", "ok", result, finished)
	if err != nil || !matched {
		t.Fatalf("finish: matched=%v err=%v", matched, err)
	}

	st, err := s.RequestStatus(ctx, "req-1")
	if err != nil {
		t.Fatalf("request status: %v", err)
	}
	if st.Status != "ok" {
		t.Fatalf("expected status ok, got %q", st.Status)
	}
	if st.FinishedAt == nil || !st.FinishedAt.Equal(finished) {
		t.Fatalf("expected the stored finished_at, got %v", st.FinishedAt)
	}
	if st.ErrorCode != "" || st.ErrorMessage != "" {
		t.Fatalf("expected no error for an ok result, got %q/%q", st.ErrorCode, st.ErrorMessage)
	}
	// The last turn's state is still reported for a finished run.
	if st.SubTurn != 1 || len(st.Todos) != 1 || len(st.ToolCalls) != 0 {
		t.Fatalf("unexpected terminal-state snapshot: %+v", st)
	}
}

func TestRequestStatusUnknownRequest(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RequestStatus(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestApplyTaskEventReplaysCreateUpdateDeleteInOrder pins applyTaskEvent
// against the live handlers' behaviour: TaskCreate mints ids from nextID in
// event order (defaulting an omitted status to pending), TaskUpdate patches
// only the fields present or removes the task with status "deleted", and
// anything the live handler would have rejected — malformed JSON, invalid
// status, empty subject or description, "deleted" combined with a patch,
// nothing to set, an unknown id — is skipped with the list unchanged and no
// id burned.
func TestApplyTaskEventReplaysCreateUpdateDeleteInOrder(t *testing.T) {
	nextID := 0
	var todos []StatusTodo

	todos = applyTaskEvent(todos, &nextID, "TaskCreate", `{"tasks":[
		{"subject":"read the task","description":"read it","activeForm":"Reading the task"},
		{"subject":"fix the bug","description":"fix it","activeForm":"Fixing the bug"}]}`)
	if len(todos) != 2 || todos[0].TaskID != "1" || todos[1].TaskID != "2" || todos[0].Status != "pending" {
		t.Fatalf("unexpected create result: %+v", todos)
	}
	if todos[0].Subject != "read the task" || todos[0].Description != "read it" {
		t.Fatalf("expected subject and description carried through replay, got %+v", todos[0])
	}
	if nextID != 2 {
		t.Fatalf("expected nextID 2 after two creates, got %d", nextID)
	}

	// An event the live handler would have rejected leaves the plan untouched
	// and must not burn an id.
	rejected := []struct{ name, args string }{
		{"TaskCreate", `{"tasks":[{"subject":"bad","description":"d","status":"bogus","activeForm":"Badding"}]}`},
		{"TaskCreate", `{"tasks":[{"subject":"","description":"d","activeForm":"Eing"}]}`},
		{"TaskCreate", `{"tasks":[{"subject":"fine","description":"","activeForm":"Eing"}]}`},
		{"TaskCreate", `{"tasks":[]}`},
		{"TaskCreate", `not json`},
		{"TaskUpdate", `not json`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","subject":"renamed"}`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","description":"changed"}`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","activeForm":"Renaming"}`},
		{"TaskUpdate", `{"taskId":"2"}`},
		{"TaskUpdate", `{"taskId":"99","status":"deleted"}`},
		{"TaskUpdate", `{"taskId":"2","status":"bogus"}`},
		{"TaskList", `{}`},
		{"Bash", `{"command":"ls"}`},
	}
	for _, c := range rejected {
		before := len(todos)
		todos = applyTaskEvent(todos, &nextID, c.name, c.args)
		if len(todos) != before {
			t.Fatalf("%s %s must be skipped, got %+v", c.name, c.args, todos)
		}
	}
	if nextID != 2 {
		t.Fatalf("rejected events must not burn ids, nextID = %d", nextID)
	}

	todos = applyTaskEvent(todos, &nextID, "TaskUpdate", `{"taskId":"2","status":"in_progress","subject":"fix the bug","description":"fix it better"}`)
	if len(todos) != 2 || todos[1].Status != "in_progress" || todos[1].Subject != "fix the bug" || todos[1].Description != "fix it better" || todos[1].ActiveForm != "Fixing the bug" {
		t.Fatalf("unexpected patch result: %+v", todos)
	}

	// "deleted" removes the task, preserving the order of the rest and
	// leaving the id counter alone.
	todos = applyTaskEvent(todos, &nextID, "TaskUpdate", `{"taskId":"1","status":"deleted"}`)
	if len(todos) != 1 || todos[0].TaskID != "2" {
		t.Fatalf("unexpected delete result: %+v", todos)
	}

	// A create after a delete mints the next id in sequence.
	todos = applyTaskEvent(todos, &nextID, "TaskCreate", `{"tasks":[{"subject":"push","description":"push it","activeForm":"Pushing"}]}`)
	if len(todos) != 2 || todos[1].TaskID != "3" {
		t.Fatalf("unexpected append after delete: %+v", todos)
	}
	if nextID != 3 {
		t.Fatalf("expected nextID 3 after the third create, got %d", nextID)
	}
}
