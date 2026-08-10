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
	// sub-turn: a TodoWrite plan and a Bash call still awaiting its result.
	// Delta events sit between everything to pin that the query ignores
	// them (they are filtered by kind in SQL, the way SessionUsageSummaries
	// does — reasoning and content deltas are the large payloads this
	// polled query must never drag out of SQLite).
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-done", Name: "Read", Arguments: `{"file_path":"a.txt"}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-done", Name: "Read", Content: "contents"}},
		{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: strings.Repeat("x", 1<<20)}},
		{Kind: KindReasoningDelta, Payload: ReasoningDeltaPayload{Text: strings.Repeat("r", 1<<20)}},
		{Kind: KindTurnFinished, Payload: TurnFinishedPayload{SubTurn: 1, FinishReason: "tool_calls"}},
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 2}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-todo", Name: "TodoWrite", Arguments: `{"todos":[
			{"content":"read the task","status":"completed","activeForm":"Reading the task"},
			{"content":"fix the bug","status":"in_progress","activeForm":"Fixing the bug"},
			{"content":"push","status":"pending","activeForm":"Pushing"}]}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-todo", Name: "TodoWrite", Content: "[~] fix the bug"}},
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
		t.Fatalf("expected the 3 todos from the last TodoWrite, got %+v", st.Todos)
	}
	if st.Todos[1].Status != "in_progress" || st.ActiveForm != "Fixing the bug" {
		t.Fatalf("expected the in-progress todo's activeForm, got todos=%+v activeForm=%q", st.Todos, st.ActiveForm)
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
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "call-1", Name: "TodoWrite", Arguments: `{"todos":[{"content":"do it","status":"completed","activeForm":"Doing it"}]}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call-1", Name: "TodoWrite", Content: "[x] do it"}},
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
