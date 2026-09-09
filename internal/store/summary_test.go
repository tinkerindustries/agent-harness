package store

import (
	"context"
	"testing"
)

func TestGetEventsAfterRanges(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	inputs := make([]EventInput, 5)
	for i := range inputs {
		inputs[i] = EventInput{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: "x"}}
	}
	if _, err := s.AppendEvents(ctx, "sess-1", inputs); err != nil {
		t.Fatalf("append events: %v", err)
	}
	// seq now runs 1..5.

	all, err := s.GetEventsAfter(ctx, "sess-1", 0, -1)
	if err != nil {
		t.Fatalf("get events after 0: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 events from the start, got %d", len(all))
	}
	if all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("unexpected seq range: first=%d last=%d", all[0].Seq, all[4].Seq)
	}

	after3, err := s.GetEventsAfter(ctx, "sess-1", 3, -1)
	if err != nil {
		t.Fatalf("get events after 3: %v", err)
	}
	if len(after3) != 2 || after3[0].Seq != 4 || after3[1].Seq != 5 {
		t.Fatalf("expected seq 4 and 5, got %+v", after3)
	}

	afterAll, err := s.GetEventsAfter(ctx, "sess-1", 5, -1)
	if err != nil {
		t.Fatalf("get events after 5: %v", err)
	}
	if len(afterAll) != 0 {
		t.Fatalf("expected no events past the end, got %d", len(afterAll))
	}

	limited, err := s.GetEventsAfter(ctx, "sess-1", 0, 2)
	if err != nil {
		t.Fatalf("get events limited: %v", err)
	}
	if len(limited) != 2 || limited[0].Seq != 1 || limited[1].Seq != 2 {
		t.Fatalf("expected first 2 events, got %+v", limited)
	}
}

func TestGetEventsAfterUnknownSession(t *testing.T) {
	s := openTestStore(t)
	events, err := s.GetEventsAfter(context.Background(), "does-not-exist", 0, -1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events, got %d", len(events))
	}
}

// TestGetEventsAfterKindsFiltersInSQL pins the ?kind= filter's read: only
// matching kinds come back, in seq order, restricted to the seq and limit
// bounds exactly like the unfiltered read — the filter is a WHERE clause,
// not a post-query discard, so paging a filter never pulls the events it
// will throw away.
func TestGetEventsAfterKindsFiltersInSQL(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "c1", Name: "Read", Arguments: `{}`}},
		{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: "noise"}},
		{Kind: KindToolCall, Payload: ToolCallPayload{ID: "c2", Name: "Bash", Arguments: `{}`}},
		{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "c2", Name: "Bash", Content: "ok"}},
		{Kind: KindUsage, Payload: UsagePayload{SubTurn: 1, CostUSD: 0.001}},
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}
	// seqs: 1 turn_started, 2 tool_call, 3 content_delta, 4 tool_call,
	//       5 tool_result, 6 usage.

	got, err := s.GetEventsAfterKinds(ctx, "sess-1", 0, -1, []EventKind{KindToolCall, KindToolResult})
	if err != nil {
		t.Fatalf("get filtered events: %v", err)
	}
	var seqs []int64
	for _, e := range got {
		if e.Kind != KindToolCall && e.Kind != KindToolResult {
			t.Fatalf("filter returned a %s event", e.Kind)
		}
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 2 || seqs[1] != 4 || seqs[2] != 5 {
		t.Fatalf("filtered seqs = %v, want 2, 4, 5", seqs)
	}

	// The seq bound and the limit apply to the filtered set, not the raw
	// log: "from" 3 skips the tool_call at seq 2, and the limit 1 stops at
	// the next match.
	got, err = s.GetEventsAfterKinds(ctx, "sess-1", 2, 1, []EventKind{KindToolCall})
	if err != nil {
		t.Fatalf("get filtered paged events: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 4 || got[0].Kind != KindToolCall {
		t.Fatalf("filtered paged read = %+v, want just the tool_call at seq 4", got)
	}

	// An empty kind list is no filter — the unfiltered read's shape.
	got, err = s.GetEventsAfterKinds(ctx, "sess-1", 0, -1, nil)
	if err != nil {
		t.Fatalf("get unfiltered events: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("expected all 6 events with no kinds, got %d", len(got))
	}
}

func TestSessionUsageSummaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")
	mustCreateSession(t, s, "sess-2")

	// sess-1: two sub-turns, two usage events.
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindUsage, Payload: UsagePayload{PromptCacheHitTokens: 100, PromptCacheMissTokens: 10, CompletionTokens: 5, ReasoningTokens: 3, CostUSD: 0.001}},
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 2}},
		{Kind: KindUsage, Payload: UsagePayload{PromptCacheHitTokens: 200, PromptCacheMissTokens: 20, CompletionTokens: 7, ReasoningTokens: 4, CostUSD: 0.002}},
	}); err != nil {
		t.Fatalf("append sess-1 events: %v", err)
	}
	// sess-2: no sub-turns yet.

	summaries, err := s.SessionUsageSummaries(ctx, []string{"sess-1", "sess-2", "does-not-exist"})
	if err != nil {
		t.Fatalf("session usage summaries: %v", err)
	}

	got1 := summaries["sess-1"]
	if got1.SubTurns != 2 {
		t.Fatalf("expected 2 sub-turns, got %d", got1.SubTurns)
	}
	if got1.PromptCacheHitTokens != 300 || got1.PromptCacheMissTokens != 30 || got1.CompletionTokens != 12 || got1.ReasoningTokens != 7 {
		t.Fatalf("unexpected accumulated usage: %+v", got1)
	}
	if got1.CostUSD < 0.0029999 || got1.CostUSD > 0.0030001 {
		t.Fatalf("unexpected accumulated cost: %v", got1.CostUSD)
	}

	if got2, ok := summaries["sess-2"]; ok && (got2.SubTurns != 0 || got2.CompletionTokens != 0) {
		t.Fatalf("expected sess-2 to be empty, got %+v", got2)
	}
	if _, ok := summaries["does-not-exist"]; ok {
		t.Fatal("expected no entry for a session with no events")
	}
}
