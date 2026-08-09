package store

import (
	"context"
	"testing"
	"time"
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

func TestRequestIDsForSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")
	mustCreateSession(t, s, "sess-2")

	if _, err := s.ClaimWorkRequest(ctx, "req-1", 1, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-1", "sess-1"); err != nil {
		t.Fatalf("set session: %v", err)
	}

	ids, err := s.RequestIDsForSessions(ctx, []string{"sess-1", "sess-2"})
	if err != nil {
		t.Fatalf("request ids for sessions: %v", err)
	}
	if ids["sess-1"] != "req-1" {
		t.Fatalf("expected sess-1 to map to req-1, got %+v", ids)
	}
	if _, ok := ids["sess-2"]; ok {
		t.Fatal("expected sess-2, created outside any work request, to have no entry")
	}
}
